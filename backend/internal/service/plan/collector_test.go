package plan

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/googleplaces"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/googleroutes"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/rakutentravel"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// ── 提供元のスタブ ────────────────────────────────────

type fakePlaces struct {
	mu      sync.Mutex
	calls   int
	queries []googleplaces.NearbyQuery
	result  []googleplaces.Place
	err     error
}

func (f *fakePlaces) SearchNearby(_ context.Context, q googleplaces.NearbyQuery) ([]googleplaces.Place, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.queries = append(f.queries, q)
	return f.result, f.err
}

type fakeHotels struct {
	mu      sync.Mutex
	calls   int
	queries []rakutentravel.VacantQuery
	result  []rakutentravel.Hotel
	err     error
}

func (f *fakeHotels) SearchVacant(_ context.Context, q rakutentravel.VacantQuery) ([]rakutentravel.Hotel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.queries = append(f.queries, q)
	return f.result, f.err
}

// fakeRoutes は移動手段ごとに結果を差し替えられる行列スタブ。
// byMode に無い手段は err を返す（提供元の部分障害の再現）。
type fakeRoutes struct {
	mu       sync.Mutex
	calls    int
	queries  []googleroutes.MatrixQuery
	duration map[model.TravelMode]time.Duration
	// missing は経路が求まらなかった終点の添字。
	missing map[int]bool
	err     error
}

func (f *fakeRoutes) ComputeMatrix(_ context.Context, q googleroutes.MatrixQuery) ([]googleroutes.MatrixElement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.duration[q.Mode]
	if !ok {
		return nil, errors.New("この移動手段は取得できません: " + string(q.Mode))
	}

	out := make([]googleroutes.MatrixElement, 0, len(q.Destinations))
	for i := range q.Destinations {
		el := googleroutes.MatrixElement{
			DestinationIndex: i,
			DistanceMeters:   int(d.Minutes() * walkSpeedMetersPerMinute),
			Duration:         secondsText(d),
			Condition:        googleroutes.ConditionRouteExists,
		}
		if f.missing[i] {
			el.Condition = "ROUTE_NOT_FOUND"
		}
		out = append(out, el)
	}
	return out, nil
}

func (f *fakeRoutes) lastQuery(t *testing.T) googleroutes.MatrixQuery {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		t.Fatal("行列が 1 度も問われていません")
	}
	return f.queries[len(f.queries)-1]
}

// secondsText は Routes API と同じ "480s" 形式に整える。
func secondsText(d time.Duration) string {
	return strconv.Itoa(int(d.Seconds())) + "s"
}

// ── 収集の組み立て ────────────────────────────────────

func collectCond(t *testing.T) *model.SearchCondition {
	t.Helper()
	cond := scoreCond(t)
	cond.Options = model.Options{
		TransportModes:           []model.TravelMode{model.TravelWalk},
		MaxCandidatesPerCategory: 5,
		LLMNarrative:             true,
	}
	return cond
}

func placesResult(n int) []googleplaces.Place {
	out := make([]googleplaces.Place, 0, n)
	for i := range n {
		p := rawPlace()
		p.ID = "ChIJ" + strconv.Itoa(i)
		p.DisplayName.Text = "居酒屋 " + p.ID
		// 会場から少しずつ遠ざける。直線距離の足切りを確かめるため。
		p.Location.Latitude = 35.7056 + float64(i)*0.0002
		out = append(out, p)
	}
	return out
}

func hotelsResult(n int) []rakutentravel.Hotel {
	out := make([]rakutentravel.Hotel, 0, n)
	for i := range n {
		h := rawHotel(rawRoom(int64(1000+i), 18000, 9000))
		h.Basic.HotelNo = 143637 + i
		h.Basic.HotelName = "ホテル " + strconv.Itoa(i)
		h.Basic.Latitude = 128540.16 + float64(i)*0.72
		out = append(out, h)
	}
	return out
}

func newTestCollector(t *testing.T, p *fakePlaces, h *fakeHotels, r *fakeRoutes) *APICollector {
	t.Helper()
	deps := CollectorDeps{Now: func() time.Time { return jst(t, "2026-08-30 12:00") }}
	if p != nil {
		deps.Places = p
	}
	if h != nil {
		deps.Hotels = h
	}
	if r != nil {
		deps.Routes = r
	}
	return NewCollector(deps, CollectorOptions{MaxCandidatesPerCategory: 8})
}

