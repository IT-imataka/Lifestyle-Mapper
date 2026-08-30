package plan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/llm"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
	"github.com/taka/lifestyle-mapper/backend/internal/prompt"
)

// LLM を一切呼ばずに、検証を通るプランが最後まで組めることを確かめる。
// 「LLM が落ちてもサービスが死なない」という設計の担保はこのテスト。
func TestRuleBasedComposeBuildsValidPlan(t *testing.T) {
	f := newFixture(t)

	h, err := NewRuleBasedComposer(f.store, nil).Compose(f.cond)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	kinds := []model.SegmentType{}
	for _, s := range h.Steps {
		kinds = append(kinds, s.Kind)
	}
	want := []model.SegmentType{model.SegmentMove, model.SegmentDining, model.SegmentMove, model.SegmentLodging}
	if len(kinds) != len(want) {
		t.Fatalf("ステップが %v です（%v を期待）", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("ステップが %v です（%v を期待）", kinds, want)
		}
	}

	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	// 退場待機は timecalc が補う。
	assertSpan(t, tl[0], "seg_1", model.SegmentBuffer, jst(t, "2026-09-12 21:00"), jst(t, "2026-09-12 21:30"))
	assertSpan(t, tl[2], "seg_3", model.SegmentDining, jst(t, "2026-09-12 21:38"), jst(t, "2026-09-12 23:08"))
	assertSpan(t, tl[4], "seg_5", model.SegmentLodging, jst(t, "2026-09-12 23:14"), jst(t, "2026-09-13 10:00"))

	if v := NewValidator(f.store).Validate(tl, f.cond); !v.OK() {
		t.Fatalf("ルールベースのプランが検証に落ちました: %v", v.Violations)
	}
}

// 文章は LLM 由来の層。ルールベースは埋めない。
func TestRuleBasedProducesNoNarrative(t *testing.T) {
	f := newFixture(t)
	h, err := NewRuleBasedComposer(f.store, nil).Compose(f.cond)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if h.Narrative != nil {
		t.Errorf("プラン全体の文章が付いています: %+v", h.Narrative)
	}
	for _, s := range h.Steps {
		if s.Narrative != nil {
			t.Errorf("%s に文章が付いています: %+v", s.Kind, s.Narrative)
		}
	}
}

// 事実（リンク・残室警告）は LLM 経路と同じ実装を通る。
func TestRuleBasedKeepsFactsAndWarnings(t *testing.T) {
	f := newFixture(t)
	h, err := NewRuleBasedComposer(f.store, nil).Compose(f.cond)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if len(h.Steps[1].Links) == 0 {
		t.Error("飲食のリンクが組み立てられていません")
	}
	if len(h.Warnings) != 1 || h.Warnings[0].Code != model.WarnVacancyLow {
		t.Errorf("残室の警告がありません: %+v", h.Warnings)
	}
	// 採用した候補は差し替え候補に出さない。
	for _, a := range h.Alternatives.Dining {
		if a.Place.ID == f.izakay.ID {
			t.Error("採用済みの居酒屋が差し替え候補に混ざっています")
		}
	}
	if len(h.Alternatives.Dining) != 1 || h.Alternatives.Dining[0].Place.ID != f.ramen.ID {
		t.Errorf("差し替え候補が期待と異なります: %+v", h.Alternatives.Dining)
	}
}

// 飲食を切れば宿だけのプランになる。
func TestRuleBasedHonorsDisabledCategories(t *testing.T) {
	f := newFixture(t)
	f.cond.Dining.Enabled = false
	mustPutRoute(t, f.store, model.RouteOriginVenue, string(f.hotel.ID), model.TravelWalk, 900, 12*time.Minute)

	h, err := NewRuleBasedComposer(f.store, nil).Compose(f.cond)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if len(h.Steps) != 2 || h.Steps[1].Kind != model.SegmentLodging {
		t.Fatalf("宿だけのプランになっていません: %+v", h.Steps)
	}
}

