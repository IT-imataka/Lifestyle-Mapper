package plan

import (
	"context"
	"errors"
	"fmt"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/llm"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
	"github.com/taka/lifestyle-mapper/backend/internal/prompt"
)

// RuleBasedComposer は **LLM を一切使わずに**時系列を組む。
//
// 設計書のフォールバック 3 段構え（Claude → Gemini → ルールベース）の最終段であり、
// options.llmNarrative が false のときの本線でもある。
//
// 文章は付けない。narrative は LLM 由来の層と決めているので、機械的なラベルで
// 埋めて「LLM が書いたように見える文字列」を混ぜない。UI は narrative が nil でも
// セグメント種別だけで描画できる設計になっている。
//
// 候補の選択は「スコア順の先頭から、経路が引けたものを採る」だけ。
// 良し悪しの判断は scorer が済ませている前提で、ここは判断をしない。
type RuleBasedComposer struct {
	store *FactStore
	links LinkBuilder
}

func NewRuleBasedComposer(store *FactStore, links LinkBuilder) *RuleBasedComposer {
	if links == nil {
		links = DefaultLinkBuilder{}
	}
	return &RuleBasedComposer{store: store, links: links}
}

// Compose は Hydrated を直接組み立てる。
//
// LLM 経路（Hydrator）と同じ型を返すので、以降の timecalc → validator は
// 経路を意識しない。**LLM が落ちた日も止まらない**ことがこの層の存在意義。
func (c *RuleBasedComposer) Compose(cond *model.SearchCondition) (*Hydrated, error) {
	if cond == nil {
		return nil, apperror.Internal(fmt.Errorf("検索条件が nil です"), "composer.Compose")
	}
	// 経路の解決とリンク生成は Hydrator と同じ実装を使う。
	// 事実の扱いが 2 通りあると、片方だけ直す事故が起きる。
	h := NewHydrator(c.store, c.links)

	out := &Hydrated{Steps: make([]ResolvedStep, 0, 4)}
	here := locationRef{key: model.RouteOriginVenue, name: c.store.Venue().Name}
	used := make(map[model.CandidateID]bool, 2)

	// 終演直後の待機は timecalc が exitBufferMinutes から必ず挿入するため、ここでは置かない。

	if cond.Dining.Enabled {
		if place, route, ok := c.pickPlace(h, here, cond); ok {
			out.Steps = append(out.Steps,
				ResolvedStep{Kind: model.SegmentMove, Route: route},
				ResolvedStep{
					Kind:  model.SegmentDining,
					Stay:  cond.Dining.DesiredStay,
					Place: place,
					Links: c.links.DiningLinks(place),
				})
			here = locationRef{key: string(place.ID), name: place.Name}
			used[place.ID] = true
		}
	}

	if cond.Lodging.Enabled {
		if choice, route, ok := c.pickHotel(h, here, cond); ok {
			out.Steps = append(out.Steps,
				ResolvedStep{Kind: model.SegmentMove, Route: route},
				ResolvedStep{
					Kind:    model.SegmentLodging,
					Lodging: &choice,
					Links:   c.links.LodgingLinks(choice.Hotel, choice.Plan),
				})
			used[choice.Hotel.ID] = true
			if w, ok := vacancyWarning(choice.Hotel, choice.Plan); ok {
				out.Warnings = append(out.Warnings, w)
			}
		}
	}

	if len(out.Steps) == 0 {
		// 待機だけのプランは提示する価値がない。候補か経路のどちらかが足りていない。
		return nil, apperror.Wrap(
			fmt.Errorf("経路の引ける候補がありません（候補 %d 件・経路 %d 件）",
				c.store.Len(), c.store.RouteCount()),
			apperror.CodeNoCandidatesFound, "composer")
	}

	out.Alternatives = h.alternativesExcept(used, nil)
	return out, nil
}

// pickPlace はスコア順の先頭から、現在地から経路が引ける飲食店を採る。
func (c *RuleBasedComposer) pickPlace(h *Hydrator, from locationRef, cond *model.SearchCondition) (*model.PlaceFact, *model.RouteFact, bool) {
	for _, p := range c.store.Places(model.CategoryDining) {
		// 到着時刻が確定していない段階なので営業時間は見ない。判定は validator が行う。
		route, err := h.resolveRoute(from, locationRef{key: string(p.ID), name: p.Name}, cond)
		if err != nil {
			continue
		}
		return p, route, true
	}
	return nil, nil, false
}

// pickHotel はスコア順の先頭から、空きがあり経路が引ける宿を採る。
func (c *RuleBasedComposer) pickHotel(h *Hydrator, from locationRef, cond *model.SearchCondition) (model.LodgingChoice, *model.RouteFact, bool) {
	for _, hotel := range c.store.Hotels() {
		roomPlan := hotel.CheapestPlan()
		if roomPlan == nil {
			continue // 満室
		}
		route, err := h.resolveRoute(from, locationRef{key: string(hotel.ID), name: hotel.Name}, cond)
		if err != nil {
			continue
		}
		return model.LodgingChoice{Hotel: hotel, Plan: roomPlan}, route, true
	}
	return model.LodgingChoice{}, nil, false
}

