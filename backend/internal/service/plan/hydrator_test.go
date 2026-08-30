package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/llm"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

// 設計書の例（東京ドーム → 居酒屋 → ホテル）を組み立てる。
type fixture struct {
	store  *FactStore
	cond   *model.SearchCondition
	izakay *model.PlaceFact
	ramen  *model.PlaceFact
	hotel  *model.HotelFact
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := NewFactStore(model.Waypoint{
		Name:     "東京ドーム",
		Location: model.Location{Lat: 35.7056, Lng: 139.7519},
	})

	cost := 4000
	izakaya := &model.PlaceFact{
		Cat: model.CategoryDining, ProviderPlaceID: "ChIJizakaya", Name: "居酒屋 ○○ 水道橋店",
		Loc: model.Location{Lat: 35.7021, Lng: 139.7538}, PhoneNumber: "+81-3-1234-5678",
		EstimatedCostPerPersonJPY: &cost,
	}
	ramen := &model.PlaceFact{
		Cat: model.CategoryDining, ProviderPlaceID: "ChIJramen", Name: "ラーメン △△",
		Loc: model.Location{Lat: 35.7030, Lng: 139.7550},
	}
	deadline := model.ClockTime(26 * 60)
	checkout := model.ClockTime(10 * 60)
	hotel := &model.HotelFact{
		ProviderHotelID: "123456", Name: "○○ホテル 後楽園",
		Loc: model.Location{Lat: 35.7075, Lng: 139.7519},
		Plans: []model.HotelPlanFact{
			{ProviderPlanID: "9876543", PlanName: "【素泊まり】直前割", TotalPriceJPY: 16200,
				VacancyStatus: model.VacancyFewLeft, RemainingRooms: ip(2),
				CheckInDeadline: &deadline, CheckOutTime: &checkout},
			{ProviderPlanID: "9876544", PlanName: "【朝食付き】", TotalPriceJPY: 19000,
				VacancyStatus: model.VacancyAvailable},
		},
	}

	if _, err := store.AddPlace(izakaya); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlace(ramen); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddHotel(hotel); err != nil {
		t.Fatal(err)
	}

	// 会場 → 居酒屋 → ホテルの徒歩経路。
	mustPutRoute(t, store, model.RouteOriginVenue, string(izakaya.ID), model.TravelWalk, 650, 8*time.Minute)
	mustPutRoute(t, store, string(izakaya.ID), string(hotel.ID), model.TravelWalk, 430, 6*time.Minute)

	cond := &model.SearchCondition{
		Event: model.Event{
			Venue:  model.Venue{Name: "東京ドーム", Location: model.Location{Lat: 35.7056, Lng: 139.7519}},
			EndsAt: time.Date(2026, 9, 12, 21, 0, 0, 0, jstTest),
		},
		Situation: model.Situation{Intent: model.IntentStay},
		Dining:    model.DiningPreference{Enabled: true},
		Lodging:   model.LodgingPreference{Enabled: true},
	}
	cond.ApplyDefaults()
	// bool は ApplyDefaults が触らない（未指定と明示的な false を区別できないため）。
	// controller が既定値を適用した後の状態に合わせる。
	cond.Options.LLMNarrative = true

	return &fixture{store: store, cond: cond, izakay: izakaya, ramen: ramen, hotel: hotel}
}

var jstTest = time.FixedZone("JST", 9*60*60)