func statusOf(t *testing.T, sources []model.SourceStatus, p model.Provider) model.SourceStatus {
	t.Helper()
	for _, s := range sources {
		if s.Provider == p {
			return s
		}
	}
	t.Fatalf("%s の観測情報がありません: %+v", p, sources)
	return model.SourceStatus{}
}

func walkOnly(d time.Duration) map[model.TravelMode]time.Duration {
	return map[model.TravelMode]time.Duration{model.TravelWalk: d}
}

// ── 部分失敗の許容 ────────────────────────────────────

func TestCollectSurvivesProviderFailure(t *testing.T) {
	// 楽天が落ちても飲食だけで組めるなら組む。全体 500 より partial のほうが役に立つ。
	places := &fakePlaces{result: placesResult(3)}
	hotels := &fakeHotels{err: apperror.New(apperror.CodeUpstreamFailed, "楽天が落ちています")}
	routes := &fakeRoutes{duration: walkOnly(6 * time.Minute)}

	store, sources, err := newTestCollector(t, places, hotels, routes).
		Collect(context.Background(), collectCond(t))
	if err != nil {
		t.Fatalf("片方の失敗で全体を落としました: %v", err)
	}

	if got := statusOf(t, sources, model.ProviderRakutenTravel); got.State != model.SourceFailed {
		t.Errorf("楽天の状態 = %q, want failed", got.State)
	}
	if got := statusOf(t, sources, model.ProviderGooglePlaces); got.State != model.SourceOK || got.ResultCount != 3 {
		t.Errorf("Places の状態 = %q (%d 件), want ok (3 件)", got.State, got.ResultCount)
	}
	if store.Count(model.CategoryDining) != 3 {
		t.Errorf("飲食候補 = %d 件, want 3", store.Count(model.CategoryDining))
	}
	if store.Count(model.CategoryLodging) != 0 {
		t.Errorf("落ちた提供元から候補が生えています: %d 件", store.Count(model.CategoryLodging))
	}
}

func TestCollectSkipsDisabledCategories(t *testing.T) {
	// 呼ばなかったことを failed と区別する。skipped は障害ではない。
	places := &fakePlaces{result: placesResult(3)}
	hotels := &fakeHotels{result: hotelsResult(2)}
	routes := &fakeRoutes{duration: walkOnly(6 * time.Minute)}

	cond := collectCond(t)
	cond.Lodging.Enabled = false

	_, sources, err := newTestCollector(t, places, hotels, routes).Collect(context.Background(), cond)
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, sources, model.ProviderRakutenTravel); got.State != model.SourceSkipped {
		t.Errorf("楽天の状態 = %q, want skipped", got.State)
	}
	if hotels.calls != 0 {
		t.Errorf("無効なカテゴリの提供元を呼んでいます: %d 回", hotels.calls)
	}
}

func TestCollectReportsDegradedWhenNothingNormalizes(t *testing.T) {
	// 返ってきたのに 1 件も訳せない＝提供元の仕様が変わった疑い。
	// 0 件と同じ顔をさせない。
	broken := placesResult(3)
	for i := range broken {
		broken[i].Location = googleplaces.LatLng{} // 座標が消えた
	}
	places := &fakePlaces{result: broken}
	routes := &fakeRoutes{duration: walkOnly(6 * time.Minute)}

	cond := collectCond(t)
	cond.Lodging.Enabled = false

	_, sources, err := newTestCollector(t, places, nil, routes).Collect(context.Background(), cond)
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, sources, model.ProviderGooglePlaces); got.State != model.SourceDegraded {
		t.Errorf("Places の状態 = %q, want degraded", got.State)
	}
	// 候補が無い以上、経路を問う相手もいない。ここで課金を止める。
	if routes.calls != 0 {
		t.Errorf("候補ゼロで経路を問い合わせています: %d 回", routes.calls)
	}
}

