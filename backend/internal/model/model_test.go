package model

import (
	"strings"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
)

var jst = time.FixedZone("JST", 9*60*60)

func TestCandidateID(t *testing.T) {
	id, err := NewCandidateID(CategoryDining, 3)
	if err != nil {
		t.Fatalf("採番に失敗: %v", err)
	}
	if got, want := string(id), "cand_dining_003"; got != want {
		t.Errorf("ID = %q, want %q", got, want)
	}
	if !id.Valid() {
		t.Error("採番した ID が契約のパターンに合致しません")
	}
	if got := id.Category(); got != CategoryDining {
		t.Errorf("Category() = %q, want %q", got, CategoryDining)
	}
	if _, err := NewCandidateID(CategoryDining, 1000); err == nil {
		t.Error("連番 1000 は弾かれるべきです")
	}
	// LLM の幻覚を模した ID は必ず落ちること。
	for _, bad := range []string{"cand_dining_3", "cand_hotel_001", "dining_003", ""} {
		if CandidateID(bad).Valid() {
			t.Errorf("不正な ID を通しました: %q", bad)
		}
	}
}

func TestPlanIDIsULIDShaped(t *testing.T) {
	seen := map[PlanID]bool{}
	for i := 0; i < 100; i++ {
		id := NewPlanID()
		if !id.Valid() {
			t.Fatalf("生成した PlanID が契約のパターンに合致しません: %q", id)
		}
		if seen[id] {
			t.Fatalf("PlanID が重複しました: %q", id)
		}
		seen[id] = true
	}
	// ULID は時系列で辞書順が保たれる。
	a := NewPlanID()
	time.Sleep(2 * time.Millisecond)
	if b := NewPlanID(); !(string(a) < string(b)) {
		t.Errorf("後から採番した ID が辞書順で前に来ています: %q >= %q", a, b)
	}
}

func TestClockTimeOver24h(t *testing.T) {
	c, ok := ParseClockTime("26:00")
	if !ok {
		t.Fatal("26:00 を解釈できませんでした")
	}
	if c.String() != "26:00" {
		t.Errorf("String() = %q, want %q", c.String(), "26:00")
	}
	// 9/12 の 26:00 は 9/13 の 2:00。
	got := c.On(time.Date(2026, 9, 12, 21, 0, 0, 0, jst))
	want := time.Date(2026, 9, 13, 2, 0, 0, 0, jst)
	if !got.Equal(want) {
		t.Errorf("On() = %s, want %s", got, want)
	}
	if _, ok := ParseClockTime("2600"); ok {
		t.Error("区切りの無い文字列を通しました")
	}
}

func TestOpeningHoursCanStay(t *testing.T) {
	// 17:00 開店、翌 2:00 閉店の深夜営業。
	h := OpeningHours{Known: true, Periods: []OpeningPeriod{{
		Open:  time.Date(2026, 9, 12, 17, 0, 0, 0, jst),
		Close: time.Date(2026, 9, 13, 2, 0, 0, 0, jst),
	}}}
	arrival := time.Date(2026, 9, 12, 21, 38, 0, 0, jst)
	if !h.IsOpenAt(arrival) {
		t.Error("営業中と判定されるべきです")
	}
	if !h.CanStay(arrival, 90*time.Minute) {
		t.Error("90 分の滞在は収まるはずです")
	}
	if h.CanStay(time.Date(2026, 9, 13, 1, 45, 0, 0, jst), 90*time.Minute) {
		t.Error("閉店をまたぐ滞在は不可のはずです")
	}
	// 営業時間不明は「開いている」とみなさない。
	if (OpeningHours{}).IsOpenAt(arrival) {
		t.Error("営業時間不明を営業中と判定しました")
	}
}

func TestTimelineValidate(t *testing.T) {
	start := time.Date(2026, 9, 12, 21, 0, 0, 0, jst)
	place := &PlaceFact{ID: "cand_dining_003", Cat: CategoryDining, Name: "居酒屋"}
	route := &RouteFact{Key: NewRouteKey(RouteOriginVenue, "cand_dining_003", TravelWalk),
		DistanceMeters: 650, Duration: 8 * time.Minute}

	buf := NewBufferSegment(1, start, 30*time.Minute, BufferExitCongestion, "東京ドーム")
	mv := NewMoveSegment(2, buf.EndAt, route)
	din := NewDiningSegment(3, mv.EndAt, 90*time.Minute, place, nil)

	tl := Timeline{buf, mv, din}
	if err := tl.Validate(); err != nil {
		t.Fatalf("正しいタイムラインが弾かれました: %v", err)
	}
	if d, m := tl.TotalWalk(); d != 8*time.Minute || m != 650 {
		t.Errorf("TotalWalk() = %v/%dm, want 8m0s/650m", d, m)
	}

	// 時刻が連続していない場合は弾く。
	gap := Timeline{buf, mv, NewDiningSegment(3, mv.EndAt.Add(time.Minute), 90*time.Minute, place, nil)}
	if err := gap.Validate(); err == nil {
		t.Error("時刻の飛びを検出できていません")
	} else if !strings.Contains(err.Error(), "連続していません") {
		t.Errorf("想定外のエラー: %v", err)
	}

	// 同じ候補を 2 回使うプランは弾く。
	dup := Timeline{din, NewDiningSegment(4, din.EndAt, 30*time.Minute, place, nil)}
	if err := dup.Validate(); err == nil {
		t.Error("候補の重複を検出できていません")
	}

	// 種別と詳細の不一致は弾く。
	broken := Segment{ID: "seg_1", Type: SegmentDining, StartAt: start, EndAt: start,
		Buffer: &BufferDetail{Kind: BufferSpareTime}}
	if err := (Timeline{broken}).Validate(); err == nil {
		t.Error("種別と詳細の不一致を検出できていません")
	}
}

