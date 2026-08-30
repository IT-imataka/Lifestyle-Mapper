package plan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/llm"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
	"github.com/taka/lifestyle-mapper/backend/internal/prompt"
)

// stubCollector は収集済みの FactStore をそのまま返す。外部 API を叩かずに全段を通す。
type stubCollector struct {
	store   *FactStore
	sources []model.SourceStatus
	err     error
}

func (c *stubCollector) Collect(context.Context, *model.SearchCondition) (*FactStore, []model.SourceStatus, error) {
	return c.store, c.sources, c.err
}

// memRepo は保存の契約だけを満たす最小の実装。
type memRepo struct {
	plans   map[model.PlanID]*model.Plan
	saveErr error
}

func newMemRepo() *memRepo { return &memRepo{plans: map[model.PlanID]*model.Plan{}} }

func (r *memRepo) Save(_ context.Context, p *model.Plan) error {
	if r.saveErr != nil {
		return r.saveErr
	}
	r.plans[p.ID] = p
	return nil
}

func (r *memRepo) Find(_ context.Context, id model.PlanID) (*model.Plan, error) {
	return r.plans[id], nil
}

func newService(f *fixture, client llm.Composer, repo Repository, sources []model.SourceStatus) *Service {
	return NewService(Deps{
		Collector:  &stubCollector{store: f.store, sources: sources},
		Repository: repo,
		LLM:        client,
		Prompt:     prompt.MustNew(),
		Now:        func() time.Time { return f.cond.Event.EndsAt.Add(-2 * time.Hour) },
	}, Options{
		TTL:               15 * time.Minute,
		BaseURL:           "https://lifestyle-mapper.app/",
		MaxRepairAttempts: 1,
		LLMEnabled:        true,
	})
}

func TestGenerateBuildsPlanFromLLMDraft(t *testing.T) {
	f := newFixture(t)
	repo := newMemRepo()
	client := newFakeLLM(f)

	res, err := newService(f, client, repo, okSources()).Generate(context.Background(), f.cond)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	p := res.Plan
	if !p.ID.Valid() {
		t.Errorf("プラン ID が不正です: %s", p.ID)
	}
	if p.Status != model.StatusCompleted {
		t.Errorf("status が %s です（completed を期待）", p.Status)
	}
	if p.ShareURL != "https://lifestyle-mapper.app/plan/"+p.ID.String() {
		t.Errorf("shareUrl が %q です", p.ShareURL)
	}
	if !p.ExpiresAt.Equal(p.GeneratedAt.Add(15 * time.Minute)) {
		t.Errorf("expiresAt が %s です（生成 15 分後を期待）", p.ExpiresAt)
	}
	// 文章は LLM 由来、事実は Go 由来。両方が揃う。
	if p.Narrative == nil || p.Narrative.Title != "終演の余韻を、水道橋の深夜に持ち帰る" {
		t.Errorf("LLM の文章が載っていません: %+v", p.Narrative)
	}
	if len(p.Timeline) != 5 {
		t.Fatalf("セグメント数が %d 件です（5 件を期待）", len(p.Timeline))
	}
	if p.Summary.Feasibility != model.FeasibilityOK {
		t.Errorf("feasibility が %s です", p.Summary.Feasibility)
	}
	if p.Summary.TotalWalkMeters != 1080 {
		t.Errorf("徒歩距離が %dm です（1080m を期待）", p.Summary.TotalWalkMeters)
	}
	if p.Summary.Cost.LodgingJPY != 16200 || p.Summary.Cost.DiningJPY != 8000 {
		t.Errorf("費用が %+v です", p.Summary.Cost)
	}
	if res.LLM.Provider != model.LLMAnthropic || res.LLM.PromptVersion != prompt.Version {
		t.Errorf("meta.llm が %+v です", res.LLM)
	}
	if repo.plans[p.ID] == nil {
		t.Error("プランが保存されていません")
	}
}