func TestCollectSkipsRoutesWithoutCandidates(t *testing.T) {
	places := &fakePlaces{}
	hotels := &fakeHotels{}
	routes := &fakeRoutes{duration: walkOnly(6 * time.Minute)}

	store, sources, err := newTestCollector(t, places, hotels, routes).
		Collect(context.Background(), collectCond(t))
	if err != nil {
		t.Fatalf("0 件は障害ではありません: %v", err)
	}
	if store.Len() != 0 {
		t.Errorf("候補が生えています: %d 件", store.Len())
	}
	if routes.calls != 0 {
		t.Errorf("候補ゼロで経路を問い合わせています: %d 回", routes.calls)
	}
	// 経路を呼んでいないので観測情報も 2 件のまま。
	if len(sources) != 2 {
		t.Errorf("観測情報 = %d 件, want 2", len(sources))
	}
}

func TestCollectFailsWhenNoRouteAtAll(t *testing.T) {
	// 候補はあるのに経路が 1 本も引けない。移動時間を作り話で埋めるより、
	// 取得に失敗したと正直に返す。
	places := &fakePlaces{result: placesResult(3)}
	routes := &fakeRoutes{err: errors.New("Routes API が落ちています")}

	cond := collectCond(t)
	cond.Lodging.Enabled = false

	store, sources, err := newTestCollector(t, places, nil, routes).Collect(context.Background(), cond)
	if err == nil {
		t.Fatal("経路が全滅したのにエラーになりません")
	}
	if store != nil {
		t.Error("経路の無い FactStore を返しています")
	}
	if got := apperror.From(err); got.Code != apperror.CodeUpstreamFailed {
		t.Errorf("エラーコード = %q, want UPSTREAM_FAILED", got.Code)
	}
	if got := statusOf(t, sources, model.ProviderGoogleRoutes); got.State != model.SourceFailed {
		t.Errorf("Routes の状態 = %q, want failed", got.State)
	}
}

func TestCollectDegradesWhenOneModeFails(t *testing.T) {
	// 徒歩が取れても公共交通が落ちることはある。片方だけで組めるなら組む。
	places := &fakePlaces{result: placesResult(3)}
	routes := &fakeRoutes{duration: walkOnly(6 * time.Minute)} // TRANSIT は未登録＝失敗

	cond := collectCond(t)
	cond.Lodging.Enabled = false
	cond.Options.TransportModes = []model.TravelMode{model.TravelWalk, model.TravelTransit}

	store, sources, err := newTestCollector(t, places, nil, routes).Collect(context.Background(), cond)
	if err != nil {
		t.Fatalf("片方の手段が生きているのに落としました: %v", err)
	}
	got := statusOf(t, sources, model.ProviderGoogleRoutes)
	if got.State != model.SourceDegraded {
		t.Errorf("Routes の状態 = %q, want degraded", got.State)
	}
	if got.Err == nil {
		t.Error("失敗した手段の理由が残っていません")
	}
	if store.Count(model.CategoryDining) != 3 {
		t.Errorf("飲食候補 = %d 件, want 3", store.Count(model.CategoryDining))
	}
}

func TestCollectDropsCandidateWithoutRoute(t *testing.T) {
	// 行けない候補は点数ではなく門で止める。
	places := &fakePlaces{result: placesResult(3)}
	routes := &fakeRoutes{duration: walkOnly(6 * time.Minute), missing: map[int]bool{1: true}}

	cond := collectCond(t)
	cond.Lodging.Enabled = false

	store, _, err := newTestCollector(t, places, nil, routes).Collect(context.Background(), cond)
	if err != nil {
		t.Fatal(err)
	}
	if store.Count(model.CategoryDining) != 2 {
		t.Errorf("飲食候補 = %d 件, want 2", store.Count(model.CategoryDining))
	}
}

// ── 採番と経路の紐づけ ────────────────────────────────

func TestCollectBindsRoutesToAssignedCandidateID(t *testing.T) {
	// 経路は採点の後に採番された ID で引けなければ意味がない。
	places := &fakePlaces{result: placesResult(2)}
	hotels := &fakeHotels{result: hotelsResult(2)}
	routes := &fakeRoutes{duration: walkOnly(6 * time.Minute)}

	store, _, err := newTestCollector(t, places, hotels, routes).
		Collect(context.Background(), collectCond(t))
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []model.CandidateID{
		"cand_dining_001", "cand_dining_002", "cand_lodging_001", "cand_lodging_002",
	} {
		r, ok := store.RouteFromVenue(id, model.TravelWalk)
		if !ok {
			t.Fatalf("%s への経路が引けません", id)
		}
		if r.Duration != 6*time.Minute {
			t.Errorf("%s への所要時間 = %v, want 6m", id, r.Duration)
		}
		if r.Key.From != model.RouteOriginVenue || r.Key.To != string(id) {
			t.Errorf("経路の鍵が候補 ID に振り直されていません: %+v", r.Key)
		}
	}
	if store.RouteCount() != 4 {
		t.Errorf("経路 = %d 本, want 4", store.RouteCount())
	}
}

