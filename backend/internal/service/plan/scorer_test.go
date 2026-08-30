package plan

import (
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// scoreCond は採点用の検索条件。終演 21:00 / 退場 30 分 / 徒歩上限 15 分。
func scoreCond(t *testing.T) *model.SearchCondition {
	t.Helper()
	cond := &model.SearchCondition{
		Event: model.Event{
			Name:       "テスト公演",
			Venue:      model.Venue{Name: "東京ドーム", Location: model.Location{Lat: 35.7056, Lng: 139.7519}},
			EndsAt:     jst(t, "2026-09-05 21:00"),
			ExitBuffer: 30 * time.Minute,
		},
		Party:     model.Party{Adults: 2, Relationship: model.RelationshipFriends},
		Situation: model.Situation{Intent: model.IntentStay, MaxWalk: 15 * time.Minute},
		Dining: model.DiningPreference{
			Enabled:         true,
			BudgetPerPerson: model.MoneyRangeJPY{Min: 2000, Max: ip(6000)},
			DesiredStay:     90 * time.Minute,
		},
		Lodging: model.LodgingPreference{
			Enabled:        true,
			CheckIn:        jst(t, "2026-09-05 00:00"),
			CheckOut:       jst(t, "2026-09-06 00:00"),
			Rooms:          1,
			BudgetPerNight: model.MoneyRangeJPY{Min: 8000, Max: ip(20000)},
		},
		Context: model.RequestContext{Locale: "ja-JP", Currency: "JPY", Timezone: "Asia/Tokyo"},
	}
	return cond
}

// diningFact は採点対象の飲食候補を組み立てる。営業は 17:00〜翌 2:00。
func diningFact(t *testing.T, name string, costPerPerson int) *model.PlaceFact {
	t.Helper()
	f := &model.PlaceFact{
		Cat:             model.CategoryDining,
		ProviderPlaceID: "ChIJ" + name,
		Name:            name,
		Loc:             model.Location{Lat: 35.7061, Lng: 139.7522},
		Genres:          []string{string(model.GenreIzakaya)},
		Hours:           openUntil(t, "2026-09-05 17:00", "2026-09-06 02:00"),
	}
	if costPerPerson > 0 {
		f.EstimatedCostPerPersonJPY = &costPerPerson
	}
	return f
}

// walkRoutes は「全候補が同じ徒歩時間」の引き手を作る。
func walkRoutes(durations ...time.Duration) routeLookup {
	return func(i int) (*model.RouteFact, bool) {
		if i < 0 || i >= len(durations) {
			return nil, false
		}
		if durations[i] < 0 {
			return nil, false // 経路が引けなかった候補
		}
		return &model.RouteFact{
			Key:      model.NewRouteKey(model.RouteOriginVenue, "", model.TravelWalk),
			Duration: durations[i],
		}, true
	}
}

func names(list []scoredPlace) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.fact.Name)
	}
	return out
}

// ── 飲食の足切り ──────────────────────────────────────

func TestScorePlacesGatesBeforeScoring(t *testing.T) {
	// 出してはいけないものは点数ではなく門で止める。スコアで沈めるだけにすると、
	// 候補が少ない夜に「徒歩 40 分の店」が繰り上がって提示される。
	cond := scoreCond(t)

	tests := []struct {
		name     string
		fact     func() *model.PlaceFact
		duration time.Duration
		want     bool // 候補に残るか
	}{
		{
			"経路が引けない候補は落とす",
			func() *model.PlaceFact { return diningFact(t, "行けない店", 3500) },
			-1, false,
		},
		{
			"徒歩上限ちょうどは残す",
			func() *model.PlaceFact { return diningFact(t, "上限ちょうど", 3500) },
			15 * time.Minute, true,
		},
		{
			"徒歩上限超過は落とす",
			func() *model.PlaceFact { return diningFact(t, "遠い店", 3500) },
			16 * time.Minute, false,
		},
		{
			"予算超過は落とす",
			func() *model.PlaceFact { return diningFact(t, "高い店", 15000) },
			5 * time.Minute, false,
		},
		{
			// 推定できないことを理由に候補を消さない。
			"目安額が取れない店は残す",
			func() *model.PlaceFact { return diningFact(t, "価格不明", 0) },
			5 * time.Minute, true,
		},
		{
			// Places が営業時間を返さないだけのことがある。ここで落とすと
			// 個人店ばかりの深夜帯で候補が消える。判定は validator が確定時刻で行う。
			"営業時間が不明な店は残す",
			func() *model.PlaceFact {
				f := diningFact(t, "時間不明", 3500)
				f.Hours = model.OpeningHours{}
				return f
			},
			5 * time.Minute, true,
		},
		{
			// 21:30 退場 + 徒歩 5 分で 21:35 着。90 分居られない。
			"希望滞在時間ぶん居られない店は落とす",
			func() *model.PlaceFact {
				f := diningFact(t, "もう閉まる", 3500)
				f.Hours = openUntil(t, "2026-09-05 17:00", "2026-09-05 22:00")
				return f
			},
			5 * time.Minute, false,
		},
		{
			"閉店ちょうどまで居られる店は残す",
			func() *model.PlaceFact {
				f := diningFact(t, "ぎりぎり", 3500)
				f.Hours = openUntil(t, "2026-09-05 17:00", "2026-09-05 23:05")
				return f
			},
			5 * time.Minute, true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scorePlaces([]*model.PlaceFact{tt.fact()}, walkRoutes(tt.duration), cond, 8)
			if (len(got) == 1) != tt.want {
				t.Errorf("残った件数 = %d, 残すべき = %v", len(got), tt.want)
			}
		})
	}
}

