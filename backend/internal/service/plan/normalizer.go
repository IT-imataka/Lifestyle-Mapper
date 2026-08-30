package plan

import (
	"strings"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/googleplaces"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/googleroutes"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/rakutentravel"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// このファイルは各社の生レスポンスをドメインの事実に訳す。
//
// **訳せなかったものは捨てる**のが方針。座標が無い店、名前が無いホテル、
// 経路が求まらなかった区間は、後段で必ず破綻するので候補に入れない。
// 「一応入れておいて後で弾く」をやると、破綻の原因がパイプラインの奥で
// 顔を出して切り分けが効かなくなる。

// ── 飲食・スポット ────────────────────────────────────

// normalizePlace は Places の 1 件を PlaceFact に訳す。
// 訳せなければ false を返す（候補として成立しない）。
func normalizePlace(p googleplaces.Place, cat model.CandidateCategory,
	stayDate time.Time, now time.Time) (*model.PlaceFact, bool) {

	loc := p.Location.Model()
	if p.ID == "" || p.DisplayName.Text == "" || !loc.Valid() {
		return nil, false
	}

	f := &model.PlaceFact{
		Cat:             cat,
		ProviderPlaceID: p.ID,
		Name:            p.DisplayName.Text,
		Genres:          toGenreWords(p.Types),
		Rating:          p.Rating,
		UserRatingCount: p.UserRatingCount,
		Address:         p.FormattedAddress,
		Loc:             loc,
		PhoneNumber:     p.NationalPhoneNumber,
		Hours:           resolveOpeningHours(p.RegularOpeningHours, stayDate),
		FetchedAt:       now,
	}
	if level, ok := parsePriceLevel(p.PriceLevel); ok {
		f.PriceLevel = &level
		if cost, ok := estimateCostPerPerson(level); ok {
			f.EstimatedCostPerPersonJPY = &cost
		}
	}
	if len(p.Photos) > 0 {
		f.PhotoRef = p.Photos[0].Name
	}
	return f, true
}

// placeTypeByGenre はドメインのジャンルを Places の type に写す。
//
// Places の type は 1 対 1 に対応しない（居酒屋という type は無い）ので、
// 検索時は近いものに寄せ、結果側の分類は toGenreWords が担う。
// **検索の網は広く、絞り込みは手元で**という方針。
var placeTypeByGenre = map[model.DiningGenre][]string{
	model.GenreIzakaya:          {"bar", "japanese_restaurant"},
	model.GenreRamen:            {"ramen_restaurant"},
	model.GenreYakiniku:         {"barbecue_restaurant", "korean_restaurant"},
	model.GenreSushi:            {"sushi_restaurant"},
	model.GenreCafe:             {"cafe", "coffee_shop"},
	model.GenreBar:              {"bar", "pub"},
	model.GenreFamilyRestaurant: {"diner", "family_restaurant"},
	model.GenreFastFood:         {"fast_food_restaurant"},
	model.GenreItalian:          {"italian_restaurant"},
	model.GenreChinese:          {"chinese_restaurant"},
}

// genreByPlaceType は逆向きの写像。結果の types からジャンル語を起こす。
var genreByPlaceType = map[string]model.DiningGenre{
	"ramen_restaurant":     model.GenreRamen,
	"sushi_restaurant":     model.GenreSushi,
	"barbecue_restaurant":  model.GenreYakiniku,
	"korean_restaurant":    model.GenreYakiniku,
	"cafe":                 model.GenreCafe,
	"coffee_shop":          model.GenreCafe,
	"bar":                  model.GenreBar,
	"pub":                  model.GenreIzakaya,
	"diner":                model.GenreFamilyRestaurant,
	"family_restaurant":    model.GenreFamilyRestaurant,
	"fast_food_restaurant": model.GenreFastFood,
	"italian_restaurant":   model.GenreItalian,
	"chinese_restaurant":   model.GenreChinese,
	"japanese_restaurant":  model.GenreIzakaya,
}

// PlaceTypesFor は検索に投げる Places の type 一覧を返す。
// ジャンル未指定なら restaurant に寄せる（深夜に開いている飲食全般）。
func PlaceTypesFor(genres []model.DiningGenre) []string {
	if len(genres) == 0 {
		return []string{"restaurant"}
	}
	seen := make(map[string]struct{}, len(genres)*2)
	out := make([]string, 0, len(genres)*2)
	for _, g := range genres {
		for _, t := range placeTypeByGenre[g] {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return []string{"restaurant"}
	}
	return out
}

// toGenreWords は Places の types を自前の分類語に写す。
// 対応するものが無ければ空を返す。**未知の type を素通しさせない**のは、
// LLM のプロンプトに提供元の内部語彙が混ざるのを防ぐため。
func toGenreWords(types []string) []string {
	seen := make(map[model.DiningGenre]struct{}, len(types))
	out := make([]string, 0, len(types))
	for _, t := range types {
		g, ok := genreByPlaceType[t]
		if !ok {
			continue
		}
		if _, dup := seen[g]; dup {
			continue
		}
		seen[g] = struct{}{}
		out = append(out, string(g))
	}
	return out
}

// priceLevelValue は Places の価格帯を 0〜4 の整数に写す。
var priceLevelValue = map[string]int{
	"PRICE_LEVEL_FREE":           0,
	"PRICE_LEVEL_INEXPENSIVE":    1,
	"PRICE_LEVEL_MODERATE":       2,
	"PRICE_LEVEL_EXPENSIVE":      3,
	"PRICE_LEVEL_VERY_EXPENSIVE": 4,
}

func parsePriceLevel(s string) (int, bool) {
	v, ok := priceLevelValue[s]
	return v, ok
}

// costByPriceLevel は価格帯から 1 人あたりの目安額（円）を起こす。
//
// **これは推定値**。Places は実額を返さないので、予算での足切りに使う目安でしかない。
// UI でも「目安」と明示する前提で、ここでは日本の夜の飲食相場に寄せている。
var costByPriceLevel = map[int]int{
	1: 1500,
	2: 3500,
	3: 7000,
	4: 15000,
}

func estimateCostPerPerson(level int) (int, bool) {
	v, ok := costByPriceLevel[level]
	return v, ok
}

// resolveOpeningHours は曜日繰り返しの営業時間を、対象日の絶対時刻に解決する。
//
// 対象は「公演当日の夜から翌朝まで」なので、前日・当日・翌日の 3 日ぶんを
// 展開する。前日を含めるのは、**前日 17 時開店・当日 2 時閉店**の区間が
// 当日 0 時台の来店を支えているため。ここを落とすと深夜営業が全滅する。
func resolveOpeningHours(h *googleplaces.OpeningHours, stayDate time.Time) model.OpeningHours {
	if h == nil || len(h.Periods) == 0 {
		// 提供元が返さなかっただけで、終日休業ではない。Known=false で「不明」を表す。
		return model.OpeningHours{}
	}
	base := startOfDay(stayDate)

	for _, p := range h.Periods {
		if p.Close == nil {
			// 閉店点の無い区間は 24 時間営業。前後を含めて開いているとみなす。
			return model.OpeningHours{Known: true, Periods: []model.OpeningPeriod{{
				Open:  base.AddDate(0, 0, -1),
				Close: base.AddDate(0, 0, 2),
			}}}
		}
	}

	out := make([]model.OpeningPeriod, 0, len(h.Periods))
	for offset := -1; offset <= 1; offset++ {
		day := base.AddDate(0, 0, offset)
		for _, p := range h.Periods {
			if int(day.Weekday()) != p.Open.Day {
				continue
			}
			open := day.Add(time.Duration(p.Open.Hour)*time.Hour + time.Duration(p.Open.Minute)*time.Minute)
			closeAt := closeAfter(open, day, *p.Close)
			out = append(out, model.OpeningPeriod{Open: open, Close: closeAt})
		}
	}
	if len(out) == 0 {
		return model.OpeningHours{}
	}
	return model.OpeningHours{Known: true, Periods: out}
}

// closeAfter は開店時刻より後になる最初の閉店時刻を求める。
// 閉店の曜日が開店の翌日以降にずれる深夜営業を、日数差として素直に扱う。
func closeAfter(open, openDay time.Time, c googleplaces.TimePoint) time.Time {
	diff := (c.Day - int(openDay.Weekday()) + 7) % 7
	closeAt := openDay.AddDate(0, 0, diff).
		Add(time.Duration(c.Hour)*time.Hour + time.Duration(c.Minute)*time.Minute)
	if !closeAt.After(open) {
		// 同日表記のまま開店を追い越せない場合（例: 開 17:00 / 閉 02:00）は翌日扱い。
		closeAt = closeAt.AddDate(0, 0, 1)
	}
	return closeAt
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// ── 宿泊 ──────────────────────────────────────────────

// normalizeHotel は楽天の 1 施設を HotelFact に訳す。
// 空きプランが 1 つも無い施設は候補にしない（提示できない宿を並べない）。
func normalizeHotel(h rakutentravel.Hotel, now time.Time) (*model.HotelFact, bool) {
	loc := h.Basic.Location()
	if h.Basic.HotelNo == 0 || h.Basic.HotelName == "" || !loc.Valid() {
		return nil, false
	}

	plans := make([]model.HotelPlanFact, 0, len(h.Rooms))
	for _, room := range h.Rooms {
		p, ok := normalizeHotelPlan(room, h.Detail)
		if !ok {
			continue
		}
		plans = append(plans, p)
	}
	if len(plans) == 0 {
		return nil, false
	}

	f := &model.HotelFact{
		ProviderHotelID: h.Basic.ProviderID(),
		Name:            h.Basic.HotelName,
		Address:         h.Basic.Address(),
		Loc:             loc,
		PhotoURL:        h.Basic.HotelImageURL,
		InformationURL:  informationURL(h.Basic),
		Plans:           plans,
		FetchedAt:       now,
	}
	// レビューは 0 件のとき 0.0 が返る。「評価 0 の宿」と誤読させないため落とす。
	if h.Basic.ReviewCount > 0 {
		avg, count := h.Basic.ReviewAverage, h.Basic.ReviewCount
		f.ReviewAverage, f.ReviewCount = &avg, &count
	}
	return f, true
}

// informationURL は施設ページを選ぶ。プラン一覧のほうが予約に近いので優先する。
func informationURL(b rakutentravel.HotelBasicInfo) string {
	if b.PlanListURL != "" {
		return b.PlanListURL
	}
	return b.HotelInformationURL
}

func normalizeHotelPlan(room rakutentravel.Room, detail *rakutentravel.HotelDetailInfo) (model.HotelPlanFact, bool) {
	total := room.TotalJPY()
	if room.Basic.PlanID == 0 || total <= 0 {
		// 料金の出ないプランは提示できない。価格未定のカードは予約導線にならない。
		return model.HotelPlanFact{}, false
	}

	p := model.HotelPlanFact{
		ProviderPlanID: room.Basic.ProviderPlanID(),
		PlanName:       room.Basic.PlanName,
		RoomName:       room.Basic.RoomName,
		TotalPriceJPY:  total,
		VacancyStatus:  vacancyOf(room.Basic),
		ReserveURL:     room.Basic.ReserveURL,
		Amenities:      amenitiesOf(room.Basic),
	}
	if per, ok := room.PerPersonJPY(); ok {
		p.PricePerPersonJPY = &per
	}
	if detail != nil {
		p.CheckInTime = parseClock(detail.CheckinTime)
		p.CheckOutTime = parseClock(detail.CheckoutTime)
		// 最終チェックイン時刻は "26:00" のような 24 時超え表記で来る。
		// **終演が遅い公演では、この 1 項目がプランの成否を分ける**。
		p.CheckInDeadline = parseClock(detail.LastCheckInTime)
	}
	return p, true
}

// vacancyOf は空室状況を決める。楽天は残室数を返さないため、
// 「残りわずか」フラグの有無だけを翻訳し、数は作らない。
func vacancyOf(b rakutentravel.RoomBasicInfo) model.VacancyStatus {
	if b.SalesformFlag == 1 {
		return model.VacancyFewLeft
	}
	// 検索が空室検索なので、返ってきた時点で空きはある。
	return model.VacancyAvailable
}

// amenitiesOf はプランのフラグをアメニティ語に写す。
// 楽天が返すのは食事の有無くらいなので、分かるものだけを起こす。
func amenitiesOf(b rakutentravel.RoomBasicInfo) []string {
	var out []string
	if b.WithBreakfastFlag == 1 {
		out = append(out, string(model.LodgingBreakfastIncluded))
	}
	if strings.Contains(b.RoomName, "禁煙") || strings.Contains(b.PlanName, "禁煙") {
		out = append(out, string(model.LodgingNoSmoking))
	}
	return out
}

func parseClock(s string) *model.ClockTime {
	c, ok := model.ParseClockTime(s)
	if !ok {
		return nil
	}
	return &c
}

// ── 経路 ──────────────────────────────────────────────

// normalizeMatrixElement は行列の 1 要素を RouteFact に訳す。
// 経路が求まらなかった要素は false（そこへは行けない、という事実）。
func normalizeMatrixElement(el googleroutes.MatrixElement, key model.RouteKey,
	from, to model.Waypoint, now time.Time) (*model.RouteFact, bool) {

	if !el.OK() {
		return nil, false
	}
	d, err := el.TravelDuration()
	if err != nil {
		return nil, false
	}
	return &model.RouteFact{
		Key:            key,
		From:           from,
		To:             to,
		DistanceMeters: el.DistanceMeters,
		Duration:       d,
		// 行列は所要時間と距離しか返さない。経路線と乗換は
		// 採用が決まった区間だけ computeRoutes で埋める（課金の都合）。
		MapsURL:   googleroutes.DirectionsURL(from, to, key.Mode),
		FetchedAt: now,
	}, true
}
