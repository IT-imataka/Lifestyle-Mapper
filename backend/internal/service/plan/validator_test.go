package plan

import (
	"errors"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// openUntil は「その日の夕方から翌 close 時まで」の営業時間を作る。
func openUntil(t *testing.T, open, close string) model.OpeningHours {
	t.Helper()
	return model.OpeningHours{
		Known:   true,
		Periods: []model.OpeningPeriod{{Open: jst(t, open), Close: jst(t, close)}},
	}
}

// buildTimeline は fixture の下書きを時刻付きタイムラインまで組み上げる。
func (f *fixture) buildTimeline(t *testing.T) model.Timeline {
	t.Helper()
	h, err := NewHydrator(f.store, nil).Hydrate(f.draft(), f.cond)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	return tl
}

func hasWarning(v Verdict, code model.WarningCode) bool {
	for _, w := range v.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func TestValidateAcceptsFeasiblePlan(t *testing.T) {
	f := newFixture(t)
	f.izakay.Hours = openUntil(t, "2026-09-12 17:00", "2026-09-13 02:00")

	v := NewValidator(f.store).Validate(f.buildTimeline(t), f.cond)
	if !v.OK() {
		t.Fatalf("成立するプランが不合格になりました: %v", v.Violations)
	}
	if v.Feasibility != model.FeasibilityOK {
		t.Errorf("feasibility が %s です（ok を期待）", v.Feasibility)
	}
	if v.Err() != nil {
		t.Errorf("合格なのにエラーが返っています: %v", v.Err())
	}
}

// 滞在中に閉店する店は組み直しの対象。
func TestClosingDuringStayIsViolation(t *testing.T) {
	f := newFixture(t)
	// 21:38 着・23:08 まで滞在する想定に対し、23:00 閉店。
	f.izakay.Hours = openUntil(t, "2026-09-12 17:00", "2026-09-12 23:00")

	v := NewValidator(f.store).Validate(f.buildTimeline(t), f.cond)
	if v.OK() {
		t.Fatal("滞在中に閉店する店が通ってしまいました")
	}
	if v.Feasibility != model.FeasibilityInfeasible {
		t.Errorf("feasibility が %s です（infeasible を期待）", v.Feasibility)
	}
	if err := v.Err(); !apperror.From(err).Repairable() {
		t.Error("組み直しの余地があるのに repairable ではありません")
	}
}

// 到着時にまだ開いていない店も同様。
func TestArrivalBeforeOpeningIsViolation(t *testing.T) {
	f := newFixture(t)
	f.izakay.Hours = openUntil(t, "2026-09-12 23:30", "2026-09-13 05:00")

	v := NewValidator(f.store).Validate(f.buildTimeline(t), f.cond)
	if v.OK() {
		t.Fatal("開店前に到着するプランが通ってしまいました")
	}
}

// 営業時間が取れていない候補は「判定できない」であって「休業」ではない。
func TestUnknownOpeningHoursIsNotViolation(t *testing.T) {
	f := newFixture(t) // fixture の店は Hours 未設定
	v := NewValidator(f.store).Validate(f.buildTimeline(t), f.cond)
	if !v.OK() {
		t.Fatalf("営業時間不明の候補が不合格になりました: %v", v.Violations)
	}
}

// 閉店間際は不合格にはしないが、余裕がないことは伝える。
func TestClosingSoonIsTight(t *testing.T) {
	f := newFixture(t)
	f.izakay.Hours = openUntil(t, "2026-09-12 17:00", "2026-09-12 23:10") // 退店 23:08 の 2 分後

	v := NewValidator(f.store).Validate(f.buildTimeline(t), f.cond)
	if !v.OK() {
		t.Fatalf("不合格になりました: %v", v.Violations)
	}
	if v.Feasibility != model.FeasibilityTight {
		t.Errorf("feasibility が %s です（tight を期待）", v.Feasibility)
	}
	if !hasWarning(v, model.WarnTightSchedule) {
		t.Error("TIGHT_SCHEDULE の警告がありません")
	}
}

// 徒歩の上限はユーザーの体力の申告。超えたら候補を選び直させる。
func TestWalkOverLimitIsViolation(t *testing.T) {
	f := newFixture(t)
	tl := f.buildTimeline(t)
	f.cond.Situation.MaxWalk = 5 * time.Minute // 会場→居酒屋は徒歩 8 分

	v := NewValidator(f.store).Validate(tl, f.cond)
	if v.OK() {
		t.Fatal("徒歩の上限を超えるプランが通ってしまいました")
	}
}

// 最終チェックインに間に合わない宿は組み直しの対象。
func TestCheckInDeadlineMissedIsViolation(t *testing.T) {
	f := newFixture(t)
	deadline := model.ClockTime(22 * 60) // 到着は 23:14
	f.hotel.Plans[0].CheckInDeadline = &deadline

	v := NewValidator(f.store).Validate(f.buildTimeline(t), f.cond)
	if v.OK() {
		t.Fatal("チェックイン期限を過ぎたプランが通ってしまいました")
	}
}

func TestCheckInDeadlineNearIsTight(t *testing.T) {
	f := newFixture(t)
	deadline := model.ClockTime(23*60 + 16) // 到着 23:14 の 2 分後
	f.hotel.Plans[0].CheckInDeadline = &deadline

	v := NewValidator(f.store).Validate(f.buildTimeline(t), f.cond)
	if !v.OK() {
		t.Fatalf("不合格になりました: %v", v.Violations)
	}
	if v.Feasibility != model.FeasibilityTight {
		t.Errorf("feasibility が %s です（tight を期待）", v.Feasibility)
	}
}

// FactStore を通っていない候補は、ここでも遮断する（幻覚の防壁は二重にかける）。
func TestUnknownCandidateIsViolation(t *testing.T) {
	f := newFixture(t)
	ghost := &model.PlaceFact{
		ID: model.CandidateID("cand_dining_099"), Cat: model.CategoryDining,
		Name: "実在しない居酒屋", Loc: model.Location{Lat: 35.70, Lng: 139.75},
	}
	tl := model.Timeline{
		model.NewDiningSegment(1, jst(t, "2026-09-12 21:30"), 90*time.Minute, ghost, nil),
	}

	v := NewValidator(f.store).Validate(tl, f.cond)
	if v.OK() {
		t.Fatal("FactStore に無い候補が通ってしまいました")
	}
}

// 予算超過は組み直しても直らない（候補が無い）。警告に留める。
func TestBudgetExceededIsWarningNotViolation(t *testing.T) {
	f := newFixture(t)
	max := 3000
	f.cond.Dining.BudgetPerPerson = model.MoneyRangeJPY{Min: 0, Max: &max} // 目安は 4000 円
	lodgingMax := 10000
	f.cond.Lodging.BudgetPerNight = model.MoneyRangeJPY{Min: 0, Max: &lodgingMax} // 実際は 16200 円

	v := NewValidator(f.store).Validate(f.buildTimeline(t), f.cond)
	if !v.OK() {
		t.Fatalf("予算超過で不合格になりました: %v", v.Violations)
	}
	if !hasWarning(v, model.WarnBudgetExceeded) {
		t.Error("BUDGET_EXCEEDED の警告がありません")
	}
	if n := len(v.Warnings); n != 2 {
		t.Errorf("警告が %d 件です（飲食と宿泊の 2 件を期待）: %+v", n, v.Warnings)
	}
}

// 希望した要素が入らなかったことは黙って隠さない。
func TestMissingCategoriesAreWarned(t *testing.T) {
	f := newFixture(t)
	tl := model.Timeline{
		model.NewBufferSegment(1, f.cond.Event.EndsAt, 30*time.Minute, model.BufferExitCongestion, "東京ドーム"),
	}

	v := NewValidator(f.store).Validate(tl, f.cond)
	if !v.OK() {
		t.Fatalf("不合格になりました: %v", v.Violations)
	}
	if !hasWarning(v, model.WarnNoDiningFound) || !hasWarning(v, model.WarnNoLodgingFound) {
		t.Errorf("候補不足の警告がありません: %+v", v.Warnings)
	}
}

// 構造が壊れたタイムラインは、そもそも表示以前の問題として即座に落とす。
func TestBrokenStructureIsInfeasible(t *testing.T) {
	f := newFixture(t)
	tl := model.Timeline{
		model.NewBufferSegment(1, f.cond.Event.EndsAt, 30*time.Minute, model.BufferExitCongestion, "東京ドーム"),
		// 前のコマの終了時刻と連続していない。
		model.NewDiningSegment(2, jst(t, "2026-09-12 22:00"), 60*time.Minute, f.izakay, nil),
	}

	v := NewValidator(f.store).Validate(tl, f.cond)
	if v.OK() {
		t.Fatal("時刻が連続していないタイムラインが通ってしまいました")
	}
	if v.Feasibility != model.FeasibilityInfeasible {
		t.Errorf("feasibility が %s です（infeasible を期待）", v.Feasibility)
	}
}

func TestShouldRepair(t *testing.T) {
	repairableErr := apperror.New(apperror.CodeTimelineInvalid, "")
	fatalErr := apperror.New(apperror.CodeNoCandidatesFound, "")

	cases := []struct {
		name     string
		err      error
		attempts int
		limit    int
		want     bool
	}{
		{"組み直せるエラーの初回", repairableErr, 0, 1, true},
		{"上限に達したら組み直さない", repairableErr, 1, 1, false},
		{"上限を上げれば続けられる", repairableErr, 1, 2, true},
		{"上限未指定は既定の 1 回", repairableErr, MaxRepairAttempts, 0, false},
		{"組み直しても直らないエラー", fatalErr, 0, 1, false},
		{"素の error は組み直さない", errors.New("boom"), 0, 1, false},
		{"エラーが無ければ組み直さない", nil, 0, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldRepair(c.err, c.attempts, c.limit); got != c.want {
				t.Errorf("ShouldRepair = %v（%v を期待）", got, c.want)
			}
		})
	}
}
