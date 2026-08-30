package plan

import (
	"errors"
	"fmt"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// timecalc は Hydrated（順序・滞在時間まで確定したプラン）に絶対時刻を与える。
//
// **LLM に時刻の足し算をさせない**ためだけに存在する層。起点は event.endsAt ただ 1 つで、
// そこから buffer は滞在時間、move は Routes API の実測値、dining は滞在時間、
// lodging は提供元のチェックアウト時刻を順に積み上げる。
// UI に出る時刻はすべてここで作られたものになる。

// DefaultCheckOut は宿泊プランがチェックアウト時刻を返さなかったときの既定値。
// 楽天トラベルはプランによって checkOutTime が空で返るため、10:00 に倒す。
const DefaultCheckOut = model.ClockTime(10 * 60)

// lateNightCutoffHour より前の到着は前日の宿泊として数える。
// 終演が遅い公演では 26:00（翌 2 時）チェックインが普通に起きる。
const lateNightCutoffHour = 6

// BuildTimeline は Hydrated を時刻付きの Timeline にする。
//
// 隙間を作らないのが不変条件で、各コマの開始時刻は必ず前のコマの終了時刻に等しい。
// 「何もしない時間」は buffer として明示的にコマ化する（Timeline.Validate が連続性を検査する）。
func BuildTimeline(h *Hydrated, cond *model.SearchCondition) (model.Timeline, error) {
	if h == nil || cond == nil {
		return nil, apperror.Internal(errors.New("入力が nil です"), "timecalc.BuildTimeline")
	}
	if len(h.Steps) == 0 {
		return nil, timelineErr(errors.New("ステップが 1 つもありません"))
	}

	loc := cond.Context.TimeLocation()
	// 起点。ユーザーのタイムゾーンに寄せておくと、以降の日付境界の判定がぶれない。
	cursor := cond.Event.EndsAt.In(loc)

	steps := withExitBuffer(h.Steps, cond)
	timeline := make(model.Timeline, 0, len(steps))
	order := 0
	next := func() int { order++; return order }

	for _, s := range steps {
		var seg model.Segment

		switch s.Kind {
		case model.SegmentBuffer:
			if s.Buffer == nil {
				return nil, apperror.Internal(fmt.Errorf("buffer の詳細がありません"), "timecalc.BuildTimeline")
			}
			if s.Stay <= 0 {
				continue // 長さ 0 の待機はコマにしない
			}
			seg = model.NewBufferSegment(next(), cursor, s.Stay, s.Buffer.Kind, s.Buffer.AtPlaceName)

		case model.SegmentMove:
			if s.Route == nil {
				return nil, apperror.Internal(fmt.Errorf("move に経路がありません"), "timecalc.BuildTimeline")
			}
			// 所要時間は実測値をそのまま使う。LLM の提案値は捨てる。
			seg = model.NewMoveSegment(next(), cursor, s.Route)

		case model.SegmentDining:
			if s.Place == nil {
				return nil, apperror.Internal(fmt.Errorf("dining に候補がありません"), "timecalc.BuildTimeline")
			}
			stay := s.Stay
			if stay <= 0 {
				stay = cond.Dining.DesiredStay
			}
			seg = model.NewDiningSegment(next(), cursor, stay, s.Place, s.Links)

		case model.SegmentLodging:
			if s.Lodging == nil || s.Lodging.Hotel == nil || s.Lodging.Plan == nil {
				return nil, apperror.Internal(fmt.Errorf("lodging に宿泊プランがありません"), "timecalc.BuildTimeline")
			}
			checkOut, err := lodgingCheckOut(cursor, s.Lodging.Plan, cond)
			if err != nil {
				return nil, err
			}
			seg = model.NewLodgingSegment(next(), cursor, checkOut, *s.Lodging, s.Links)

		default:
			return nil, timelineErr(fmt.Errorf("未知のステップ種別です: %q", s.Kind))
		}

		seg.Narrative = s.Narrative
		cursor = seg.EndAt
		timeline = append(timeline, seg)
	}

	// 種別と詳細の対応・ID の重複・時刻の連続性をここで一括検査する。
	// 不合格なら LLM に組み直させる余地があるため repairable にして返す。
	if err := timeline.Validate(); err != nil {
		return nil, timelineErr(err)
	}
	return timeline, nil
}

// withExitBuffer は先頭に規制退場の待機を保証する。
//
// 起点が endsAt である以上、退場に要する時間を LLM の提案任せにすると
// 「終演と同時に歩き出す」プランが通ってしまう。ユーザーが指定した
// exitBufferMinutes を**下限**として必ず確保する（上回るぶんには LLM の判断を尊重する）。
func withExitBuffer(steps []ResolvedStep, cond *model.SearchCondition) []ResolvedStep {
	buf := cond.Event.ExitBuffer
	if buf <= 0 {
		return steps
	}
	if first := steps[0]; first.Kind == model.SegmentBuffer && first.Buffer != nil &&
		first.Buffer.Kind == model.BufferExitCongestion {
		if first.Stay >= buf {
			return steps
		}
		out := append([]ResolvedStep(nil), steps...)
		out[0].Stay = buf
		return out
	}
	// LLM が待機を置かなかった場合。文章は付かないが、時刻の正しさを優先する。
	head := ResolvedStep{
		Kind: model.SegmentBuffer,
		Stay: buf,
		Buffer: &model.BufferDetail{
			Kind:        model.BufferExitCongestion,
			AtPlaceName: cond.Event.Venue.Name,
		},
	}
	return append([]ResolvedStep{head}, steps...)
}

// lodgingCheckOut はチェックアウトの絶対時刻を決める。
func lodgingCheckOut(arrival time.Time, p *model.HotelPlanFact, cond *model.SearchCondition) (time.Time, error) {
	stayDate := stayDateOf(arrival, cond)
	nights := cond.Lodging.Nights()
	if nights < 1 {
		nights = 1
	}
	// CheckOutAt は「宿泊日の翌朝」を返すため、最終泊の日付を渡す。
	lastNight := stayDate.AddDate(0, 0, nights-1)

	checkOut, ok := p.CheckOutAt(lastNight)
	if !ok {
		checkOut = DefaultCheckOut.On(lastNight.AddDate(0, 0, 1))
	}
	if !checkOut.After(arrival) {
		// 到着がチェックアウトを過ぎている＝この宿では成立しない。別の宿なら組み直せる。
		return time.Time{}, timelineErr(fmt.Errorf(
			"チェックイン %s がチェックアウト %s を過ぎています",
			arrival.Format(time.RFC3339), checkOut.Format(time.RFC3339)))
	}
	return checkOut, nil
}

// stayDateOf は宿泊日（＝予約上のチェックイン日）を返す。
//
// 26:00 チェックインのような深夜到着は、暦の上では翌日でも前日の宿泊として数える。
// ここを取り違えるとチェックアウトが丸 1 日ずれる。
func stayDateOf(arrival time.Time, cond *model.SearchCondition) time.Time {
	if !cond.Lodging.CheckIn.IsZero() {
		return cond.Lodging.CheckIn.In(arrival.Location())
	}
	if arrival.Hour() < lateNightCutoffHour {
		return arrival.AddDate(0, 0, -1)
	}
	return arrival
}

func timelineErr(cause error) *apperror.Error {
	return apperror.Wrap(cause, apperror.CodeTimelineInvalid, "timecalc")
}
