package plan

import (
	"sort"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// このファイルは正規化済みの候補を **足切りしてから並べ替える**。
//
// 順序が大事で、先に足切りする。行けない店・入れない店・予算外の宿を
// スコアで下位に沈めるだけにすると、候補が少ない夜に「徒歩 40 分の店」が
// 繰り上がって提示される。**出してはいけないものは、点数ではなく門で止める**。
//
// スコアは LLM に渡す順序を決めるためだけのもので、UI には出ない。
// 「なぜこの店なのか」を語るのは LLM の仕事で、点数を見せることではない。

// scoreWeights は評価軸の重み。合計 1.0 に揃えてある。
//
// 近さを最大にしているのは、終演後の深夜に**歩く距離が体験を決める**ため。
// 評価の高さより「そこまで歩けるか」が先に効く。
var scoreWeights = struct {
	Proximity float64 // 近さ（徒歩時間）
	Rating    float64 // 評価
	Budget    float64 // 予算への収まり
	Fit       float64 // 条件（ジャンル・要件）への一致
}{
	Proximity: 0.40,
	Rating:    0.25,
	Budget:    0.20,
	Fit:       0.15,
}

// scoredPlace は採点済みの飲食候補。
type scoredPlace struct {
	fact  *model.PlaceFact
	route *model.RouteFact
	score float64
}

// scoredHotel は採点済みの宿泊候補。
type scoredHotel struct {
	fact  *model.HotelFact
	plan  *model.HotelPlanFact
	route *model.RouteFact
	score float64
}

// routeLookup は候補（正規化時点ではまだ ID を持たない）から会場までの経路を引く。
// 添字で引くのは、FactStore の採番が **採点の後**に来るため。
type routeLookup func(index int) (*model.RouteFact, bool)

// scorePlaces は飲食候補を足切りして並べ替え、上位 limit 件を返す。
//
// arrival は会場を出て店に着くまでの見込み時刻の基準。実際の到着時刻は
// 経路ごとに違うので、候補ごとに「退場時刻 + その候補への移動時間」で判定する。
func scorePlaces(facts []*model.PlaceFact, routes routeLookup,
	cond *model.SearchCondition, limit int) []scoredPlace {

	departure := cond.Event.DepartureAt()
	out := make([]scoredPlace, 0, len(facts))

	for i, f := range facts {
		route, ok := routes(i)
		if !ok {
			continue // 経路が引けない＝そこへは行けない
		}
		if !route.WithinWalkLimit(cond.Situation.MaxWalk) {
			continue // 歩ける距離を超えている
		}

		arrival := departure.Add(route.Duration)
		// 営業時間が **不明な店は落とさない**。Places が返さないだけのことがあり、
		// ここで落とすと個人店ばかりの深夜帯で候補が消える。判定は validator が
		// 確定時刻に対してもう一度行う。
		if f.Hours.Known && !f.Hours.CanStay(arrival, cond.Dining.DesiredStay) {
			continue
		}
		if !withinDiningBudget(f, cond) {
			continue
		}

		out = append(out, scoredPlace{
			fact:  f,
			route: route,
			score: placeScore(f, route, cond),
		})
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return truncatePlaces(out, limit)
}

// withinDiningBudget は 1 人あたりの目安額が予算に収まるかを見る。
// 目安額が取れない店は落とさない。**推定できないことを理由に候補を消さない**。
func withinDiningBudget(f *model.PlaceFact, cond *model.SearchCondition) bool {
	if f.EstimatedCostPerPersonJPY == nil {
		return true
	}
	return cond.Dining.BudgetPerPerson.Contains(*f.EstimatedCostPerPersonJPY)
}

func placeScore(f *model.PlaceFact, route *model.RouteFact, cond *model.SearchCondition) float64 {
	s := scoreWeights.Proximity * proximityScore(route.Duration, cond.Situation.MaxWalk)
	s += scoreWeights.Rating * ratingScore(f.Rating, f.UserRatingCount)
	s += scoreWeights.Budget * diningBudgetScore(f, cond)
	s += scoreWeights.Fit * diningFitScore(f, cond)
	return s
}

// proximityScore は近いほど 1 に寄る。上限ちょうどで 0。
func proximityScore(d, limit time.Duration) float64 {
	if limit <= 0 || d <= 0 {
		return 1
	}
	if d >= limit {
		return 0
	}
	return 1 - float64(d)/float64(limit)
}

// ratingScore は評価を 0〜1 に写す。
//
// 件数で減衰させるのは、**5.0（レビュー 2 件）を 4.2（800 件）より上に置かない**ため。
// 深夜に開いている個人店はレビューが薄く、素の平均では信頼できない。
func ratingScore(rating *float64, count *int) float64 {
	if rating == nil || *rating <= 0 {
		return 0.5 // 不明は中央。無評価というだけで沈めない
	}
	normalized := (*rating - 3.0) / 2.0 // 3.0 未満は 0、5.0 で 1
	switch {
	case normalized < 0:
		normalized = 0
	case normalized > 1:
		normalized = 1
	}
	return normalized * confidence(count)
}

// confidence はレビュー件数による信頼度。100 件で概ね飽和する。
func confidence(count *int) float64 {
	if count == nil || *count <= 0 {
		return 0.5
	}
	n := float64(*count)
	return 0.5 + 0.5*n/(n+100)
}

// diningBudgetScore は予算帯の中心に近いほど高い。
// 上限ぎりぎりより、少し余裕のある額のほうが「良い夜」になりやすい。
func diningBudgetScore(f *model.PlaceFact, cond *model.SearchCondition) float64 {
	if f.EstimatedCostPerPersonJPY == nil {
		return 0.5
	}
	b := cond.Dining.BudgetPerPerson
	if !b.HasMax() {
		return 0.5
	}
	span := float64(*b.Max - b.Min)
	if span <= 0 {
		return 1
	}
	center := float64(b.Min) + span/2
	diff := float64(*f.EstimatedCostPerPersonJPY) - center
	if diff < 0 {
		diff = -diff
	}
	score := 1 - diff/(span/2)
	if score < 0 {
		return 0
	}
	return score
}

// diningFitScore は希望ジャンルへの一致度。要件（個室・喫煙）は
// Places が返さないため見ない。**取れない情報で点を付けない**。
func diningFitScore(f *model.PlaceFact, cond *model.SearchCondition) float64 {
	if len(cond.Dining.Genres) == 0 {
		return 0.5
	}
	for _, g := range cond.Dining.Genres {
		if f.HasGenre(string(g)) {
			return 1
		}
	}
	return 0
}

func truncatePlaces(list []scoredPlace, limit int) []scoredPlace {
	if limit > 0 && len(list) > limit {
		return list[:limit]
	}
	return list
}

// scoreHotels は宿泊候補を足切りして並べ替える。
//
// 飲食と違い **最終チェックイン時刻**という固い制約がある。終演が 21 時、
// 食事が 90 分なら宿に着くのは 23 時半以降で、23 時締切の宿は成立しない。
// earliestArrival はその見込み到着時刻。
func scoreHotels(facts []*model.HotelFact, routes routeLookup,
	cond *model.SearchCondition, earliestArrival time.Time, limit int) []scoredHotel {

	out := make([]scoredHotel, 0, len(facts))

	for i, f := range facts {
		route, ok := routes(i)
		if !ok {
			continue
		}
		if !route.WithinWalkLimit(cond.Situation.MaxWalk) {
			continue
		}
		roomPlan := pickHotelPlan(f, cond, earliestArrival)
		if roomPlan == nil {
			continue
		}
		out = append(out, scoredHotel{
			fact:  f,
			plan:  roomPlan,
			route: route,
			score: hotelScore(f, roomPlan, route, cond),
		})
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// pickHotelPlan は予算内で間に合う最安プランを選ぶ。
//
// 最安を採るのは、同じ宿の中では価格しか差が無いことが多いため。
// **チェックイン締切に間に合わないプランは価格に関わらず捨てる**。
func pickHotelPlan(f *model.HotelFact, cond *model.SearchCondition, arrival time.Time) *model.HotelPlanFact {
	var best *model.HotelPlanFact
	for i := range f.Plans {
		p := &f.Plans[i]
		if p.VacancyStatus == model.VacancySoldOut {
			continue
		}
		if !cond.Lodging.BudgetPerNight.Contains(p.TotalPriceJPY) {
			continue
		}
		if !checkInFeasible(p, cond, arrival) {
			continue
		}
		if best == nil || p.TotalPriceJPY < best.TotalPriceJPY {
			best = p
		}
	}
	return best
}

// checkInFeasible は到着見込み時刻までにチェックインできるかを返す。
// 締切が不明なプランは落とさない（不明を不可と読み替えない）。
func checkInFeasible(p *model.HotelPlanFact, cond *model.SearchCondition, arrival time.Time) bool {
	if arrival.IsZero() {
		return true
	}
	stayDate := cond.Lodging.CheckIn
	if stayDate.IsZero() {
		stayDate = cond.Event.EndsAt
	}
	deadline, ok := p.CheckInDeadlineAt(stayDate)
	if !ok {
		return true
	}
	return !arrival.After(deadline)
}

func hotelScore(f *model.HotelFact, p *model.HotelPlanFact,
	route *model.RouteFact, cond *model.SearchCondition) float64 {

	s := scoreWeights.Proximity * proximityScore(route.Duration, cond.Situation.MaxWalk)
	s += scoreWeights.Rating * ratingScore(f.ReviewAverage, f.ReviewCount)
	s += scoreWeights.Budget * lodgingBudgetScore(p, cond)
	s += scoreWeights.Fit * lodgingFitScore(p, cond)
	return s
}

// lodgingBudgetScore は安いほど高い。飲食と違い「中心が良い」ではないのは、
// 宿は寝るだけで、浮いた額が翌日の体験に回るため。
func lodgingBudgetScore(p *model.HotelPlanFact, cond *model.SearchCondition) float64 {
	b := cond.Lodging.BudgetPerNight
	if !b.HasMax() || *b.Max <= b.Min {
		return 0.5
	}
	span := float64(*b.Max - b.Min)
	score := 1 - float64(p.TotalPriceJPY-b.Min)/span
	switch {
	case score < 0:
		return 0
	case score > 1:
		return 1
	}
	return score
}

// lodgingFitScore は希望した設備をどれだけ満たすか。
func lodgingFitScore(p *model.HotelPlanFact, cond *model.SearchCondition) float64 {
	if len(cond.Lodging.Requirements) == 0 {
		return 0.5
	}
	matched := 0
	for _, r := range cond.Lodging.Requirements {
		// late_checkin_ok は締切時刻そのもので判定済みなので、ここでは満たしたとみなす。
		if r == model.LodgingLateCheckinOK && p.CheckInDeadline != nil {
			matched++
			continue
		}
		if p.HasAmenity(string(r)) {
			matched++
		}
	}
	return float64(matched) / float64(len(cond.Lodging.Requirements))
}
