package plan

import (
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/googleplaces"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/googleroutes"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/rakutentravel"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// ── 飲食・スポット ────────────────────────────────────

func rawPlace() googleplaces.Place {
	return googleplaces.Place{
		ID:               "ChIJizakaya",
		DisplayName:      googleplaces.LocalizedText{Text: "居酒屋 ○○", LanguageCode: "ja"},
		FormattedAddress: "東京都文京区後楽1-3-61",
		Location:         googleplaces.LatLng{Latitude: 35.7061, Longitude: 139.7522},
		Types:            []string{"japanese_restaurant", "restaurant", "point_of_interest"},
		PriceLevel:       "PRICE_LEVEL_MODERATE",
	}
}

func TestNormalizePlaceRejectsIncompleteRecords(t *testing.T) {
	// 座標や名前を欠いた候補を通すと、地図にも時系列にも載せられないまま
	// パイプラインの奥で破綻する。入口で落とす。
	stay := jst(t, "2026-09-05 21:00")

	tests := []struct {
		name  string
		mutit func(*googleplaces.Place)
	}{
		{"ID が無い", func(p *googleplaces.Place) { p.ID = "" }},
		{"表示名が無い", func(p *googleplaces.Place) { p.DisplayName.Text = "" }},
		{"座標が未設定", func(p *googleplaces.Place) { p.Location = googleplaces.LatLng{} }},
		{"緯度が範囲外", func(p *googleplaces.Place) { p.Location.Latitude = 120 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := rawPlace()
			tt.mutit(&p)
			if _, ok := normalizePlace(p, model.CategoryDining, stay, stay); ok {
				t.Error("訳せないはずの候補が通りました")
			}
		})
	}
}

func TestNormalizePlaceMapsProviderVocabulary(t *testing.T) {
	stay := jst(t, "2026-09-05 21:00")
	now := jst(t, "2026-08-30 12:00")

	p := rawPlace()
	p.Rating = fp(4.3)
	p.UserRatingCount = ip(812)
	p.NationalPhoneNumber = "03-1234-5678"
	p.Photos = []googleplaces.Photo{{Name: "places/ChIJizakaya/photos/abc"}, {Name: "places/ChIJizakaya/photos/def"}}

	f, ok := normalizePlace(p, model.CategoryDining, stay, now)
	if !ok {
		t.Fatal("正規化できませんでした")
	}
	if f.Cat != model.CategoryDining || f.ProviderPlaceID != "ChIJizakaya" || f.Name != "居酒屋 ○○" {
		t.Errorf("基本項目が写っていません: %+v", f)
	}
	if f.Loc.Lat != 35.7061 || f.Loc.Lng != 139.7522 {
		t.Errorf("座標が写っていません: %+v", f.Loc)
	}
	// PRICE_LEVEL_MODERATE は 2、そこから 1 人 3500 円を推定する。
	if f.PriceLevel == nil || *f.PriceLevel != 2 {
		t.Errorf("価格帯 = %v, want 2", f.PriceLevel)
	}
	if f.EstimatedCostPerPersonJPY == nil || *f.EstimatedCostPerPersonJPY != 3500 {
		t.Errorf("目安額 = %v, want 3500", f.EstimatedCostPerPersonJPY)
	}
	// types は自前の分類語だけに絞る。提供元の内部語彙を LLM に流さない。
	if len(f.Genres) != 1 || f.Genres[0] != string(model.GenreIzakaya) {
		t.Errorf("ジャンル = %v, want [izakaya]", f.Genres)
	}
	// 写真は 1 枚目の参照名だけ。表示 URL はここで組まない（API キーが載るため）。
	if f.PhotoRef != "places/ChIJizakaya/photos/abc" {
		t.Errorf("写真参照 = %q", f.PhotoRef)
	}
	if !f.FetchedAt.Equal(now) {
		t.Errorf("取得時刻 = %v, want %v", f.FetchedAt, now)
	}
}

func TestNormalizePlaceLeavesEstimateEmptyWhenPriceUnknown(t *testing.T) {
	// 価格帯を返さない店は珍しくない。0 円と読み替えると予算の足切りが壊れる。
	stay := jst(t, "2026-09-05 21:00")
	p := rawPlace()
	p.PriceLevel = ""

	f, ok := normalizePlace(p, model.CategoryDining, stay, stay)
	if !ok {
		t.Fatal("正規化できませんでした")
	}
	if f.PriceLevel != nil || f.EstimatedCostPerPersonJPY != nil {
		t.Errorf("推定できない価格を埋めています: level=%v cost=%v", f.PriceLevel, f.EstimatedCostPerPersonJPY)
	}
}

// ── 営業時間の解決 ────────────────────────────────────

func TestResolveOpeningHoursCoversLateNight(t *testing.T) {
	// このアプリの本番は「終演後の深夜」。17:00 開店・翌 2:00 閉店の区間を
	// 落とすと、狙っている時間帯の候補が丸ごと消える。
	stay := jst(t, "2026-09-05 21:00")
	sat := int(stay.Weekday())
	sun := (sat + 1) % 7
	fri := (sat + 6) % 7

	h := &googleplaces.OpeningHours{Periods: []googleplaces.Period{
		{Open: googleplaces.TimePoint{Day: fri, Hour: 17}, Close: &googleplaces.TimePoint{Day: sat, Hour: 2}},
		{Open: googleplaces.TimePoint{Day: sat, Hour: 17}, Close: &googleplaces.TimePoint{Day: sun, Hour: 2}},
	}}

	got := resolveOpeningHours(h, stay)
	if !got.Known {
		t.Fatal("営業時間が不明のままです")
	}
	// 公演当日 21:00 の来店も、日付をまたいだ 0:30 の滞在も同じ区間で開いている。
	for _, at := range []string{"2026-09-05 21:00", "2026-09-06 00:30", "2026-09-06 01:59"} {
		if !got.IsOpenAt(jst(t, at)) {
			t.Errorf("%s に閉まっている判定です", at)
		}
	}
	if got.IsOpenAt(jst(t, "2026-09-06 02:00")) {
		t.Error("閉店時刻ちょうどを営業中と判定しています")
	}
	// 前日の区間も展開されている（当日 0 時台の来店を支えるのは前日開店の区間）。
	if !got.IsOpenAt(jst(t, "2026-09-05 01:00")) {
		t.Error("前日開店の深夜区間が落ちています")
	}
}

func TestResolveOpeningHoursTreatsMissingCloseAsAlwaysOpen(t *testing.T) {
	stay := jst(t, "2026-09-05 21:00")
	h := &googleplaces.OpeningHours{Periods: []googleplaces.Period{
		{Open: googleplaces.TimePoint{Day: 0, Hour: 0}},
	}}

	got := resolveOpeningHours(h, stay)
	if !got.Known || len(got.Periods) != 1 {
		t.Fatalf("24 時間営業として扱われていません: %+v", got)
	}
	for _, at := range []string{"2026-09-04 23:00", "2026-09-06 04:00"} {
		if !got.IsOpenAt(jst(t, at)) {
			t.Errorf("24 時間営業なのに %s が閉店判定です", at)
		}
	}
}

func TestResolveOpeningHoursUnknownIsNotClosed(t *testing.T) {
	// 提供元が返さなかっただけ。「終日休業」と読み替えると、
	// 営業時間を登録していない個人店が深夜帯から全滅する。
	stay := jst(t, "2026-09-05 21:00")

	for _, h := range []*googleplaces.OpeningHours{nil, {}} {
		got := resolveOpeningHours(h, stay)
		if got.Known || len(got.Periods) != 0 {
			t.Errorf("不明を営業時間ありとして扱っています: %+v", got)
		}
	}
}

func TestCloseAfterRollsOverWhenSameDayNotationPrecedesOpen(t *testing.T) {
	// 開 17:00 / 閉 02:00 が同じ曜日で表記される提供元がある。
	// そのまま解くと閉店が開店の 15 時間前になり、区間が空になる。
	day := jst(t, "2026-09-05 00:00")
	open := day.Add(17 * time.Hour)

	got := closeAfter(open, day, googleplaces.TimePoint{Day: int(day.Weekday()), Hour: 2})
	if want := jst(t, "2026-09-06 02:00"); !got.Equal(want) {
		t.Errorf("閉店時刻 = %v, want %v", got, want)
	}
}

// ── ジャンルの写像 ────────────────────────────────────

func TestPlaceTypesForDedupesAndFallsBack(t *testing.T) {
	tests := []struct {
		name   string
		genres []model.DiningGenre
		want   []string
	}{
		{"未指定は飲食全般に寄せる", nil, []string{"restaurant"}},
		{"未知のジャンルも飲食全般に落とす", []model.DiningGenre{"unknown_genre"}, []string{"restaurant"}},
		{"単一ジャンル", []model.DiningGenre{model.GenreRamen}, []string{"ramen_restaurant"}},
		{
			// 居酒屋と bar は type が重なる。重複したまま投げると同じ店が二重に返る。
			"重なる type は 1 つにまとめる",
			[]model.DiningGenre{model.GenreIzakaya, model.GenreBar},
			[]string{"bar", "japanese_restaurant", "pub"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlaceTypesFor(tt.genres)
			if len(got) != len(tt.want) {
				t.Fatalf("types = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("types = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestToGenreWordsDropsUnknownTypes(t *testing.T) {
	// 提供元の内部語彙（point_of_interest / establishment）が
	// そのままプロンプトへ流れると、LLM が意味を取り違える。
	got := toGenreWords([]string{"barbecue_restaurant", "korean_restaurant", "point_of_interest", "establishment"})
	if len(got) != 1 || got[0] != string(model.GenreYakiniku) {
		t.Errorf("ジャンル語 = %v, want [yakiniku]", got)
	}
	if got := toGenreWords([]string{"point_of_interest"}); len(got) != 0 {
		t.Errorf("未知の type から語を作っています: %v", got)
	}
}

// ── 宿泊 ──────────────────────────────────────────────

func rawRoom(planID int64, total, perPerson int) rakutentravel.Room {
	return rakutentravel.Room{
		Basic: rakutentravel.RoomBasicInfo{
			PlanID:     planID,
			PlanName:   "【素泊まり】深夜チェックインOK",
			RoomName:   "ツイン（禁煙）",
			ReserveURL: "https://hb.afl.rakuten.co.jp/hgc/xxxx/?pc=plan",
		},
		Charges: []rakutentravel.DailyCharge{
			{StayDate: "2026-09-05", Total: total, RakutenCharge: perPerson},
		},
	}
}

func rawHotel(rooms ...rakutentravel.Room) rakutentravel.Hotel {
	return rakutentravel.Hotel{
		Basic: rakutentravel.HotelBasicInfo{
			HotelNo:             143637,
			HotelName:           "東京ドームホテル",
			Address1:            "東京都",
			Address2:            "文京区後楽1-3-61",
			HotelInformationURL: "https://travel.rakuten.co.jp/HOTEL/143637/143637.html",
			PlanListURL:         "https://travel.rakuten.co.jp/HOTEL/143637/rate.html",
			// 楽天の座標は「秒」。35.7056 度 = 128540.16 秒。
			Latitude:  128540.16,
			Longitude: 503106.84,
		},
		Detail: &rakutentravel.HotelDetailInfo{
			CheckinTime:     "15:00",
			CheckoutTime:    "11:00",
			LastCheckInTime: "26:00",
		},
		Rooms: rooms,
	}
}

func TestNormalizeHotelConvertsSecondsToDegrees(t *testing.T) {
	// 度のまま送受信すると赤道上の海を指す。しかもエラーは出ない。
	now := jst(t, "2026-08-30 12:00")

	f, ok := normalizeHotel(rawHotel(rawRoom(1001, 18000, 9000)), now)
	if !ok {
		t.Fatal("正規化できませんでした")
	}
	if diff := f.Loc.Lat - 35.7056; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("緯度 = %v, want 35.7056", f.Loc.Lat)
	}
	if diff := f.Loc.Lng - 139.7519; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("経度 = %v, want 139.7519", f.Loc.Lng)
	}
	if f.Address != "東京都文京区後楽1-3-61" {
		t.Errorf("住所 = %q", f.Address)
	}
	if f.ProviderHotelID != "143637" {
		t.Errorf("施設 ID = %q, want 143637", f.ProviderHotelID)
	}
}

func TestNormalizeHotelKeepsProviderURLsVerbatim(t *testing.T) {
	// アフィリエイト URL を自前で組み直すと、ID の 1 文字違いで収益が黙って消える。
	now := jst(t, "2026-08-30 12:00")

	f, ok := normalizeHotel(rawHotel(rawRoom(1001, 18000, 9000)), now)
	if !ok {
		t.Fatal("正規化できませんでした")
	}
	// 施設ページはプラン一覧を優先する（予約導線に近い）。
	if f.InformationURL != "https://travel.rakuten.co.jp/HOTEL/143637/rate.html" {
		t.Errorf("施設 URL = %q", f.InformationURL)
	}
	if f.Plans[0].ReserveURL != "https://hb.afl.rakuten.co.jp/hgc/xxxx/?pc=plan" {
		t.Errorf("予約 URL が書き換わっています: %q", f.Plans[0].ReserveURL)
	}

	// プラン一覧が無ければ施設ページに落とす。
	h := rawHotel(rawRoom(1001, 18000, 9000))
	h.Basic.PlanListURL = ""
	f, _ = normalizeHotel(h, now)
	if f.InformationURL != "https://travel.rakuten.co.jp/HOTEL/143637/143637.html" {
		t.Errorf("施設 URL のフォールバックが効いていません: %q", f.InformationURL)
	}
}

func TestNormalizeHotelRejectsWhenNoPresentablePlan(t *testing.T) {
	now := jst(t, "2026-08-30 12:00")

	tests := []struct {
		name  string
		hotel rakutentravel.Hotel
	}{
		{"プランが 1 つも無い", rawHotel()},
		{"料金が出ないプランだけ", rawHotel(rawRoom(1001, 0, 0))},
		{"プラン ID が無い", rawHotel(rawRoom(0, 18000, 9000))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := normalizeHotel(tt.hotel, now); ok {
				t.Error("提示できない宿が候補に入りました")
			}
		})
	}

	// 施設側の欠落も同様に落とす。
	h := rawHotel(rawRoom(1001, 18000, 9000))
	h.Basic.HotelName = ""
	if _, ok := normalizeHotel(h, now); ok {
		t.Error("名前の無いホテルが通りました")
	}
}

func TestNormalizeHotelDropsZeroReviewAverage(t *testing.T) {
	// レビュー 0 件のとき平均は 0.0 で返る。そのまま持つと
	// 「評価 0 の宿」に見え、スコアでも最下位に沈む。
	now := jst(t, "2026-08-30 12:00")
	h := rawHotel(rawRoom(1001, 18000, 9000))
	h.Basic.ReviewCount, h.Basic.ReviewAverage = 0, 0

	f, _ := normalizeHotel(h, now)
	if f.ReviewAverage != nil || f.ReviewCount != nil {
		t.Errorf("0 件のレビューを評価として持っています: avg=%v count=%v", f.ReviewAverage, f.ReviewCount)
	}

	h.Basic.ReviewCount, h.Basic.ReviewAverage = 214, 4.12
	f, _ = normalizeHotel(h, now)
	if f.ReviewAverage == nil || *f.ReviewAverage != 4.12 || f.ReviewCount == nil || *f.ReviewCount != 214 {
		t.Errorf("評価が写っていません: avg=%v count=%v", f.ReviewAverage, f.ReviewCount)
	}
}

func TestNormalizeHotelPlanKeepsLateCheckInNotation(t *testing.T) {
	// "26:00" を 24 時制に丸めると、終演が遅い公演で
	// 「間に合わない宿」と「間に合う宿」が逆転する。
	now := jst(t, "2026-08-30 12:00")

	f, ok := normalizeHotel(rawHotel(rawRoom(1001, 18000, 9000)), now)
	if !ok {
		t.Fatal("正規化できませんでした")
	}
	p := f.Plans[0]
	if p.CheckInDeadline == nil || *p.CheckInDeadline != model.ClockTime(26*60) {
		t.Fatalf("最終チェックイン = %v, want 26:00", p.CheckInDeadline)
	}
	deadline, ok := p.CheckInDeadlineAt(jst(t, "2026-09-05 00:00"))
	if !ok {
		t.Fatal("最終チェックイン時刻を解決できません")
	}
	if want := jst(t, "2026-09-06 02:00"); !deadline.Equal(want) {
		t.Errorf("締切 = %v, want %v", deadline, want)
	}
	if p.CheckInTime == nil || *p.CheckInTime != model.ClockTime(15*60) {
		t.Errorf("チェックイン = %v, want 15:00", p.CheckInTime)
	}
	if p.PricePerPersonJPY == nil || *p.PricePerPersonJPY != 9000 {
		t.Errorf("1 名あたり = %v, want 9000", p.PricePerPersonJPY)
	}
	if p.TotalPriceJPY != 18000 {
		t.Errorf("合計 = %d, want 18000", p.TotalPriceJPY)
	}
}

func TestNormalizeHotelPlanWithoutDetailHasUnknownDeadline(t *testing.T) {
	// Detail が返らないことがある。nil は「制限なし」ではなく「不明」。
	now := jst(t, "2026-08-30 12:00")
	h := rawHotel(rawRoom(1001, 18000, 9000))
	h.Detail = nil

	f, _ := normalizeHotel(h, now)
	if f.Plans[0].CheckInDeadline != nil {
		t.Error("不明な締切を値として埋めています")
	}
}

func TestVacancyAndAmenitiesFromFlags(t *testing.T) {
	now := jst(t, "2026-08-30 12:00")

	room := rawRoom(1001, 18000, 9000)
	room.Basic.SalesformFlag = 1
	room.Basic.WithBreakfastFlag = 1

	f, _ := normalizeHotel(rawHotel(room), now)
	p := f.Plans[0]
	if p.VacancyStatus != model.VacancyFewLeft {
		t.Errorf("空室状況 = %q, want few_left", p.VacancyStatus)
	}
	// 楽天は残室数を返さない。数えられないものを推測して埋めない。
	if p.RemainingRooms != nil {
		t.Errorf("残室数を作り出しています: %v", p.RemainingRooms)
	}
	if !p.HasAmenity(string(model.LodgingBreakfastIncluded)) {
		t.Errorf("朝食付きが写っていません: %v", p.Amenities)
	}
	// 部屋名の「禁煙」から起こす。
	if !p.HasAmenity(string(model.LodgingNoSmoking)) {
		t.Errorf("禁煙が写っていません: %v", p.Amenities)
	}

	// 空室検索の結果なので、フラグが無ければ空きあり。
	f, _ = normalizeHotel(rawHotel(rawRoom(1002, 18000, 9000)), now)
	if f.Plans[0].VacancyStatus != model.VacancyAvailable {
		t.Errorf("空室状況 = %q, want available", f.Plans[0].VacancyStatus)
	}
}

// ── 経路 ──────────────────────────────────────────────

func TestNormalizeMatrixElementRejectsUnroutable(t *testing.T) {
	// **経路が無い要素でも duration は 0 で返る**。condition を見ないと
	// 「徒歩 0 分の隣」と読んで、行けない候補を最上位に置いてしまう。
	venue := model.Waypoint{Name: "東京ドーム", Location: model.Location{Lat: 35.7056, Lng: 139.7519}}
	dest := model.Waypoint{Name: "居酒屋 ○○", Location: model.Location{Lat: 35.7061, Lng: 139.7522}}
	key := model.NewRouteKey(model.RouteOriginVenue, "", model.TravelWalk)
	now := jst(t, "2026-08-30 12:00")

	tests := []struct {
		name string
		el   googleroutes.MatrixElement
	}{
		{
			"経路が存在しない",
			googleroutes.MatrixElement{DestinationIndex: 0, Duration: "0s", Condition: "ROUTE_NOT_FOUND"},
		},
		{
			// 1 件の失敗で行列全体を捨てないため、要素単位の status も見る。
			"要素単位の失敗",
			googleroutes.MatrixElement{
				DestinationIndex: 0, Duration: "480s", Condition: googleroutes.ConditionRouteExists,
				Status: googleroutes.ElementStatus{Code: 3, Message: "INVALID_ARGUMENT"},
			},
		},
		{
			"所要時間が秒表記でない",
			googleroutes.MatrixElement{DestinationIndex: 0, Duration: "8m", Condition: googleroutes.ConditionRouteExists},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := normalizeMatrixElement(tt.el, key, venue, dest, now); ok {
				t.Error("行けない区間を経路として通しました")
			}
		})
	}
}

func TestNormalizeMatrixElementBuildsRouteFact(t *testing.T) {
	venue := model.Waypoint{Name: "東京ドーム", Location: model.Location{Lat: 35.7056, Lng: 139.7519}}
	dest := model.Waypoint{Name: "居酒屋 ○○", Location: model.Location{Lat: 35.7061, Lng: 139.7522}}
	key := model.NewRouteKey(model.RouteOriginVenue, "", model.TravelWalk)
	now := jst(t, "2026-08-30 12:00")

	el := googleroutes.MatrixElement{
		DestinationIndex: 0,
		DistanceMeters:   520,
		Duration:         "480s",
		Condition:        googleroutes.ConditionRouteExists,
	}
	r, ok := normalizeMatrixElement(el, key, venue, dest, now)
	if !ok {
		t.Fatal("経路を訳せませんでした")
	}
	if r.Duration != 8*time.Minute || r.DistanceMeters != 520 {
		t.Errorf("実測値が写っていません: %v / %dm", r.Duration, r.DistanceMeters)
	}
	if r.Mode() != model.TravelWalk || r.From.Name != "東京ドーム" || r.To.Name != "居酒屋 ○○" {
		t.Errorf("端点が写っていません: %+v", r.Key)
	}
	// 行列は経路線も乗換も返さない。採用が決まってから computeRoutes で埋める。
	if r.EncodedPolyline != "" || len(r.TransitLegs) != 0 {
		t.Error("行列が返さないはずの詳細が入っています")
	}
	if r.MapsURL == "" {
		t.Error("地図 URL が組み立てられていません")
	}
}

func fp(f float64) *float64 { return &f }