func TestScorePlacesOrdersByProximityFirst(t *testing.T) {
	// 終演後の深夜は歩く距離が体験を決める。同条件なら近い順。
	cond := scoreCond(t)
	facts := []*model.PlaceFact{
		diningFact(t, "遠い", 3500),
		diningFact(t, "近い", 3500),
		diningFact(t, "中間", 3500),
	}
	got := scorePlaces(facts, walkRoutes(13*time.Minute, 3*time.Minute, 8*time.Minute), cond, 8)

	want := []string{"近い", "中間", "遠い"}
	for i, n := range names(got) {
		if n != want[i] {
			t.Fatalf("並び = %v, want %v", names(got), want)
		}
	}
}

func TestScorePlacesTruncatesToLimit(t *testing.T) {
	// limit は LLM 入力トークンの天井そのもの。超えて渡さない。
	cond := scoreCond(t)
	var facts []*model.PlaceFact
	var durations []time.Duration
	for i := range 10 {
		facts = append(facts, diningFact(t, string(rune('A'+i)), 3500))
		durations = append(durations, time.Duration(i+1)*time.Minute)
	}

	got := scorePlaces(facts, walkRoutes(durations...), cond, 3)
	if len(got) != 3 {
		t.Fatalf("件数 = %d, want 3", len(got))
	}
	// 打ち切っても上位から残る。
	if names(got)[0] != "A" {
		t.Errorf("先頭 = %q, want A", names(got)[0])
	}

	// limit が 0 以下なら打ち切らない。
	if got := scorePlaces(facts, walkRoutes(durations...), cond, 0); len(got) != 10 {
		t.Errorf("limit 0 で打ち切っています: %d 件", len(got))
	}
}

// ── スコアの意図 ──────────────────────────────────────

func TestRatingScoreDampensThinReviewCounts(t *testing.T) {
	// 5.0（2 件）を 4.2（800 件）より上に置かない。深夜に開いている個人店は
	// レビューが薄く、素の平均では信頼できない。
	thin := ratingScore(fp(5.0), ip(2))
	thick := ratingScore(fp(4.2), ip(800))
	if thin >= thick {
		t.Errorf("薄いレビューの 5.0 が厚い 4.2 を上回りました: %.4f >= %.4f", thin, thick)
	}
	// 無評価は中央。評価が無いというだけで沈めない。
	if got := ratingScore(nil, nil); got != 0.5 {
		t.Errorf("無評価のスコア = %.4f, want 0.5", got)
	}
	// 3.0 以下は下限に張り付く。
	if got := ratingScore(fp(2.5), ip(1000)); got != 0 {
		t.Errorf("低評価のスコア = %.4f, want 0", got)
	}
}

func TestProximityScoreEdges(t *testing.T) {
	tests := []struct {
		name  string
		d     time.Duration
		limit time.Duration
		want  float64
	}{
		{"上限ちょうどは 0", 15 * time.Minute, 15 * time.Minute, 0},
		{"上限超過も 0", 30 * time.Minute, 15 * time.Minute, 0},
		{"ゼロ距離は 1", 0, 15 * time.Minute, 1},
		{"上限未設定は 1", 8 * time.Minute, 0, 1},
		{"半分なら 0.5", 5 * time.Minute, 10 * time.Minute, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := proximityScore(tt.d, tt.limit); got != tt.want {
				t.Errorf("近さスコア = %.4f, want %.4f", got, tt.want)
			}
		})
	}
}