// LLM に到達できなければルールベースで組む。**サービスは死なない**。
func TestGenerateFallsBackToRuleBased(t *testing.T) {
	f := newFixture(t)
	client := &fakeLLM{err: llm.Unavailable(model.LLMAnthropic, errors.New("接続できません"))}

	res, err := newService(f, client, newMemRepo(), okSources()).Generate(context.Background(), f.cond)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Plan.Narrative != nil {
		t.Error("ルールベースなのに文章が付いています")
	}
	if len(res.Plan.Timeline) == 0 {
		t.Error("時系列が空です")
	}
	if res.LLM.Provider != model.LLMRuleBased {
		t.Errorf("meta.llm.provider が %s です（rule_based を期待）", res.LLM.Provider)
	}
	if !hasPlanWarning(res.Plan, model.WarnLLMUnavailable) {
		t.Error("LLM が使えなかったことが伝わっていません")
	}
}

// 利用者が自分で文章生成を切った場合は、警告を出さない。
func TestGenerateSkipsLLMOnRequest(t *testing.T) {
	f := newFixture(t)
	f.cond.Options.LLMNarrative = false
	client := newFakeLLM(f)

	res, err := newService(f, client, newMemRepo(), okSources()).Generate(context.Background(), f.cond)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if client.calls != 0 {
		t.Error("llmNarrative=false なのに LLM が呼ばれました")
	}
	if hasPlanWarning(res.Plan, model.WarnLLMUnavailable) {
		t.Error("利用者が切った機能について警告が出ています")
	}
}

// 検証に落ちた下書きは、理由を添えて 1 回だけ組み直させる。
func TestGenerateRepairsOnceThenSucceeds(t *testing.T) {
	f := newFixture(t)
	// 1 回目は実在しない候補を指す下書き、2 回目は正しい下書き。
	bad := f.draft()
	bad.Steps[2].CandidateID = sp("cand_dining_099")
	client := &scriptedLLM{responses: []*llm.Response{
		{Draft: bad, Meta: model.LLMMeta{Provider: model.LLMAnthropic}},
		{Draft: f.draft(), Meta: model.LLMMeta{Provider: model.LLMAnthropic}},
	}}

	res, err := newService(f, client, newMemRepo(), okSources()).Generate(context.Background(), f.cond)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if client.calls != 2 {
		t.Errorf("LLM の呼び出しが %d 回です（2 回を期待）", client.calls)
	}
	if res.LLM.RepairAttempts != 1 {
		t.Errorf("repairAttempts が %d です（1 を期待）", res.LLM.RepairAttempts)
	}
	// 2 回目のプロンプトには前回の不備が載る。
	if len(client.gotViolations) < 2 || len(client.gotViolations[1]) == 0 {
		t.Error("組み直しに不合格理由が渡っていません")
	}
}

// 組み直しても直らなければルールベースへ落ちる。全体 500 にはしない。
func TestGenerateFallsBackAfterRepairLimit(t *testing.T) {
	f := newFixture(t)
	bad := f.draft()
	bad.Steps[2].CandidateID = sp("cand_dining_099")
	client := &scriptedLLM{responses: []*llm.Response{
		{Draft: bad, Meta: model.LLMMeta{Provider: model.LLMAnthropic}},
		{Draft: bad, Meta: model.LLMMeta{Provider: model.LLMAnthropic}},
	}}

	res, err := newService(f, client, newMemRepo(), okSources()).Generate(context.Background(), f.cond)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if client.calls != 2 {
		t.Errorf("LLM の呼び出しが %d 回です（初回 + 組み直し 1 回を期待）", client.calls)
	}
	if res.LLM.Provider != model.LLMRuleBased {
		t.Errorf("meta.llm.provider が %s です（rule_based を期待）", res.LLM.Provider)
	}
}

// 一部のプロバイダが落ちても、組めたなら partial で返す。
func TestGenerateReportsPartialOnDegradedSource(t *testing.T) {
	f := newFixture(t)
	sources := append(okSources(), model.SourceStatus{
		Provider: model.ProviderRakutenTravel, State: model.SourceDegraded,
		Err: apperror.New(apperror.CodeUpstreamTimeout, ""),
	})

	res, err := newService(f, newFakeLLM(f), newMemRepo(), sources).Generate(context.Background(), f.cond)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Plan.Status != model.StatusPartial {
		t.Errorf("status が %s です（partial を期待）", res.Plan.Status)
	}
	if !hasPlanWarning(res.Plan, model.WarnProviderDegraded) {
		t.Error("部分失敗が warnings に出ていません")
	}
	if len(res.Sources) != len(sources) {
		t.Error("meta.sources がそのまま返っていません")
	}
}

