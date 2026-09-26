package plan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/llm"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// Service はプラン生成の司令塔。設計書のパイプライン ①〜⑦ をこの順で進める。
//
//	Collector → FactStore → Composer(LLM) → Hydrator → timecalc → Validator → Plan
//
// この層が持つのは**順序と失敗時の落とし方**だけで、判断は各段に委ねる。
// 落とし方は 3 段構え:
//
//	LLM で組む → 不合格なら 1 回だけ組み直す → それでも駄目ならルールベースで組む
//
// 最後の段が残っている限りサービスは死なない。文章が消えても時系列は返る。
type Service struct {
	collector Collector
	repo      Repository
	llm       llm.Composer
	builder   PromptBuilder
	links     LinkBuilder
	opt       Options
	now       func() time.Time
}

// Collector は外部 API から事実を集める（設計書 ②③③'）。
// **interface は利用側のここに置く**。実装は infrastructure に閉じる。
type Collector interface {
	// Collect は候補と経路を詰めた FactStore を返す。
	// 一部のプロバイダが落ちても、残りで組めるなら error は返さず SourceStatus で伝える。
	Collect(ctx context.Context, cond *model.SearchCondition) (*FactStore, []model.SourceStatus, error)
}

// Repository は生成済みプランの保管。共有 URL からの再取得に使う。
type Repository interface {
	Save(ctx context.Context, p *model.Plan) error
	// Find は見つからなければ (nil, nil) を返す。「無い」はエラーではない。
	Find(ctx context.Context, id model.PlanID) (*model.Plan, error)
}

// Options は生成の設定。config から注入する。
type Options struct {
	// TTL は空室情報の鮮度保証。既定は model.PlanTTL。
	TTL time.Duration
	// BaseURL は shareUrl の組み立てに使うフロントのオリジン。
	BaseURL string
	// MaxRepairAttempts は Validator 不合格による再生成の上限。
	MaxRepairAttempts int
	// LLMEnabled が false なら composer を丸ごと飛ばす（LLM が炎上した日の非常口）。
	LLMEnabled bool
	Compose    ComposeOptions
}

// Deps は Service の依存。数が多いので位置引数にしない。
type Deps struct {
	Collector  Collector
	Repository Repository
	// LLM は nil 可。nil ならルールベースで組む。
	LLM    llm.Composer
	Prompt PromptBuilder
	Links  LinkBuilder
	Now    func() time.Time
}