func TestDiningBudgetScorePeaksAtCenter(t *testing.T) {
	// 上限ぎりぎりより、少し余裕のある額のほうが「良い夜」になりやすい。
	cond := scoreCond(t) // 2000〜6000、中心 4000

	center := diningBudgetScore(diningFact(t, "中心", 4000), cond)
	edge := diningBudgetScore(diningFact(t, "上限", 6000), cond)
	if center <= edge {
		t.Errorf("中心 %.4f が上限 %.4f を上回っていません", center, edge)
	}
	if center != 1 {
		t.Errorf("中心のスコア = %.4f, want 1", center)
	}
	// 目安額が取れない店・上限が無い予算はどちらも中央に置く。
	if got := diningBudgetScore(diningFact(t, "不明", 0), cond); got != 0.5 {
		t.Errorf("目安額不明のスコア = %.4f, want 0.5", got)
	}
	cond.Dining.BudgetPerPerson = model.MoneyRangeJPY{Min: 2000}
	if got := diningBudgetScore(diningFact(t, "上限なし", 4000), cond); got != 0.5 {
		t.Errorf("上限なしのスコア = %.4f, want 0.5", got)
	}
}

func TestDiningFitScoreMatchesRequestedGenre(t *testing.T) {
	cond := scoreCond(t)
	izakaya := diningFact(t, "居酒屋", 3500)
	ramen := diningFact(t, "ラーメン", 1500)
	ramen.Genres = []string{string(model.GenreRamen)}

	// ジャンル未指定なら差を付けない。
	if got := diningFitScore(izakaya, cond); got != 0.5 {
		t.Errorf("ジャンル未指定のスコア = %.4f, want 0.5", got)
	}

	cond.Dining.Genres = []model.DiningGenre{model.GenreIzakaya}
	if got := diningFitScore(izakaya, cond); got != 1 {
		t.Errorf("一致時のスコア = %.4f, want 1", got)
	}
	if got := diningFitScore(ramen, cond); got != 0 {
		t.Errorf("不一致時のスコア = %.4f, want 0", got)
	}
}

// ── 宿泊 ──────────────────────────────────────────────

func hotelFact(name string, plans ...model.HotelPlanFact) *model.HotelFact {
	return &model.HotelFact{
		ProviderHotelID: name,
		Name:            name,
		Loc:             model.Location{Lat: 35.7060, Lng: 139.7530},
		Plans:           plans,
	}
}

func hotelPlan(id string, total int, deadline *model.ClockTime, amenities ...string) model.HotelPlanFact {
	return model.HotelPlanFact{
		ProviderPlanID:  id,
		PlanName:        "プラン " + id,
		TotalPriceJPY:   total,
		VacancyStatus:   model.VacancyAvailable,
		CheckInDeadline: deadline,
		Amenities:       amenities,
	}
}

func clockPtr(t *testing.T, s string) *model.ClockTime {
	t.Helper()
	c, ok := model.ParseClockTime(s)
	if !ok {
		t.Fatalf("時刻を解釈できません: %q", s)
	}
	return &c
}

func TestPickHotelPlanChoosesCheapestFeasible(t *testing.T) {
	cond := scoreCond(t) // 予算 8000〜20000
	arrival := jst(t, "2026-09-05 23:30")

	f := hotelFact("ホテル",
		hotelPlan("sold", 9000, clockPtr(t, "26:00")),
		hotelPlan("over", 25000, clockPtr(t, "26:00")),   // 予算超過
		hotelPlan("late", 12000, clockPtr(t, "23:00")),   // 締切に間に合わない
		hotelPlan("cheap", 14000, clockPtr(t, "26:00")),  // 採用されるべき
		hotelPlan("pricey", 18000, clockPtr(t, "26:00")), // 同条件だが高い
	)
	f.Plans[0].VacancyStatus = model.VacancySoldOut

	got := pickHotelPlan(f, cond, arrival)
	if got == nil {
		t.Fatal("プランが選ばれませんでした")
	}
	if got.ProviderPlanID != "cheap" {
		t.Errorf("採用プラン = %q, want cheap", got.ProviderPlanID)
	}
}

func TestPickHotelPlanKeepsUnknownDeadline(t *testing.T) {
	// 締切が不明なプランを不可と読み替えない。
	cond := scoreCond(t)
	arrival := jst(t, "2026-09-06 03:00")

	f := hotelFact("ホテル", hotelPlan("unknown", 12000, nil))
	if got := pickHotelPlan(f, cond, arrival); got == nil {
		t.Error("締切不明のプランを落としました")
	}

	// 締切が判っていて間に合わないなら落とす。
	f = hotelFact("ホテル", hotelPlan("late", 12000, clockPtr(t, "26:00")))
	if got := pickHotelPlan(f, cond, arrival); got != nil {
		t.Errorf("間に合わないプランを採用しました: %q", got.ProviderPlanID)
	}
}