func TestLinkValidateRequiresTrackingID(t *testing.T) {
	l := Link{Kind: LinkAffiliate, Provider: LinkProviderRakutenTravel, Label: "予約", URL: "https://example.test"}
	if err := l.Validate(); err == nil {
		t.Error("計測 ID 無しのアフィリエイトリンクを通しました（収益の取りこぼしに直結します）")
	}
	l.TrackingID = "trk_b7"
	if err := l.Validate(); err != nil {
		t.Errorf("正しいリンクが弾かれました: %v", err)
	}
	// アフィリエイト以外は計測 ID 不要。
	if err := (Link{Kind: LinkMap, Provider: LinkProviderGoogleMaps, URL: "https://example.test"}).Validate(); err != nil {
		t.Errorf("地図リンクが弾かれました: %v", err)
	}
}

func TestEstimatedCost(t *testing.T) {
	cost := 4000
	total := 16200
	fare := 200
	start := time.Date(2026, 9, 12, 21, 0, 0, 0, jst)
	hotel := &HotelFact{ID: "cand_lodging_001", Plans: []HotelPlanFact{{TotalPriceJPY: total}}}
	tl := Timeline{
		NewDiningSegment(1, start, 90*time.Minute, &PlaceFact{ID: "cand_dining_003", EstimatedCostPerPersonJPY: &cost}, nil),
		NewMoveSegment(2, start, &RouteFact{Key: NewRouteKey("a", "b", TravelTransit), FareJPY: &fare}),
		NewLodgingSegment(3, start, start.Add(10*time.Hour), LodgingChoice{Hotel: hotel, Plan: &hotel.Plans[0]}, nil),
	}
	got := tl.EstimatedCost(2)
	want := CostBreakdown{DiningJPY: 8000, LodgingJPY: 16200, TransitJPY: 200}
	if got != want {
		t.Errorf("EstimatedCost() = %+v, want %+v", got, want)
	}
	if got.TotalJPY() != 24400 {
		t.Errorf("TotalJPY() = %d, want 24400", got.TotalJPY())
	}
}

func TestSearchConditionValidate(t *testing.T) {
	now := time.Date(2026, 8, 29, 19, 0, 0, 0, jst)
	valid := func() *SearchCondition {
		s := &SearchCondition{
			Event: Event{
				Name:   "○○ TOUR 2026 東京公演",
				Venue:  Venue{Name: "東京ドーム", Location: Location{Lat: 35.7056, Lng: 139.7519}},
				EndsAt: time.Date(2026, 9, 12, 21, 0, 0, 0, jst),
			},
			Situation: Situation{Intent: IntentStay},
			Dining:    DiningPreference{Enabled: true},
			Lodging:   LodgingPreference{Enabled: true},
		}
		s.ApplyDefaults()
		return s
	}

	if err := valid().Validate(now); err != nil {
		t.Fatalf("正しい条件が弾かれました: %v (%+v)", err, err.Details)
	}

	// 既定値が契約どおり入ること。
	s := valid()
	if s.Event.ExitBuffer != DefaultExitBuffer || s.Options.MaxCandidatesPerCategory != 8 ||
		s.Situation.MaxWalk != DefaultMaxWalk || s.Context.Timezone != DefaultTimezone {
		t.Errorf("既定値が適用されていません: %+v", s)
	}

	past := valid()
	past.Event.EndsAt = now.Add(-time.Hour)
	err := past.Validate(now)
	if err == nil {
		t.Fatal("過去の終演時刻を通しました")
	}
	if err.Code != apperror.CodeValidationFailed {
		t.Errorf("Code = %q, want %q", err.Code, apperror.CodeValidationFailed)
	}
	if len(err.Details) != 1 || err.Details[0].Field != "event.endsAt" {
		t.Errorf("details = %+v, want event.endsAt 1 件", err.Details)
	}

	// 飲食も宿泊も無効なら組むものが無い。
	empty := valid()
	empty.Dining.Enabled, empty.Lodging.Enabled = false, false
	if empty.Validate(now) == nil {
		t.Error("飲食・宿泊がどちらも無効な条件を通しました")
	}
}

func TestDistanceMeters(t *testing.T) {
	// 東京ドーム → 水道橋駅は約 350m。
	dome := Location{Lat: 35.7056, Lng: 139.7519}
	suidobashi := Location{Lat: 35.7021, Lng: 139.7538}
	if d := dome.DistanceMeters(suidobashi); d < 300 || d > 450 {
		t.Errorf("DistanceMeters() = %d, want 300〜450 程度", d)
	}
	if (Location{}).Valid() {
		t.Error("ゼロ値を有効な座標と判定しました")
	}
}