// 経路の無い候補は飛ばして次を採る。経路の推定はしない。
func TestRuleBasedSkipsCandidateWithoutRoute(t *testing.T) {
	f := newFixture(t)
	f.cond.Lodging.Enabled = false
	// スコア上位の居酒屋への経路が無い状況を作り直す。
	store := NewFactStore(f.store.Venue())
	far := &model.PlaceFact{Cat: model.CategoryDining, ProviderPlaceID: "ChIJfar", Name: "遠い店",
		Loc: model.Location{Lat: 35.75, Lng: 139.80}}
	near := &model.PlaceFact{Cat: model.CategoryDining, ProviderPlaceID: "ChIJnear", Name: "近い店",
		Loc: model.Location{Lat: 35.70, Lng: 139.75}}
	if _, err := store.AddPlace(far); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlace(near); err != nil {
		t.Fatal(err)
	}
	mustPutRoute(t, store, model.RouteOriginVenue, string(near.ID), model.TravelWalk, 500, 7*time.Minute)

	h, err := NewRuleBasedComposer(store, nil).Compose(f.cond)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if h.Steps[1].Place.ID != near.ID {
		t.Errorf("採用されたのは %s です（経路のある %s を期待）", h.Steps[1].Place.ID, near.ID)
	}
}

// 満室の宿は採らない。
func TestRuleBasedSkipsSoldOutHotel(t *testing.T) {
	f := newFixture(t)
	for i := range f.hotel.Plans {
		f.hotel.Plans[i].VacancyStatus = model.VacancySoldOut
	}

	h, err := NewRuleBasedComposer(f.store, nil).Compose(f.cond)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	for _, s := range h.Steps {
		if s.Kind == model.SegmentLodging {
			t.Fatal("満室の宿が採用されました")
		}
	}
}

// 組むものが何も無いなら、待機だけのプランを返さず正直に失敗する。
func TestRuleBasedFailsWithoutUsableCandidates(t *testing.T) {
	f := newFixture(t)
	store := NewFactStore(f.store.Venue())

	_, err := NewRuleBasedComposer(store, nil).Compose(f.cond)
	if err == nil {
		t.Fatal("候補が無いのにプランが返りました")
	}
	if got := apperror.CodeOf(err); got != apperror.CodeNoCandidatesFound {
		t.Errorf("エラーコードが %s です（%s を期待）", got, apperror.CodeNoCandidatesFound)
	}
	// 候補が無いのは組み直しても直らない。
	if apperror.From(err).Repairable() {
		t.Error("候補不足が repairable になっています")
	}
}

// ── LLM 経路 ──────────────────────────────────────────

// fakeLLM は提供元の代わり。プロンプトを覗いて、渡した材料を検証する。
type fakeLLM struct {
	got   llm.Request
	resp  *llm.Response
	err   error
	calls int
}

func (f *fakeLLM) ComposePlan(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.calls++
	f.got = req
	return f.resp, f.err
}

func (f *fakeLLM) Provider() model.LLMProvider { return model.LLMAnthropic }

func newFakeLLM(f *fixture) *fakeLLM {
	return &fakeLLM{resp: &llm.Response{
		Draft: f.draft(),
		Meta:  llm.NewMeta(model.LLMAnthropic, "claude-opus-5", 4820*time.Millisecond, 3204, 1180),
	}}
}

func newTestComposer(f *fixture, client llm.Composer) *Composer {
	return NewComposer(f.store, client, prompt.MustNew(), ComposeOptions{MaxTokens: 8000, Effort: "medium"})
}

func TestComposeSendsCandidatesAndFillsPromptVersion(t *testing.T) {
	f := newFixture(t)
	client := newFakeLLM(f)

	resp, err := newTestComposer(f, client).Compose(context.Background(), f.cond, nil)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if resp.Meta.PromptVersion != prompt.Version {
		t.Errorf("promptVersion が %q です（%q を期待）", resp.Meta.PromptVersion, prompt.Version)
	}
	if client.got.System == "" {
		t.Error("役割指示が渡っていません")
	}
	if client.got.MaxTokens != 8000 || client.got.Effort != "medium" {
		t.Errorf("設定が渡っていません: %+v", client.got)
	}

	// 候補は candidateId で渡り、経路は実測値が添えられる。
	for _, want := range []string{
		string(f.izakay.ID), string(f.ramen.ID), string(f.hotel.ID),
		"会場から徒歩8分（650m）", // 会場 → 居酒屋の実測
	} {
		if !strings.Contains(client.got.Prompt, want) {
			t.Errorf("プロンプトに %q が含まれていません", want)
		}
	}
	// 経路が無い候補は距離を推定せず「未取得」と伝える。
	if !strings.Contains(client.got.Prompt, "会場からの経路は未取得") {
		t.Error("経路未取得の候補の扱いがプロンプトに出ていません")
	}
}