func TestCollectHonorsCandidateLimit(t *testing.T) {
	// MaxCandidatesPerCategory は LLM 入力トークンの天井そのもの。
	places := &fakePlaces{result: placesResult(12)}
	routes := &fakeRoutes{duration: walkOnly(6 * time.Minute)}

	cond := collectCond(t)
	cond.Lodging.Enabled = false
	cond.Options.MaxCandidatesPerCategory = 5

	store, _, err := newTestCollector(t, places, nil, routes).Collect(context.Background(), cond)
	if err != nil {
		t.Fatal(err)
	}
	if store.Count(model.CategoryDining) != 5 {
		t.Errorf("飲食候補 = %d 件, want 5", store.Count(model.CategoryDining))
	}

	// 要求が収集側の上限を超えたら収集側で抑える。
	cond.Options.MaxCandidatesPerCategory = 20
	store, _, err = newTestCollector(t, &fakePlaces{result: placesResult(12)}, nil,
		&fakeRoutes{duration: walkOnly(6 * time.Minute)}).Collect(context.Background(), cond)
	if err != nil {
		t.Fatal(err)
	}
	if store.Count(model.CategoryDining) != 8 {
		t.Errorf("飲食候補 = %d 件, want 8（収集側の上限）", store.Count(model.CategoryDining))
	}
}

// ── 課金にかかる組み立て ──────────────────────────────

func TestCollectSendsOneMatrixPerMode(t *testing.T) {
	// 候補ごとに computeRoutes を投げると単価の高い呼び出しが候補数ぶん走る。
	// 会場 → 全候補を 1 リクエストにまとめる。
	places := &fakePlaces{result: placesResult(6)}
	hotels := &fakeHotels{result: hotelsResult(4)}
	routes := &fakeRoutes{duration: walkOnly(6 * time.Minute)}

	cond := collectCond(t)
	if _, _, err := newTestCollector(t, places, hotels, routes).Collect(context.Background(), cond); err != nil {
		t.Fatal(err)
	}

	if routes.calls != 1 {
		t.Errorf("行列の呼び出し = %d 回, want 1", routes.calls)
	}
	q := routes.lastQuery(t)
	if len(q.Destinations) != 10 {
		t.Errorf("終点 = %d 件, want 10（飲食 6 + 宿泊 4）", len(q.Destinations))
	}
	// 出発は現在時刻ではなく退場時刻。深夜の公共交通は本数で所要時間が変わる。
	if want := jst(t, "2026-09-05 21:30"); !q.DepartureAt.Equal(want) {
		t.Errorf("出発時刻 = %v, want %v", q.DepartureAt, want)
	}
	if q.Origin.Name != "東京ドーム" {
		t.Errorf("起点 = %q, want 東京ドーム", q.Origin.Name)
	}
}

func TestCollectCapsMatrixDestinations(t *testing.T) {
	// 行列は 1 リクエストだが要素数で課金される。採点前に直線距離で粗く切る。
	places := &fakePlaces{result: placesResult(40)}
	routes := &fakeRoutes{duration: walkOnly(6 * time.Minute)}

	cond := collectCond(t)
	cond.Lodging.Enabled = false

	if _, _, err := newTestCollector(t, places, nil, routes).Collect(context.Background(), cond); err != nil {
		t.Fatal(err)
	}
	q := routes.lastQuery(t)
	if len(q.Destinations) != matrixBudgetPerCategory {
		t.Fatalf("終点 = %d 件, want %d", len(q.Destinations), matrixBudgetPerCategory)
	}
	// 残ったのは近い順。placesResult は添字が大きいほど遠い。
	for _, w := range q.Destinations {
		if w.Name == "居酒屋 ChIJ39" {
			t.Error("最も遠い候補が実測に回されています")
		}
	}
}