// ── LLM 経路 ──────────────────────────────────────────

// PromptBuilder は composer が要求するプロンプト組み立ての契約。
// **interface を利用側（ここ）に置く**ことで、prompt パッケージは service を知らずに済む。
type PromptBuilder interface {
	Build(prompt.Input) (string, error)
	Version() string
}

// ComposeOptions は 1 回の生成に渡す設定。config から注入する。
type ComposeOptions struct {
	MaxTokens int
	// Effort は Anthropic の構造化出力の思考量。他提供元では無視される。
	Effort string
	// Schema が空なら llm パッケージの埋め込みスキーマを使う。
	Schema []byte
}

// Composer は候補と検索条件から LLM に下書きを書かせる（設計書の ④）。
//
// ここが持つのは「何を渡して何を受け取るか」だけで、
// プロンプトの文面は prompt パッケージ、提供元ごとの作法は infrastructure/llm にある。
// 事実の照合は一切しない（それは Hydrator の仕事）。
type Composer struct {
	store   *FactStore
	client  llm.Composer
	builder PromptBuilder
	opt     ComposeOptions
}

func NewComposer(store *FactStore, client llm.Composer, builder PromptBuilder, opt ComposeOptions) *Composer {
	return &Composer{store: store, client: client, builder: builder, opt: opt}
}

// Compose は下書きを 1 本作る。
//
// violations が空でなければ再生成（repair）で、前回の不合格理由がプロンプトに載る。
// 返すエラーは LLM_UNAVAILABLE（ルールベースへ落とす合図）か
// LLM_INVALID_OUTPUT（組み直しの合図）のいずれか。
func (c *Composer) Compose(ctx context.Context, cond *model.SearchCondition, violations []string) (*llm.Response, error) {
	if c.client == nil || c.builder == nil {
		return nil, apperror.New(apperror.CodeLLMUnavailable, "").
			WithOp("composer.Compose").
			WithCause(errors.New("LLM が構成されていません"))
	}
	if cond == nil {
		return nil, apperror.Internal(errors.New("検索条件が nil です"), "composer.Compose")
	}

	text, err := c.builder.Build(c.promptInput(cond, violations))
	if err != nil {
		return nil, apperror.Internal(err, "composer.Compose")
	}

	resp, err := c.client.ComposePlan(ctx, llm.Request{
		Prompt:     text,
		System:     prompt.System(),
		Schema:     c.opt.Schema,
		MaxTokens:  c.opt.MaxTokens,
		Effort:     c.opt.Effort,
		Violations: violations,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Draft == nil {
		return nil, llm.InvalidOutput(c.client.Provider(), errors.New("下書きが空です"))
	}

	// 提供元は自分が何版のプロンプトで呼ばれたかを知らない。ここで埋める。
	resp.Meta.PromptVersion = c.builder.Version()
	return resp, nil
}

// promptInput は FactStore の候補をプロンプトの材料に移す。
//
// 経路を添えるのは、LLM に距離を推定させないため。徒歩何分かは実測値でしか語らせない。
func (c *Composer) promptInput(cond *model.SearchCondition, violations []string) prompt.Input {
	in := prompt.Input{Condition: cond, Violations: violations}

	if cond.Dining.Enabled {
		for _, p := range c.store.Places(model.CategoryDining) {
			in.Dining = append(in.Dining, prompt.DiningCandidate{
				Place:     p,
				FromVenue: c.routeFromVenue(p.ID, cond),
			})
		}
	}
	if cond.Lodging.Enabled {
		for _, h := range c.store.Hotels() {
			roomPlan := h.CheapestPlan()
			if roomPlan == nil {
				continue // 満室の宿は候補として見せない。選ばせてから弾くのは無駄な往復。
			}
			in.Lodging = append(in.Lodging, prompt.LodgingCandidate{
				Hotel:     h,
				Plan:      roomPlan,
				FromVenue: c.routeFromVenue(h.ID, cond),
			})
		}
	}
	return in
}

// routeFromVenue は会場からの実測経路を引く。無ければ nil（推定はしない）。
func (c *Composer) routeFromVenue(id model.CandidateID, cond *model.SearchCondition) *model.RouteFact {
	var candidates []*model.RouteFact
	for _, mode := range cond.Options.TransportModes {
		if r, ok := c.store.RouteFromVenue(id, mode); ok {
			candidates = append(candidates, r)
		}
	}
	// Hydrator と同じ規則で選ぶ。見せた経路と組む経路を食い違わせない。
	return pickRoute(candidates, cond.Situation.MaxWalk)
}