func NewService(deps Deps, opt Options) *Service {
	if deps.Links == nil {
		deps.Links = DefaultLinkBuilder{}
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if opt.TTL <= 0 {
		opt.TTL = model.PlanTTL
	}
	if opt.MaxRepairAttempts < 0 {
		opt.MaxRepairAttempts = 0
	}
	return &Service{
		collector: deps.Collector,
		repo:      deps.Repository,
		llm:       deps.LLM,
		builder:   deps.Prompt,
		links:     deps.Links,
		opt:       opt,
		now:       deps.Now,
	}
}

// Result は 1 回の生成の成果。
// Plan は永続化され、Sources / LLM はレスポンスの meta になる。
// **部分失敗を隠さない**ため、成功時でも Sources は必ず返す。
type Result struct {
	Plan    *model.Plan
	Sources []model.SourceStatus
	LLM     model.LLMMeta
}

// errLLMSkipped は「LLM を呼ばなかった」ことを示す内部の合図。
// 呼んで失敗した場合と区別する（利用者が自分で切ったなら警告を出さない）。
var errLLMSkipped = errors.New("LLM をスキップしました")

// Generate はプランを 1 本生成する。
func (s *Service) Generate(ctx context.Context, cond *model.SearchCondition) (*Result, error) {
	if cond == nil {
		return nil, apperror.Internal(errors.New("検索条件が nil です"), "plan.Generate")
	}
	if s.collector == nil {
		return nil, apperror.Internal(errors.New("Collector が構成されていません"), "plan.Generate")
	}
	// controller でも検証済みだが、ワーカーから直接呼ばれる経路のためにここでも通す。
	cond.ApplyDefaults()
	if err := cond.Validate(s.now()); err != nil {
		return nil, err
	}

	// instrument: before collector
	store, sources, err := s.collector.Collect(ctx, cond)
	if err != nil {
		return nil, err
	}
	if store == nil || store.Len() == 0 {
		return nil, apperror.New(apperror.CodeNoCandidatesFound, "").WithOp("plan.Generate")
	}
	// 以降 FactStore は読むだけ。**真実の源を後から書き換えさせない**。
	store.Freeze()

	var warnings []model.Warning
	warnings = append(warnings, degradedWarnings(sources)...)

	// instrument: before composeWithLLM
	hyd, tl, verdict, llmMeta, err := s.composeWithLLM(ctx, cond, store)
	if err != nil {
		if !errors.Is(err, errLLMSkipped) {
			// 呼んで駄目だった場合だけ伝える。文章が無いことは機能の欠落ではない。
			warnings = append(warnings, model.NewWarning(model.WarnLLMUnavailable, model.SeverityInfo, "",
				"文章の生成ができなかったため、時系列のみを表示しています。"))
		}
		hyd, tl, verdict, err = s.composeByRule(cond, store)
		if err != nil {
			return nil, err
		}
		llmMeta = s.ruleBasedMeta()
	}

	plan := s.assemble(cond, hyd, tl, verdict, append(warnings, hyd.Warnings...), sources)
	if s.repo != nil {
		if err := s.repo.Save(ctx, plan); err != nil {
			// 保存できなくても手元のプランは返せる。共有 URL が死ぬだけ。
			return nil, apperror.Wrap(err, apperror.CodeInternal, "plan.Generate")
		}
	}
	return &Result{Plan: plan, Sources: sources, LLM: llmMeta}, nil
}

// Get は保存済みのプランを返す。
func (s *Service) Get(ctx context.Context, id model.PlanID) (*model.Plan, error) {
	if !id.Valid() {
		return nil, apperror.NotFound("plan.Get")
	}
	if s.repo == nil {
		return nil, apperror.Internal(errors.New("Repository が構成されていません"), "plan.Get")
	}
	p, err := s.repo.Find(ctx, id)
	if err != nil {
		return nil, apperror.Wrap(err, apperror.CodeInternal, "plan.Get")
	}
	if p == nil {
		return nil, apperror.NotFound("plan.Get")
	}
	// 空室は数分で変わる。期限切れは 410 で再検索を促すほうが、
	// 「クリックしたら満室だった」より痛くない。
	if p.Expired(s.now()) {
		return nil, apperror.Expired("plan.Get")
	}
	return p, nil
}

// composeWithLLM は LLM 経路を、組み直しも含めて試す。
//
// 返るエラーは呼び出し側でルールベースへ落とす合図になる。
// errLLMSkipped は「そもそも呼ばなかった」で、失敗ではない。
func (s *Service) composeWithLLM(ctx context.Context, cond *model.SearchCondition, store *FactStore) (
	*Hydrated, model.Timeline, Verdict, model.LLMMeta, error) {

	if !s.opt.LLMEnabled || !cond.Options.LLMNarrative || s.llm == nil || s.builder == nil {
		return nil, nil, Verdict{}, model.LLMMeta{}, errLLMSkipped
	}

	composer := NewComposer(store, s.llm, s.builder, s.opt.Compose)
	hydrator := NewHydrator(store, s.links)
	validator := NewValidator(store)

	var hints []string
	for attempt := 0; ; attempt++ {
		resp, err := composer.Compose(ctx, cond, hints)
		if err != nil {
			// 提供元に到達できない／出力が壊れている。組み直せるなら理由を添えて再挑戦。
			if !ShouldRepair(err, attempt, s.opt.MaxRepairAttempts) {
				return nil, nil, Verdict{}, model.LLMMeta{}, err
			}
			hints = []string{err.Error()}
			continue
		}
		meta := resp.Meta
		meta.RepairAttempts = attempt

		hyd, tl, verdict, err := s.buildFromDraft(resp, cond, hydrator, validator)
		if err == nil {
			return hyd, tl, verdict, meta, nil
		}
		if !ShouldRepair(err, attempt, s.opt.MaxRepairAttempts) {
			return nil, nil, Verdict{}, meta, err
		}
		// 次の生成には「どこが駄目だったか」を渡す。同じ失敗を繰り返させない。
		hints = repairHints(err, verdict)
	}
}

// buildFromDraft は下書きを事実で埋め、時刻を確定し、検証まで通す。
func (s *Service) buildFromDraft(resp *llm.Response, cond *model.SearchCondition,
	hydrator *Hydrator, validator *Validator) (*Hydrated, model.Timeline, Verdict, error) {

	hyd, err := hydrator.Hydrate(resp.Draft, cond)
	if err != nil {
		return nil, nil, Verdict{}, err
	}
	tl, err := BuildTimeline(hyd, cond)
	if err != nil {
		return nil, nil, Verdict{}, err
	}
	verdict := validator.Validate(tl, cond)
	if !verdict.OK() {
		return nil, nil, verdict, verdict.Err()
	}
	return hyd, tl, verdict, nil
}

// composeByRule は LLM 抜きで組む最終段。
//
// ここでも Validator は通すが、**不合格でも返す**。組み直す相手がいない以上、
// 「余裕がない」と警告付きで見せるほうが、何も出さないより役に立つ。
func (s *Service) composeByRule(cond *model.SearchCondition, store *FactStore) (
	*Hydrated, model.Timeline, Verdict, error) {

	hyd, err := NewRuleBasedComposer(store, s.links).Compose(cond)
	if err != nil {
		return nil, nil, Verdict{}, err
	}
	tl, err := BuildTimeline(hyd, cond)
	if err != nil {
		return nil, nil, Verdict{}, err
	}
	return hyd, tl, NewValidator(store).Validate(tl, cond), nil
}

func (s *Service) ruleBasedMeta() model.LLMMeta {
	meta := model.LLMMeta{Provider: model.LLMRuleBased, SchemaVersion: llm.SchemaVersion}
	if s.builder != nil {
		meta.PromptVersion = s.builder.Version()
	}
	return meta
}

// assemble は確定した時系列を Plan にまとめる。
// summary は **Go が確定した事実のみ**で、LLM は書き込めない。
func (s *Service) assemble(cond *model.SearchCondition, hyd *Hydrated, tl model.Timeline,
	verdict Verdict, warnings []model.Warning, sources []model.SourceStatus) *model.Plan {

	now := s.now()
	walk, meters := tl.TotalWalk()

	id := model.NewPlanID()
	p := &model.Plan{
		ID:           id,
		Status:       planStatus(sources),
		GeneratedAt:  now,
		ExpiresAt:    now.Add(s.opt.TTL),
		ShareURL:     s.shareURL(id),
		Narrative:    hyd.Narrative,
		Timeline:     tl,
		Alternatives: hyd.Alternatives,
		Summary: model.PlanSummary{
			StartAt:         tl.StartAt(),
			EndAt:           tl.EndAt(),
			TotalWalk:       walk,
			TotalWalkMeters: meters,
			Cost:            tl.EstimatedCost(cond.Party.Guests()),
			Feasibility:     verdict.Feasibility,
		},
	}
	// 警告に段の重複があっても UI が困るだけなので、コードと文言で一意にする。
	p.Warnings = dedupeWarnings(append(warnings, verdict.Warnings...))
	return p
}

func (s *Service) shareURL(id model.PlanID) string {
	if s.opt.BaseURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/plan/%s", trimSlash(s.opt.BaseURL), id)
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// planStatus は部分失敗を隠さない。
// 楽天が落ちても飲食だけ返して partial にするほうが、全体 500 より UX が良い。
func planStatus(sources []model.SourceStatus) model.PlanStatus {
	for _, src := range sources {
		if src.Degraded() {
			return model.StatusPartial
		}
	}
	return model.StatusCompleted
}

func degradedWarnings(sources []model.SourceStatus) []model.Warning {
	var out []model.Warning
	for _, src := range sources {
		if !src.Degraded() {
			continue
		}
		out = append(out, model.NewWarning(model.WarnProviderDegraded, model.SeverityInfo, "",
			fmt.Sprintf("%sの情報を取得できなかったため、候補が少なくなっています。", providerJA(src.Provider))))
	}
	return out
}

func providerJA(p model.Provider) string {
	switch p {
	case model.ProviderGooglePlaces:
		return "飲食店"
	case model.ProviderGoogleRoutes:
		return "経路"
	case model.ProviderRakutenTravel:
		return "宿泊"
	}
	return string(p)
}

func dedupeWarnings(ws []model.Warning) []model.Warning {
	type key struct {
		code    model.WarningCode
		segment string
		message string
	}
	seen := make(map[key]struct{}, len(ws))
	out := make([]model.Warning, 0, len(ws))
	for _, w := range ws {
		k := key{w.Code, w.SegmentID, w.Message}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, w)
	}
	return out
}

// repairHints は次の生成に渡す「直してほしい点」を選ぶ。
// Validator の不合格理由は LLM が読んで直せる日本語なので、あればそれを優先する。
func repairHints(err error, v Verdict) []string {
	if len(v.Violations) > 0 {
		return v.Violations
	}
	return []string{err.Error()}
}