func TestCollectSplitsLodgingBudgetPerPerson(t *testing.T) {
	// 楽天の料金条件は 1 名 1 泊あたり。割らずに渡すと
	// 上限 20000 円が「1 人 20000 円」になって予算外の宿ばかり返る。
	hotels := &fakeHotels{result: hotelsResult(2)}
	routes := &fakeRoutes{duration: walkOnly(6 * time.Minute)}

	cond := collectCond(t)
	cond.Dining.Enabled = false
	cond.Party.Adults = 2 // 2 名

	if _, _, err := newTestCollector(t, nil, hotels, routes).Collect(context.Background(), cond); err != nil {
		t.Fatal(err)
	}
	if len(hotels.queries) != 1 {
		t.Fatalf("楽天の呼び出し = %d 回, want 1", len(hotels.queries))
	}
	q := hotels.queries[0]
	if q.MinChargePerPerson != 4000 || q.MaxChargePerPerson != 10000 {
		t.Errorf("料金条件 = %d〜%d, want 4000〜10000（1 泊 8000〜20000 ÷ 2 名）",
			q.MinChargePerPerson, q.MaxChargePerPerson)
	}
	if !q.CheckIn.Equal(jst(t, "2026-09-05 00:00")) || !q.CheckOut.Equal(jst(t, "2026-09-06 00:00")) {
		t.Errorf("宿泊日程 = %v 〜 %v", q.CheckIn, q.CheckOut)
	}
	if q.Adults != 2 || q.Rooms != 1 {
		t.Errorf("人数・室数 = %d 名 / %d 室", q.Adults, q.Rooms)
	}
	// 検索半径は km。徒歩 15 分 → 1560m → 1.56km。
	if q.RadiusKm < 1.5 || q.RadiusKm > 1.6 {
		t.Errorf("検索半径 = %.2fkm, want 約 1.56km", q.RadiusKm)
	}
}

func TestCollectSendsGenreTypesToPlaces(t *testing.T) {
	places := &fakePlaces{result: placesResult(2)}
	routes := &fakeRoutes{duration: walkOnly(6 * time.Minute)}

	cond := collectCond(t)
	cond.Lodging.Enabled = false
	cond.Dining.Genres = []model.DiningGenre{model.GenreRamen}

	if _, _, err := newTestCollector(t, places, nil, routes).Collect(context.Background(), cond); err != nil {
		t.Fatal(err)
	}
	q := places.queries[0]
	if len(q.IncludedTypes) != 1 || q.IncludedTypes[0] != "ramen_restaurant" {
		t.Errorf("検索 type = %v, want [ramen_restaurant]", q.IncludedTypes)
	}
	if q.LanguageCode != "ja" {
		t.Errorf("言語 = %q, want ja", q.LanguageCode)
	}
	if q.RadiusMeters < 1550 || q.RadiusMeters > 1570 {
		t.Errorf("検索半径 = %.0fm, want 約 1560m", q.RadiusMeters)
	}
}

// ── 補助関数 ──────────────────────────────────────────

func TestSearchRadiusMetersClamps(t *testing.T) {
	tests := []struct {
		name    string
		maxWalk time.Duration
		want    float64
	}{
		{"下限で止める", time.Minute, minSearchRadiusMeters},
		{"徒歩 15 分は 1560m", 15 * time.Minute, 1560},
		{"上限で止める（楽天の searchRadius 上限）", 60 * time.Minute, maxSearchRadiusMeters},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := searchRadiusMeters(tt.maxWalk); got != tt.want {
				t.Errorf("検索半径 = %.0f, want %.0f", got, tt.want)
			}
		})
	}
}

