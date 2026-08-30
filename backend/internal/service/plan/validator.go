package plan

import (
	"fmt"
	"strings"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// Validator は組み上がったタイムラインを世に出してよいか判定する最後の関門。
//
// 見るのは「時刻の矛盾・候補 ID の実在・徒歩の上限」の 3 点。いずれも
// **LLM が候補を選び直せば直りうる**性質のものなので、不合格は組み直しの合図になる。
// 一方で予算超過や候補不足は組み直しても直らないため、警告として正直に添えるだけにする。
//
// 判定の材料は Timeline と FactStore の事実だけで、LLM の文章は一切見ない。
type Validator struct {
	store *FactStore
}

func NewValidator(store *FactStore) *Validator { return &Validator{store: store} }

// MaxRepairAttempts は組み直しの上限。1 回に留めるのは、LLM 呼び出しが
// 体感速度に直結する（1 回あたり 5 秒前後）ため。2 回目で直らないものは
// 3 回目でも直らない。
const MaxRepairAttempts = 1

// Verdict は検査の結果。
type Verdict struct {
	Feasibility model.Feasibility
	// Warnings はユーザーに見せる注意書き。
	Warnings []model.Warning
	// Violations は **LLM が読んで直すための日本語**。組み直しプロンプトにそのまま載せる。
	Violations []string
}

func (v Verdict) OK() bool { return len(v.Violations) == 0 }

// Err は不合格を repairable なエラーにする。合格なら nil。
func (v Verdict) Err() *apperror.Error {
	if v.OK() {
		return nil
	}
	return apperror.Wrap(
		fmt.Errorf("時系列の検査に不合格です: %s", strings.Join(v.Violations, " / ")),
		apperror.CodeTimelineInvalid, "validator")
}

// ShouldRepair は「もう一度 LLM に組み直させるか」を決める。
// 上流 API の失敗や内部エラーで組み直しても無駄なので、repairable なものだけを対象にする。
// limit が 0 以下なら MaxRepairAttempts を使う。
func ShouldRepair(err error, attempts, limit int) bool {
	if limit <= 0 {
		limit = MaxRepairAttempts
	}
	return err != nil && attempts < limit && apperror.From(err).Repairable()
}

// Validate はタイムラインを検査する。
func (v *Validator) Validate(tl model.Timeline, cond *model.SearchCondition) Verdict {
	verdict := Verdict{Feasibility: model.FeasibilityOK}
	add := func(format string, args ...any) {
		verdict.Violations = append(verdict.Violations, fmt.Sprintf(format, args...))
	}
	warn := func(code model.WarningCode, sev model.Severity, segID, format string, args ...any) {
		verdict.Warnings = append(verdict.Warnings, model.NewWarning(code, sev, segID, fmt.Sprintf(format, args...)))
	}
	tight := false

	// 構造と連続性。ここが崩れているものは表示以前の問題。
	if err := tl.Validate(); err != nil {
		add("タイムラインの構造が不正です: %s", err.Error())
		verdict.Feasibility = model.FeasibilityInfeasible
		return verdict
	}

	var hasDining, hasLodging bool

	for _, seg := range tl {
		// 事実の実在確認。Hydrator を通っていれば必ず通るが、
		// **幻覚の遮断は二重にかけておく**。ここを抜けた ID だけが UI に出る。
		if id := seg.CandidateID(); id != "" && v.store != nil && !v.store.Has(id) {
			add("%s は存在しない候補です。提示された候補の中から選んでください", id)
			continue
		}

		switch seg.Type {
		case model.SegmentMove:
			// 徒歩の上限はユーザーの体力の申告。超えるなら別の候補を選ばせる。
			if seg.Move.Key.Mode == model.TravelWalk && seg.Move.Duration > cond.Situation.MaxWalk {
				add("%s → %s の徒歩が %d 分で、上限の %d 分を超えています。より近い候補を選んでください",
					seg.Move.From.Name, seg.Move.To.Name,
					minutes(seg.Move.Duration), minutes(cond.Situation.MaxWalk))
			}

		case model.SegmentDining:
			hasDining = true
			if slackTight := v.checkDining(seg, add); slackTight {
				tight = true
				warn(model.WarnTightSchedule, model.SeverityWarning, seg.ID,
					"%s の閉店まで余裕がありません。滞在時間が短くなる可能性があります。", seg.Dining.Name)
			}
			v.checkDiningBudget(seg, cond, warn)

		case model.SegmentLodging:
			hasLodging = true
			if slackTight := v.checkLodging(seg, cond, add); slackTight {
				tight = true
				warn(model.WarnTightSchedule, model.SeverityWarning, seg.ID,
					"%s の最終チェックイン時刻まで余裕がありません。", seg.Lodging.Hotel.Name)
			}
			v.checkLodgingBudget(seg, cond, warn)
		}
	}

	// 徒歩の合計は上限を超えていても組み直しの理由にはしない（1 回ごとの上限は別に見ている）。
	if walk, _ := tl.TotalWalk(); walk > 2*cond.Situation.MaxWalk {
		warn(model.WarnWalkLimitExceeded, model.SeverityInfo, "",
			"歩く時間の合計が%d分です。歩きやすい靴をおすすめします。", minutes(walk))
	}

	// 希望した要素が入らなかったことは、組み直しでは直らない（候補が無い）。正直に伝える。
	if cond.Dining.Enabled && !hasDining {
		warn(model.WarnNoDiningFound, model.SeverityWarning, "",
			"条件に合う飲食店が見つからなかったため、食事はプランに含めていません。")
	}
	if cond.Lodging.Enabled && !hasLodging {
		warn(model.WarnNoLodgingFound, model.SeverityWarning, "",
			"条件に合う宿が見つからなかったため、宿泊はプランに含めていません。")
	}

	switch {
	case !verdict.OK():
		verdict.Feasibility = model.FeasibilityInfeasible
	case tight:
		verdict.Feasibility = model.FeasibilityTight
	}
	return verdict
}

// checkDining は到着時刻に入店できるか、希望どおり滞在できるかを見る。
// 戻り値は「閉店まで余裕が乏しいか」。
func (v *Validator) checkDining(seg model.Segment, add func(string, ...any)) bool {
	hours := seg.Dining.Hours
	// 提供元が営業時間を返さなかった場合は「終日休業」ではない。
	// 判定できないものを不合格にすると、情報の欠けた店がすべて落ちる。
	if !hours.Known {
		return false
	}
	if !hours.IsOpenAt(seg.StartAt) {
		add("%s（%s）は到着予定の %s に営業していません。その時刻に開いている候補を選んでください",
			seg.Dining.ID, seg.Dining.Name, clock(seg.StartAt))
		return false
	}
	closesAt, ok := hours.ClosesAfter(seg.StartAt)
	if !ok {
		return false
	}
	if seg.EndAt.After(closesAt) {
		add("%s（%s）は %s 閉店で、%s までの滞在ができません。滞在時間を短くするか別の候補を選んでください",
			seg.Dining.ID, seg.Dining.Name, clock(closesAt), clock(seg.EndAt))
		return false
	}
	return closesAt.Sub(seg.EndAt) < model.TightSlack
}

// checkLodging は最終チェックイン時刻に間に合うかを見る。
func (v *Validator) checkLodging(seg model.Segment, cond *model.SearchCondition, add func(string, ...any)) bool {
	deadline, ok := seg.Lodging.Plan.CheckInDeadlineAt(stayDateOf(seg.StartAt, cond))
	if !ok {
		// チェックイン期限が不明なら判定しない。フロントには宿の表記をそのまま出す。
		return false
	}
	if seg.StartAt.After(deadline) {
		add("%s（%s）の最終チェックインは %s ですが、到着予定は %s です。手前の滞在を短くするか別の宿を選んでください",
			seg.Lodging.Hotel.ID, seg.Lodging.Hotel.Name, clock(deadline), clock(seg.StartAt))
		return false
	}
	return deadline.Sub(seg.StartAt) < model.TightSlack
}

func (v *Validator) checkDiningBudget(seg model.Segment, cond *model.SearchCondition, warn func(model.WarningCode, model.Severity, string, string, ...any)) {
	b := cond.Dining.BudgetPerPerson
	cost := seg.Dining.EstimatedCostPerPersonJPY
	if !b.HasMax() || cost == nil || *cost <= *b.Max {
		return
	}
	warn(model.WarnBudgetExceeded, model.SeverityInfo, seg.ID,
		"%s の目安は 1 人 %d 円で、ご予算の %d 円を上回ります。", seg.Dining.Name, *cost, *b.Max)
}

func (v *Validator) checkLodgingBudget(seg model.Segment, cond *model.SearchCondition, warn func(model.WarningCode, model.Severity, string, string, ...any)) {
	b := cond.Lodging.BudgetPerNight
	if !b.HasMax() {
		return
	}
	nights := cond.Lodging.Nights()
	if nights < 1 {
		nights = 1
	}
	perNight := seg.Lodging.Plan.TotalPriceJPY / nights
	if perNight <= *b.Max {
		return
	}
	warn(model.WarnBudgetExceeded, model.SeverityInfo, seg.ID,
		"%s は 1 泊 %d 円で、ご予算の %d 円を上回ります。", seg.Lodging.Hotel.Name, perNight, *b.Max)
}

func minutes(d time.Duration) int { return int(d.Minutes()) }

// clock は日をまたぐプランでも読めるよう日付を添える。深夜 2 時の「02:00」だけでは伝わらない。
func clock(t time.Time) string { return t.Format("1/2 15:04") }