func mustPutRoute(t *testing.T, s *FactStore, from, to string, mode model.TravelMode, meters int, d time.Duration) {
	t.Helper()
	if err := s.PutRoute(&model.RouteFact{
		Key: model.NewRouteKey(from, to, mode), DistanceMeters: meters, Duration: d,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) draft() *llm.PlanDraft {
	return &llm.PlanDraft{
		PlanTitle:   "終演の余韻を、水道橋の深夜に持ち帰る",
		PlanSummary: "退場の混雑をやり過ごしてから深夜営業の居酒屋へ。",
		Vibe:        "relaxed_late_night",
		Steps: []llm.DraftStep{
			{Order: 1, Kind: model.SegmentBuffer, StayMinutes: 30, Headline: "まずは動かず、余韻に浸る"},
			{Order: 2, Kind: model.SegmentMove, StayMinutes: 0, Headline: "水道橋方面へ徒歩で"},
			{Order: 3, Kind: model.SegmentDining, CandidateID: sp(string(f.izakay.ID)), StayMinutes: 90,
				Headline: "深夜まで開いている居酒屋へ"},
			{Order: 4, Kind: model.SegmentMove, StayMinutes: 0, Headline: "ホテルへ"},
			{Order: 5, Kind: model.SegmentLodging, CandidateID: sp(string(f.hotel.ID)), StayMinutes: 0,
				Headline: "会場徒歩圏で、翌朝もゆっくり"},
		},
		UnusedCandidateNotes: []llm.UnusedCandidateNote{
			{CandidateID: string(f.ramen.ID), Reason: "23時閉店で滞在30分未満になるため見送りました。"},
		},
	}
}

func TestHydrateWiresFactsFromStore(t *testing.T) {
	f := newFixture(t)
	got, err := NewHydrator(f.store, nil).Hydrate(f.draft(), f.cond)
	if err != nil {
		t.Fatalf("再水和に失敗: %v", err)
	}
	if len(got.Steps) != 5 {
		t.Fatalf("ステップ数 = %d, want 5", len(got.Steps))
	}

	// buffer: 終演直後は規制退場、場所は会場。
	if b := got.Steps[0].Buffer; b == nil || b.Kind != model.BufferExitCongestion || b.AtPlaceName != "東京ドーム" {
		t.Errorf("待機の詳細が違います: %+v", b)
	}
	if got.Steps[0].Stay != 30*time.Minute {
		t.Errorf("待機時間 = %v, want 30m", got.Steps[0].Stay)
	}

	// move: 実測経路が入り、LLM の提案値は使われない。
	if r := got.Steps[1].Route; r == nil || r.DistanceMeters != 650 || r.Duration != 8*time.Minute {
		t.Errorf("経路が差し戻されていません: %+v", r)
	}
	if got.Steps[1].Stay != 0 {
		t.Errorf("move に滞在時間が入っています: %v", got.Steps[1].Stay)
	}

	// dining: FactStore の実体がそのまま指されている（値の写しを作らない）。
	if got.Steps[2].Place != f.izakay {
		t.Error("飲食の事実が FactStore の実体と別物です")
	}
	if got.Steps[2].Stay != 90*time.Minute {
		t.Errorf("滞在時間 = %v, want 90m", got.Steps[2].Stay)
	}

	// lodging: 最安の予約可能プランが選ばれる。
	l := got.Steps[4].Lodging
	if l == nil || l.Hotel != f.hotel || l.Plan.TotalPriceJPY != 16200 {
		t.Errorf("宿泊の選択が違います: %+v", l)
	}

	// 文章は narrative にだけ入る。
	if got.Narrative.Title != "終演の余韻を、水道橋の深夜に持ち帰る" {
		t.Errorf("プランの文章が移っていません: %+v", got.Narrative)
	}
	if got.Steps[2].Narrative.Headline != "深夜まで開いている居酒屋へ" {
		t.Errorf("ステップの文章が移っていません: %+v", got.Steps[2].Narrative)
	}
}

func TestHydrateRejectsHallucinatedCandidate(t *testing.T) {
	// **この検査が通らなくなったら、幻覚がそのままユーザーに出る。**
	f := newFixture(t)
	d := f.draft()
	d.Steps[2].CandidateID = sp("cand_dining_042")

	_, err := NewHydrator(f.store, nil).Hydrate(d, f.cond)
	if err == nil {
		t.Fatal("提示していない候補 ID を通しました")
	}
	e := apperror.From(err)
	if e.Code != apperror.CodeUnknownCandidate {
		t.Errorf("Code = %q, want %q", e.Code, apperror.CodeUnknownCandidate)
	}
	if !e.Repairable() {
		t.Error("組み直しで回復しうると判定されるべきです")
	}
}

func TestHydrateRejectsMissingRoute(t *testing.T) {
	// 直線距離からの推定で埋めない。事実と推定が混ざるほうが害が大きい。
	f := newFixture(t)
	d := f.draft()
	// 居酒屋を経路の無いラーメン店に差し替える。
	d.Steps[2].CandidateID = sp(string(f.ramen.ID))
	d.UnusedCandidateNotes[0].CandidateID = string(f.izakay.ID)

	_, err := NewHydrator(f.store, nil).Hydrate(d, f.cond)
	if err == nil {
		t.Fatal("経路が無いのにプランが組めてしまいました")
	}
	e := apperror.From(err)
	if e.Code != apperror.CodeTimelineInvalid {
		t.Errorf("Code = %q, want %q", e.Code, apperror.CodeTimelineInvalid)
	}
	if !e.Repairable() {
		t.Error("別候補を選び直せば回復しうるので Repairable であるべきです")
	}
}

func TestHydratePrefersWalkWithinLimit(t *testing.T) {
	f := newFixture(t)
	// 会場 → 居酒屋に、徒歩より速い公共交通を足す。
	mustPutRoute(t, f.store, model.RouteOriginVenue, string(f.izakay.ID), model.TravelTransit, 900, 4*time.Minute)

	got, err := NewHydrator(f.store, nil).Hydrate(f.draft(), f.cond)
	if err != nil {
		t.Fatal(err)
	}
	// 徒歩 8 分は上限 15 分に収まるので、乗り換えより徒歩を採る。
	if got.Steps[1].Route.Key.Mode != model.TravelWalk {
		t.Errorf("移動手段 = %q, want WALK", got.Steps[1].Route.Key.Mode)
	}

	// 徒歩の上限を厳しくすると、最速の手段に切り替わる。
	f.cond.Situation.MaxWalk = 5 * time.Minute
	got, err = NewHydrator(f.store, nil).Hydrate(f.draft(), f.cond)
	if err != nil {
		t.Fatal(err)
	}
	if got.Steps[1].Route.Key.Mode != model.TravelTransit {
		t.Errorf("移動手段 = %q, want TRANSIT", got.Steps[1].Route.Key.Mode)
	}
}

func TestHydrateFindsDestinationAcrossBuffer(t *testing.T) {
	// move → buffer → dining のように待機を挟んでも行き先を見失わない。
	f := newFixture(t)
	d := f.draft()
	d.Steps = []llm.DraftStep{
		{Order: 1, Kind: model.SegmentMove, StayMinutes: 0, Headline: "水道橋方面へ"},
		{Order: 2, Kind: model.SegmentBuffer, StayMinutes: 10, Headline: "少し時間を潰す"},
		{Order: 3, Kind: model.SegmentDining, CandidateID: sp(string(f.izakay.ID)), StayMinutes: 90,
			Headline: "居酒屋へ"},
	}
	// 先頭 move は下書きの検査では弾かれるので、その分だけ緩めて hydrator 単体を見る。
	d.Steps = append([]llm.DraftStep{
		{Order: 0, Kind: model.SegmentBuffer, StayMinutes: 5, Headline: "余韻"}}, d.Steps...)
	for i := range d.Steps {
		d.Steps[i].Order = i + 1
	}
	d.UnusedCandidateNotes = nil

	got, err := NewHydrator(f.store, nil).Hydrate(d, f.cond)
	if err != nil {
		t.Fatalf("待機を挟んだ移動を解決できません: %v", err)
	}
	if got.Steps[1].Route == nil || got.Steps[1].Route.DistanceMeters != 650 {
		t.Errorf("行き先を見失っています: %+v", got.Steps[1].Route)
	}
	// 2 つ目の待機は規制退場ではない。
	if got.Steps[2].Buffer.Kind != model.BufferSpareTime {
		t.Errorf("待機の種別 = %q, want spare_time", got.Steps[2].Buffer.Kind)
	}
	// 移動した後の待機なので、場所は移動先であって会場ではない。
	if got.Steps[2].Buffer.AtPlaceName != f.izakay.Name {
		t.Errorf("待機の場所 = %q, want %q", got.Steps[2].Buffer.AtPlaceName, f.izakay.Name)
	}
}

func TestHydrateBuildsLinks(t *testing.T) {
	f := newFixture(t)
	got, err := NewHydrator(f.store, nil).Hydrate(f.draft(), f.cond)
	if err != nil {
		t.Fatal(err)
	}
	links := got.Steps[2].Links
	if len(links) != 2 {
		t.Fatalf("リンク数 = %d, want 2（電話・地図）", len(links))
	}
	if links[0].Kind != model.LinkTel || links[0].URL != "tel:+81312345678" {
		t.Errorf("電話リンクが違います: %+v", links[0])
	}
	if links[1].Kind != model.LinkMap || !strings.Contains(links[1].URL, "query_place_id=ChIJizakaya") {
		t.Errorf("地図リンクが違います: %+v", links[1])
	}
	// 既定の組み立てはアフィリエイトを含まないため、計測 ID 無しでも契約違反にならない。
	for _, l := range links {
		if err := l.Validate(); err != nil {
			t.Errorf("不正なリンクが生成されました: %v", err)
		}
	}
}

func TestHydrateCollectsAlternativesAndWarnings(t *testing.T) {
	f := newFixture(t)
	got, err := NewHydrator(f.store, nil).Hydrate(f.draft(), f.cond)
	if err != nil {
		t.Fatal(err)
	}

	// 採用しなかった候補が理由付きで差し替え候補に並ぶ。
	if len(got.Alternatives.Dining) != 1 {
		t.Fatalf("差し替え候補 = %d 件, want 1", len(got.Alternatives.Dining))
	}
	alt := got.Alternatives.Dining[0]
	if alt.Place != f.ramen {
		t.Error("差し替え候補が違います")
	}
	if !strings.Contains(alt.ExcludedReason, "23時閉店") {
		t.Errorf("不採用理由が引き継がれていません: %q", alt.ExcludedReason)
	}
	// 採用済みの候補は差し替え候補に出ない。
	for _, a := range got.Alternatives.Dining {
		if a.Place == f.izakay {
			t.Error("採用済みの候補が差し替え候補に混ざっています")
		}
	}
	if len(got.Alternatives.Lodging) != 0 {
		t.Errorf("宿泊の差し替え候補 = %d 件, want 0", len(got.Alternatives.Lodging))
	}

	// 残室 2 室は先に伝える。
	if len(got.Warnings) != 1 || got.Warnings[0].Code != model.WarnVacancyLow {
		t.Fatalf("警告が出ていません: %+v", got.Warnings)
	}
	if !strings.Contains(got.Warnings[0].Message, "2室") {
		t.Errorf("残室数が伝わっていません: %q", got.Warnings[0].Message)
	}
}

func TestHydrateRejectsSoldOutHotel(t *testing.T) {
	f := newFixture(t)
	for i := range f.hotel.Plans {
		f.hotel.Plans[i].VacancyStatus = model.VacancySoldOut
	}

	_, err := NewHydrator(f.store, nil).Hydrate(f.draft(), f.cond)
	if err == nil {
		t.Fatal("満室の宿でプランが組めてしまいました")
	}
	if got := apperror.From(err).Code; got != apperror.CodeTimelineInvalid {
		t.Errorf("Code = %q, want %q", got, apperror.CodeTimelineInvalid)
	}
	// 満室の宿は差し替え候補にも出さない。
	alt := NewHydrator(f.store, nil).alternatives(&llm.PlanDraft{})
	if len(alt.Lodging) != 0 {
		t.Errorf("満室の宿が差し替え候補に出ています: %+v", alt.Lodging)
	}
}

func TestHydrateRejectsInvalidDraft(t *testing.T) {
	// 構造が壊れた下書きは、事実を引く前に落とす。
	f := newFixture(t)
	d := f.draft()
	d.Steps[2].CandidateID = nil // dining なのに候補なし

	_, err := NewHydrator(f.store, nil).Hydrate(d, f.cond)
	if err == nil {
		t.Fatal("構造の壊れた下書きを通しました")
	}
	if got := apperror.From(err).Code; got != apperror.CodeLLMInvalidOutput {
		t.Errorf("Code = %q, want %q", got, apperror.CodeLLMInvalidOutput)
	}
}