// 満室の宿は候補に載せない。選ばせてから弾くのは往復の無駄。
func TestComposeOmitsSoldOutHotels(t *testing.T) {
	f := newFixture(t)
	for i := range f.hotel.Plans {
		f.hotel.Plans[i].VacancyStatus = model.VacancySoldOut
	}
	client := newFakeLLM(f)

	if _, err := newTestComposer(f, client).Compose(context.Background(), f.cond, nil); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if strings.Contains(client.got.Prompt, string(f.hotel.ID)) {
		t.Error("満室の宿が候補として渡っています")
	}
}

// 無効にしたカテゴリの候補は渡さない（トークンを払わない）。
func TestComposeOmitsDisabledCategories(t *testing.T) {
	f := newFixture(t)
	f.cond.Dining.Enabled = false
	client := newFakeLLM(f)

	if _, err := newTestComposer(f, client).Compose(context.Background(), f.cond, nil); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if strings.Contains(client.got.Prompt, string(f.izakay.ID)) {
		t.Error("無効にした飲食の候補が渡っています")
	}
	if !strings.Contains(client.got.Prompt, string(f.hotel.ID)) {
		t.Error("宿泊の候補が渡っていません")
	}
}

// 再生成では前回の不合格理由を伝える。
func TestComposePassesViolationsOnRepair(t *testing.T) {
	f := newFixture(t)
	client := newFakeLLM(f)
	violations := []string{"order=3: dining には candidateId が必要です"}

	if _, err := newTestComposer(f, client).Compose(context.Background(), f.cond, violations); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if !strings.Contains(client.got.Prompt, "前回の出力の不備") ||
		!strings.Contains(client.got.Prompt, violations[0]) {
		t.Error("前回の不合格理由がプロンプトに載っていません")
	}
	if len(client.got.Violations) != 1 {
		t.Error("Request.Violations が渡っていません")
	}
}

// 提供元のエラーはコードを保ったまま素通しする。上位がフォールバックを判断できるように。
func TestComposePropagatesProviderError(t *testing.T) {
	f := newFixture(t)
	client := &fakeLLM{err: llm.Unavailable(model.LLMAnthropic, errors.New("timeout"))}

	_, err := newTestComposer(f, client).Compose(context.Background(), f.cond, nil)
	if got := apperror.CodeOf(err); got != apperror.CodeLLMUnavailable {
		t.Errorf("エラーコードが %s です（%s を期待）", got, apperror.CodeLLMUnavailable)
	}
}

func TestComposeRejectsEmptyDraft(t *testing.T) {
	f := newFixture(t)
	client := &fakeLLM{resp: &llm.Response{}}

	_, err := newTestComposer(f, client).Compose(context.Background(), f.cond, nil)
	if got := apperror.CodeOf(err); got != apperror.CodeLLMInvalidOutput {
		t.Errorf("エラーコードが %s です（%s を期待）", got, apperror.CodeLLMInvalidOutput)
	}
}

// LLM が構成されていない環境では、その旨を返してルールベースに落とさせる。
func TestComposeWithoutClientIsUnavailable(t *testing.T) {
	f := newFixture(t)

	_, err := NewComposer(f.store, nil, prompt.MustNew(), ComposeOptions{}).
		Compose(context.Background(), f.cond, nil)
	if got := apperror.CodeOf(err); got != apperror.CodeLLMUnavailable {
		t.Errorf("エラーコードが %s です（%s を期待）", got, apperror.CodeLLMUnavailable)
	}
}
