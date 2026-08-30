package plan

import (
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// jst は時刻の期待値を読みやすく書くための補助。
func jst(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, jstTest)
	if err != nil {
		t.Fatalf("時刻の解釈に失敗しました: %v", err)
	}
	return v
}

func assertSpan(t *testing.T, seg model.Segment, wantID string, wantType model.SegmentType, start, end time.Time) {
	t.Helper()
	if seg.ID != wantID || seg.Type != wantType {
		t.Errorf("セグメントが一致しません: got %s/%s, want %s/%s", seg.ID, seg.Type, wantID, wantType)
	}
	if !seg.StartAt.Equal(start) || !seg.EndAt.Equal(end) {
		t.Errorf("%s の時刻が一致しません:\n got  %s → %s\n want %s → %s", wantID,
			seg.StartAt.Format(time.RFC3339), seg.EndAt.Format(time.RFC3339),
			start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
}

// 設計書の例と同じ時刻列になることを、Hydrator からの一気通貫で確かめる。
func TestBuildTimelineAccumulatesFromEndsAt(t *testing.T) {
	f := newFixture(t)
	h, err := NewHydrator(f.store, nil).Hydrate(f.draft(), f.cond)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if len(tl) != 5 {
		t.Fatalf("セグメント数が %d 件です（5 件を期待）", len(tl))
	}

	assertSpan(t, tl[0], "seg_1", model.SegmentBuffer, jst(t, "2026-09-12 21:00"), jst(t, "2026-09-12 21:30"))
	assertSpan(t, tl[1], "seg_2", model.SegmentMove, jst(t, "2026-09-12 21:30"), jst(t, "2026-09-12 21:38"))
	assertSpan(t, tl[2], "seg_3", model.SegmentDining, jst(t, "2026-09-12 21:38"), jst(t, "2026-09-12 23:08"))
	assertSpan(t, tl[3], "seg_4", model.SegmentMove, jst(t, "2026-09-12 23:08"), jst(t, "2026-09-12 23:14"))
	assertSpan(t, tl[4], "seg_5", model.SegmentLodging, jst(t, "2026-09-12 23:14"), jst(t, "2026-09-13 10:00"))

	// 文章は LLM 由来のまま各コマに載る。
	if tl[0].Narrative == nil || tl[0].Narrative.Headline != "まずは動かず、余韻に浸る" {
		t.Errorf("narrative が引き継がれていません: %+v", tl[0].Narrative)
	}
}

// 移動時間は Routes API の実測値だけを使い、LLM の提案値は反映しない。
func TestMoveUsesMeasuredDuration(t *testing.T) {
	f := newFixture(t)
	draft := f.draft()
	// move に滞在時間を書かせても（本来スキーマ違反だが）時刻には効かないことを見る。
	h, err := NewHydrator(f.store, nil).Hydrate(draft, f.cond)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	h.Steps[1].Stay = 45 * time.Minute

	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if got := tl[1].Duration(); got != 8*time.Minute {
		t.Errorf("移動時間が %v です（実測の 8 分を期待）", got)
	}
}

// exitBufferMinutes は下限。LLM がそれより短く提案しても切り上げる。
func TestExitBufferIsFloor(t *testing.T) {
	f := newFixture(t)
	draft := f.draft()
	draft.Steps[0].StayMinutes = 5
	h, err := NewHydrator(f.store, nil).Hydrate(draft, f.cond)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if got := tl[0].Duration(); got != model.DefaultExitBuffer {
		t.Errorf("退場待機が %v です（%v を期待）", got, model.DefaultExitBuffer)
	}
	// 後続はすべて 30 分ぶん後ろにずれる。
	assertSpan(t, tl[1], "seg_2", model.SegmentMove, jst(t, "2026-09-12 21:30"), jst(t, "2026-09-12 21:38"))
}

// LLM が長めに待つ判断をしたなら、そちらを尊重する。
func TestExitBufferKeepsLongerProposal(t *testing.T) {
	f := newFixture(t)
	draft := f.draft()
	draft.Steps[0].StayMinutes = 45
	h, err := NewHydrator(f.store, nil).Hydrate(draft, f.cond)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if got := tl[0].Duration(); got != 45*time.Minute {
		t.Errorf("退場待機が %v です（45 分を期待）", got)
	}
}

// 待機を置かない下書きでも、終演と同時に歩き出すプランにはしない。
func TestExitBufferIsInsertedWhenAbsent(t *testing.T) {
	f := newFixture(t)
	h := &Hydrated{Steps: []ResolvedStep{
		{Kind: model.SegmentDining, Stay: 90 * time.Minute, Place: f.izakay},
	}}

	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if len(tl) != 2 || tl[0].Type != model.SegmentBuffer {
		t.Fatalf("先頭に退場待機が挿入されていません: %+v", tl)
	}
	if tl[0].Buffer.Kind != model.BufferExitCongestion || tl[0].Buffer.AtPlaceName != "東京ドーム" {
		t.Errorf("挿入された待機の内容が不正です: %+v", tl[0].Buffer)
	}
	if tl[0].Narrative != nil {
		t.Error("システムが挿入した待機に LLM の文章が付いています")
	}
	assertSpan(t, tl[1], "seg_2", model.SegmentDining, jst(t, "2026-09-12 21:30"), jst(t, "2026-09-12 23:00"))
}

// 長さ 0 の待機はコマにしない。空のカードを UI に出しても意味がない。
func TestZeroLengthBufferIsDropped(t *testing.T) {
	f := newFixture(t)
	h := &Hydrated{Steps: []ResolvedStep{
		{Kind: model.SegmentBuffer, Stay: 30 * time.Minute,
			Buffer: &model.BufferDetail{Kind: model.BufferExitCongestion, AtPlaceName: "東京ドーム"}},
		{Kind: model.SegmentBuffer, Stay: 0,
			Buffer: &model.BufferDetail{Kind: model.BufferSpareTime, AtPlaceName: "東京ドーム"}},
		{Kind: model.SegmentDining, Stay: 60 * time.Minute, Place: f.izakay},
	}}

	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if len(tl) != 2 {
		t.Fatalf("セグメント数が %d 件です（2 件を期待）: %+v", len(tl), tl)
	}
	// 連番は詰められ、欠番にならない。
	if tl[1].ID != "seg_2" {
		t.Errorf("セグメント ID が %s です（seg_2 を期待）", tl[1].ID)
	}
}

// チェックアウトは提供元の時刻で決まる。滞在時間ではない。
func TestLodgingCheckOutUsesProviderTime(t *testing.T) {
	f := newFixture(t)
	checkout := model.ClockTime(11 * 60)
	h := &Hydrated{Steps: []ResolvedStep{
		{Kind: model.SegmentLodging, Lodging: &model.LodgingChoice{
			Hotel: f.hotel,
			Plan:  &model.HotelPlanFact{ProviderPlanID: "p", CheckOutTime: &checkout},
		}},
	}}

	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	assertSpan(t, tl[1], "seg_2", model.SegmentLodging, jst(t, "2026-09-12 21:30"), jst(t, "2026-09-13 11:00"))
}

// 提供元がチェックアウト時刻を返さないプランは既定値に倒す。
func TestLodgingCheckOutFallsBackToDefault(t *testing.T) {
	f := newFixture(t)
	h := &Hydrated{Steps: []ResolvedStep{
		{Kind: model.SegmentLodging, Lodging: &model.LodgingChoice{
			Hotel: f.hotel,
			Plan:  &model.HotelPlanFact{ProviderPlanID: "p"},
		}},
	}}

	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if want := jst(t, "2026-09-13 10:00"); !tl[1].EndAt.Equal(want) {
		t.Errorf("チェックアウトが %s です（%s を期待）",
			tl[1].EndAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// 26:00 チェックインは前日の宿泊。翌朝のチェックアウトが 1 日ずれてはいけない。
func TestLateNightCheckInBelongsToPreviousDay(t *testing.T) {
	f := newFixture(t)
	// 終演を 23:30 にすると、待機と移動で日付をまたいで到着する。
	f.cond.Event.EndsAt = jst(t, "2026-09-12 23:30")
	checkout := model.ClockTime(10 * 60)
	h := &Hydrated{Steps: []ResolvedStep{
		{Kind: model.SegmentLodging, Lodging: &model.LodgingChoice{
			Hotel: f.hotel,
			Plan:  &model.HotelPlanFact{ProviderPlanID: "p", CheckOutTime: &checkout},
		}},
	}}

	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	assertSpan(t, tl[1], "seg_2", model.SegmentLodging, jst(t, "2026-09-13 00:00"), jst(t, "2026-09-13 10:00"))
}

// 検索条件に宿泊日があれば、それが宿泊日の唯一の根拠になる。
func TestStayDatePrefersRequestedCheckIn(t *testing.T) {
	f := newFixture(t)
	f.cond.Lodging.CheckIn = jst(t, "2026-09-12 00:00")
	f.cond.Lodging.CheckOut = jst(t, "2026-09-14 00:00") // 2 泊
	checkout := model.ClockTime(10 * 60)
	h := &Hydrated{Steps: []ResolvedStep{
		{Kind: model.SegmentLodging, Lodging: &model.LodgingChoice{
			Hotel: f.hotel,
			Plan:  &model.HotelPlanFact{ProviderPlanID: "p", CheckOutTime: &checkout},
		}},
	}}

	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if want := jst(t, "2026-09-14 10:00"); !tl[1].EndAt.Equal(want) {
		t.Errorf("2 泊のチェックアウトが %s です（%s を期待）",
			tl[1].EndAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// 到着がチェックアウトを過ぎる宿は成立しない。別の宿を選び直せるので repairable。
func TestArrivalAfterCheckOutIsRepairable(t *testing.T) {
	f := newFixture(t)
	f.cond.Lodging.CheckIn = jst(t, "2026-09-11 00:00") // 前日の予約に到着だけ翌日
	checkout := model.ClockTime(10 * 60)
	h := &Hydrated{Steps: []ResolvedStep{
		{Kind: model.SegmentLodging, Lodging: &model.LodgingChoice{
			Hotel: f.hotel,
			Plan:  &model.HotelPlanFact{ProviderPlanID: "p", CheckOutTime: &checkout},
		}},
	}}

	_, err := BuildTimeline(h, f.cond)
	if err == nil {
		t.Fatal("チェックアウト済みの宿が通ってしまいました")
	}
	if got := apperror.CodeOf(err); got != apperror.CodeTimelineInvalid {
		t.Errorf("エラーコードが %s です（%s を期待）", got, apperror.CodeTimelineInvalid)
	}
	if !apperror.From(err).Repairable() {
		t.Error("組み直しの余地があるのに repairable ではありません")
	}
}

// タイムラインは隙間なく連続していなければならない。
func TestTimelineIsContinuous(t *testing.T) {
	f := newFixture(t)
	h, err := NewHydrator(f.store, nil).Hydrate(f.draft(), f.cond)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	tl, err := BuildTimeline(h, f.cond)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if err := tl.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !tl.StartAt().Equal(f.cond.Event.EndsAt) {
		t.Errorf("起点が %s です（endsAt %s を期待）",
			tl.StartAt().Format(time.RFC3339), f.cond.Event.EndsAt.Format(time.RFC3339))
	}
	walk, meters := tl.TotalWalk()
	if walk != 14*time.Minute || meters != 1080 {
		t.Errorf("徒歩合計が %v / %dm です（14 分 / 1080m を期待）", walk, meters)
	}
}