func TestGenerateFailsWithoutCandidates(t *testing.T) {
	f := newFixture(t)
	empty := NewFactStore(f.store.Venue())
	svc := NewService(Deps{Collector: &stubCollector{store: empty}, Repository: newMemRepo(),
		Now: func() time.Time { return f.cond.Event.EndsAt.Add(-time.Hour) }}, Options{})

	_, err := svc.Generate(context.Background(), f.cond)
	if got := apperror.CodeOf(err); got != apperror.CodeNoCandidatesFound {
		t.Errorf("エラーコードが %s です（%s を期待）", got, apperror.CodeNoCandidatesFound)
	}
}

func TestGenerateRejectsInvalidCondition(t *testing.T) {
	f := newFixture(t)
	// 終演が過去。
	svc := NewService(Deps{Collector: &stubCollector{store: f.store},
		Now: func() time.Time { return f.cond.Event.EndsAt.Add(time.Hour) }}, Options{})

	_, err := svc.Generate(context.Background(), f.cond)
	if got := apperror.CodeOf(err); got != apperror.CodeValidationFailed {
		t.Errorf("エラーコードが %s です（%s を期待）", got, apperror.CodeValidationFailed)
	}
}

func TestGetReturnsSavedPlan(t *testing.T) {
	f := newFixture(t)
	repo := newMemRepo()
	svc := newService(f, newFakeLLM(f), repo, okSources())

	res, err := svc.Generate(context.Background(), f.cond)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got, err := svc.Get(context.Background(), res.Plan.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != res.Plan.ID {
		t.Errorf("別のプランが返りました: %s", got.ID)
	}
}

func TestGetRejectsUnknownAndExpired(t *testing.T) {
	f := newFixture(t)
	repo := newMemRepo()
	svc := newService(f, newFakeLLM(f), repo, okSources())
	res, err := svc.Generate(context.Background(), f.cond)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if _, err := svc.Get(context.Background(), "pln_notavalidid"); apperror.CodeOf(err) != apperror.CodePlanNotFound {
		t.Errorf("不正な ID のコードが %s です", apperror.CodeOf(err))
	}
	if _, err := svc.Get(context.Background(), model.NewPlanID()); apperror.CodeOf(err) != apperror.CodePlanNotFound {
		t.Errorf("未知の ID のコードが %s です", apperror.CodeOf(err))
	}

	// 空室の鮮度が切れたら再検索を促す。
	expired := NewService(Deps{Collector: &stubCollector{store: f.store}, Repository: repo,
		Now: func() time.Time { return res.Plan.ExpiresAt.Add(time.Second) }}, Options{})
	if _, err := expired.Get(context.Background(), res.Plan.ID); apperror.CodeOf(err) != apperror.CodePlanExpired {
		t.Errorf("期限切れのコードが %s です（%s を期待）", apperror.CodeOf(err), apperror.CodePlanExpired)
	}
}

// ── 補助 ──────────────────────────────────────────────

// scriptedLLM は呼び出しごとに別の応答を返す。組み直しの検証に使う。
type scriptedLLM struct {
	responses     []*llm.Response
	calls         int
	gotViolations [][]string
}

func (s *scriptedLLM) ComposePlan(_ context.Context, req llm.Request) (*llm.Response, error) {
	s.gotViolations = append(s.gotViolations, req.Violations)
	s.calls++
	if s.calls > len(s.responses) {
		return nil, llm.Unavailable(model.LLMAnthropic, errors.New("応答が尽きました"))
	}
	return s.responses[s.calls-1], nil
}

func (s *scriptedLLM) Provider() model.LLMProvider { return model.LLMAnthropic }

func okSources() []model.SourceStatus {
	return []model.SourceStatus{
		{Provider: model.ProviderGooglePlaces, State: model.SourceOK, ResultCount: 24},
		{Provider: model.ProviderGoogleRoutes, State: model.SourceOK, ResultCount: 24, Cached: true},
	}
}

func hasPlanWarning(p *model.Plan, code model.WarningCode) bool {
	for _, w := range p.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}