func TestPickHotelPlanAcceptsDeadlineExactly(t *testing.T) {
	// 26:00 締切に翌 2:00 ちょうど着。到着が締切を「超えて」いなければ通す。
	cond := scoreCond(t)
	f := hotelFact("ホテル", hotelPlan("just", 12000, clockPtr(t, "26:00")))

	if got := pickHotelPlan(f, cond, jst(t, "2026-09-06 02:00")); got == nil {
		t.Error("締切ちょうどの到着を落としました")
	}
	if got := pickHotelPlan(f, cond, jst(t, "2026-09-06 02:01")); got != nil {
		t.Error("締切を 1 分過ぎた到着を通しました")
	}
}

func TestScoreHotelsDropsHotelWithoutFeasiblePlan(t *testing.T) {
	cond := scoreCond(t)
	arrival := jst(t, "2026-09-05 23:30")

	facts := []*model.HotelFact{
		hotelFact("間に合う", hotelPlan("ok", 12000, clockPtr(t, "26:00"))),
		hotelFact("締切前", hotelPlan("late", 12000, clockPtr(t, "23:00"))),
		hotelFact("経路なし", hotelPlan("ok", 12000, clockPtr(t, "26:00"))),
		hotelFact("遠い", hotelPlan("ok", 12000, clockPtr(t, "26:00"))),
	}
	got := scoreHotels(facts, walkRoutes(6*time.Minute, 6*time.Minute, -1, 20*time.Minute), cond, arrival, 8)

	if len(got) != 1 || got[0].fact.Name != "間に合う" {
		t.Fatalf("残った宿 = %v", got)
	}
	if got[0].plan == nil || got[0].plan.ProviderPlanID != "ok" {
		t.Errorf("採用プランが紐づいていません: %+v", got[0].plan)
	}
}

func TestLodgingBudgetScorePrefersCheaper(t *testing.T) {
	// 宿は寝るだけで、浮いた額が翌日の体験に回る。中心ではなく安いほうが高い。
	cond := scoreCond(t) // 8000〜20000

	cheap := hotelPlan("cheap", 9000, nil)
	pricey := hotelPlan("pricey", 19000, nil)
	if lodgingBudgetScore(&cheap, cond) <= lodgingBudgetScore(&pricey, cond) {
		t.Error("高いプランのほうが高評価になっています")
	}

	// 下限ちょうどで 1、上限ちょうどで 0。
	atMin := hotelPlan("min", 8000, nil)
	atMax := hotelPlan("max", 20000, nil)
	if got := lodgingBudgetScore(&atMin, cond); got != 1 {
		t.Errorf("下限のスコア = %.4f, want 1", got)
	}
	if got := lodgingBudgetScore(&atMax, cond); got != 0 {
		t.Errorf("上限のスコア = %.4f, want 0", got)
	}

	// 上限が無ければ差を付けられない。
	cond.Lodging.BudgetPerNight = model.MoneyRangeJPY{Min: 8000}
	if got := lodgingBudgetScore(&cheap, cond); got != 0.5 {
		t.Errorf("上限なしのスコア = %.4f, want 0.5", got)
	}
}

func TestLodgingFitScoreCountsMatchedRequirements(t *testing.T) {
	cond := scoreCond(t)

	// 要件未指定なら差を付けない。
	p := hotelPlan("p", 12000, nil)
	if got := lodgingFitScore(&p, cond); got != 0.5 {
		t.Errorf("要件未指定のスコア = %.4f, want 0.5", got)
	}

	cond.Lodging.Requirements = []model.LodgingRequirement{
		model.LodgingNoSmoking, model.LodgingBreakfastIncluded, model.LodgingLateCheckinOK,
	}
	// late_checkin_ok は締切時刻そのもので判定済み。締切が判っていれば満たしたとみなす。
	p = hotelPlan("p", 12000, clockPtr(t, "26:00"), string(model.LodgingNoSmoking))
	if got := lodgingFitScore(&p, cond); got != 2.0/3.0 {
		t.Errorf("一致度 = %.4f, want %.4f", got, 2.0/3.0)
	}

	full := hotelPlan("full", 12000, clockPtr(t, "26:00"),
		string(model.LodgingNoSmoking), string(model.LodgingBreakfastIncluded))
	if got := lodgingFitScore(&full, cond); got != 1 {
		t.Errorf("全一致のスコア = %.4f, want 1", got)
	}
}