func TestStayDatesTreatLateNightAsPreviousDay(t *testing.T) {
	// 終演が翌 1 時でも「公演当日の宿」を取りたい。
	cond := collectCond(t)
	cond.Lodging.CheckIn, cond.Lodging.CheckOut = time.Time{}, time.Time{}
	cond.Event.EndsAt = jst(t, "2026-09-06 01:00")

	in, out := stayDates(cond)
	if want := jst(t, "2026-09-05 00:00"); !in.Equal(want) {
		t.Errorf("チェックイン = %v, want %v", in, want)
	}
	if want := jst(t, "2026-09-06 00:00"); !out.Equal(want) {
		t.Errorf("チェックアウト = %v, want %v", out, want)
	}

	// 5 時以降の終演はその日が宿泊日。
	cond.Event.EndsAt = jst(t, "2026-09-05 21:00")
	in, _ = stayDates(cond)
	if want := jst(t, "2026-09-05 00:00"); !in.Equal(want) {
		t.Errorf("チェックイン = %v, want %v", in, want)
	}

	// 明示された日程が最優先。
	cond.Lodging.CheckIn = jst(t, "2026-09-10 00:00")
	cond.Lodging.CheckOut = jst(t, "2026-09-11 00:00")
	in, _ = stayDates(cond)
	if want := jst(t, "2026-09-10 00:00"); !in.Equal(want) {
		t.Errorf("指定した日程が使われていません: %v", in)
	}
}

func TestMeasurableModesDefaultsToWalk(t *testing.T) {
	// 要求されていない手段を気を利かせて足さない。手段を増やすとリクエストが増える。
	if got := measurableModes(nil); len(got) != 1 || got[0] != model.TravelWalk {
		t.Errorf("既定の移動手段 = %v, want [WALK]", got)
	}
	if got := measurableModes([]model.TravelMode{"HOVERBOARD"}); len(got) != 1 || got[0] != model.TravelWalk {
		t.Errorf("未知の手段の扱い = %v, want [WALK]", got)
	}
	got := measurableModes([]model.TravelMode{model.TravelWalk, model.TravelTransit})
	if len(got) != 2 {
		t.Errorf("移動手段 = %v, want [WALK TRANSIT]", got)
	}
}

func TestPerPersonSplitsTotal(t *testing.T) {
	if got := perPerson(20000, 3); got != 6666 {
		t.Errorf("1 名あたり = %d, want 6666", got)
	}
	// 人数不明・金額なしは条件を付けない（0 は「指定しない」）。
	if got := perPerson(20000, 0); got != 0 {
		t.Errorf("人数 0 のとき = %d, want 0", got)
	}
	if got := perPersonMax(model.MoneyRangeJPY{Min: 8000}, 2); got != 0 {
		t.Errorf("上限なしのとき = %d, want 0", got)
	}
}

func TestLanguageOfTakesPrefix(t *testing.T) {
	for in, want := range map[string]string{"ja-JP": "ja", "en-US": "en", "": "ja", "j": "ja"} {
		if got := languageOf(in); got != want {
			t.Errorf("languageOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRouteMatrixPrefersWalk(t *testing.T) {
	// 徒歩で行ける店を電車の所要時間で採点すると、隣のビルの店が沈む。
	walk := &model.RouteFact{Key: model.NewRouteKey("venue", "", model.TravelWalk), Duration: 6 * time.Minute}
	transit := &model.RouteFact{Key: model.NewRouteKey("venue", "", model.TravelTransit), Duration: 14 * time.Minute}

	m := &routeMatrix{
		byMode: map[model.TravelMode]map[int]*model.RouteFact{
			model.TravelWalk:    {0: walk},
			model.TravelTransit: {0: transit, 1: transit},
		},
		hotelOffset: 1,
	}

	if got, ok := m.lookupPlace(0); !ok || got.Mode() != model.TravelWalk {
		t.Errorf("飲食の経路 = %+v, want WALK", got)
	}
	// 徒歩が無ければ他の手段に落とす。
	if got, ok := m.lookupHotel(0); !ok || got.Mode() != model.TravelTransit {
		t.Errorf("宿泊の経路 = %+v, want TRANSIT", got)
	}
	if _, ok := m.lookupHotel(1); ok {
		t.Error("送っていない終点の経路が引けています")
	}
	if got := m.count(); got != 3 {
		t.Errorf("経路数 = %d, want 3", got)
	}
	if got := m.all(0); len(got) != 2 {
		t.Errorf("終点 0 の全手段 = %d 本, want 2", len(got))
	}
}

func TestCollectRejectsNilCondition(t *testing.T) {
	_, _, err := newTestCollector(t, nil, nil, nil).Collect(context.Background(), nil)
	if err == nil {
		t.Fatal("nil の検索条件を受け入れました")
	}
	if got := apperror.From(err); got.Code != apperror.CodeInternal {
		t.Errorf("エラーコード = %q, want INTERNAL_ERROR", got.Code)
	}
}
