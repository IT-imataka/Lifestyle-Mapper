package plan

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/googleplaces"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/googleroutes"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/rakutentravel"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// APICollector は外部 3 API から事実を集め、採番済みの FactStore にして返す。
//
// **段の構成が課金と速度を決めている**。
//
//	第 1 段（並行）: Places と 楽天トラベル。互いに依存しないので同時に投げる。
//	第 2 段（直列）: Routes の行列。候補が確定しないと終点が決まらないため、
//	                 第 1 段の後にしか投げられない。
//
// 設計書の図は 3 つを並列に描いているが、経路の終点は候補そのものなので、
// 実装上は必ず 2 段になる。ここを無理に並列化しようとすると、
// 候補ごとに computeRoutes を投げる形（＝課金が候補数倍）に落ちる。
//
// 部分失敗はエラーにしない。楽天が落ちても飲食だけで組めるなら組み、
// SourceStatus で正直に伝える。**全体 500 より partial のほうが役に立つ**。
type APICollector struct {
	places PlaceSearcher
	hotels HotelSearcher
	routes RouteMatrixer
	opt    CollectorOptions
	now    func() time.Time
}

// PlaceSearcher は会場周辺の飲食店を探す。
// **interface は利用側のここに置く**。実装（と、その前段のキャッシュ）は infrastructure に閉じる。
type PlaceSearcher interface {
	SearchNearby(ctx context.Context, q googleplaces.NearbyQuery) ([]googleplaces.Place, error)
}

// HotelSearcher は空室のあるホテルを探す。
type HotelSearcher interface {
	SearchVacant(ctx context.Context, q rakutentravel.VacantQuery) ([]rakutentravel.Hotel, error)
}

// RouteMatrixer は 1 起点から複数終点への所要時間をまとめて求める。
type RouteMatrixer interface {
	ComputeMatrix(ctx context.Context, q googleroutes.MatrixQuery) ([]googleroutes.MatrixElement, error)
}

// CollectorDeps は APICollector の依存。いずれも nil 可で、
// nil の提供元は「呼ばなかった（skipped）」として扱う。
type CollectorDeps struct {
	Places PlaceSearcher
	Hotels HotelSearcher
	Routes RouteMatrixer
	Now    func() time.Time
}

// CollectorOptions は収集の設定。
type CollectorOptions struct {
	// Timeout は収集全体の上限。超えたぶんは部分失敗として切り捨てる。
	Timeout time.Duration
	// MaxCandidatesPerCategory は FactStore に載せる上限（＝ LLM 入力トークンの天井）。
	MaxCandidatesPerCategory int
}

// walkSpeedMetersPerMinute は徒歩時間から検索半径を起こすための速度。
// 実測ではなく、検索範囲を決めるためだけの粗い値。
const walkSpeedMetersPerMinute = 80

// 検索半径の下限・上限（メートル）。
// 上限 3km は楽天トラベルの searchRadius 上限に揃えてある。
const (
	minSearchRadiusMeters = 300
	maxSearchRadiusMeters = 3000
)

// matrixBudgetPerCategory は経路行列に載せる終点数の上限。
//
// 行列は 1 リクエストだが **要素数で課金される**ので、採点前に距離で粗く切る。
// 直線距離の上位だけを実測に回し、残りは見ない。
const matrixBudgetPerCategory = 25

func NewCollector(deps CollectorDeps, opt CollectorOptions) *APICollector {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if opt.MaxCandidatesPerCategory <= 0 {
		opt.MaxCandidatesPerCategory = model.DefaultMaxCandidatesPerCategory
	}
	return &APICollector{
		places: deps.Places,
		hotels: deps.Hotels,
		routes: deps.Routes,
		opt:    opt,
		now:    deps.Now,
	}
}

var _ Collector = (*APICollector)(nil)

// Collect は候補と経路を集めた FactStore を返す。
func (c *APICollector) Collect(ctx context.Context, cond *model.SearchCondition) (
	*FactStore, []model.SourceStatus, error) {

	if cond == nil {
		return nil, nil, apperror.Internal(errors.New("検索条件が nil です"), "collector.Collect")
	}
	if c.opt.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opt.Timeout)
		defer cancel()
	}

	venue := cond.Event.Venue.Waypoint()
	radius := searchRadiusMeters(cond.Situation.MaxWalk)

	// ── 第 1 段: 候補の収集（並行） ──
	places, hotels, sources := c.search(ctx, cond, radius)

	// 直線距離で粗く切ってから実測に回す。ここで切らないと経路行列の課金が跳ねる。
	places = nearestPlaces(places, venue.Location, matrixBudgetPerCategory)
	hotels = nearestHotels(hotels, venue.Location, matrixBudgetPerCategory)

	store := NewFactStore(venue)
	if len(places) == 0 && len(hotels) == 0 {
		// 候補が無いなら経路を問う相手もいない。ここで課金を止める。
		return store, sources, nil
	}

	// ── 第 2 段: 経路の実測 ──
	matrix, routeStatus, err := c.measure(ctx, cond, venue, places, hotels)
	sources = append(sources, routeStatus)
	if err != nil {
		// 候補はあるのに経路が 1 本も引けない。移動時間を作り話で埋めるより、
		// 取得に失敗したと正直に返す。**事実の層に推測を混ぜない**。
		return nil, sources, err
	}

	// ── 採点 → 採番 ──
	if err := c.register(store, cond, places, hotels, matrix); err != nil {
		return nil, sources, err
	}
	return store, sources, nil
}

// search は Places と楽天を同時に叩く。
//
// errgroup を使いながら**エラーを返さない**のが要点。errgroup は最初の
// エラーで ctx をキャンセルするため、素直に返すと片方の失敗が
// もう片方を道連れにする。ここで欲しいのは「両方の結果」なので、
// 失敗は戻り値ではなく SourceStatus に載せる。
func (c *APICollector) search(ctx context.Context, cond *model.SearchCondition, radius float64) (
	[]*model.PlaceFact, []*model.HotelFact, []model.SourceStatus) {

	var (
		places []*model.PlaceFact
		hotels []*model.HotelFact
		ps     model.SourceStatus
		hs     model.SourceStatus
	)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		places, ps = c.searchPlaces(gctx, cond, radius)
		return nil
	})
	g.Go(func() error {
		hotels, hs = c.searchHotels(gctx, cond, radius)
		return nil
	})
	_ = g.Wait() // 常に nil。失敗は各 SourceStatus に入っている。

	return places, hotels, []model.SourceStatus{ps, hs}
}

func (c *APICollector) searchPlaces(ctx context.Context, cond *model.SearchCondition, radius float64) (
	[]*model.PlaceFact, model.SourceStatus) {

	status := model.SourceStatus{Provider: model.ProviderGooglePlaces, State: model.SourceSkipped}
	if !cond.Dining.Enabled || c.places == nil {
		return nil, status
	}

	started := c.now()
	raw, err := c.places.SearchNearby(ctx, googleplaces.NearbyQuery{
		Center:        cond.Event.Venue.Location,
		RadiusMeters:  radius,
		IncludedTypes: PlaceTypesFor(cond.Dining.Genres),
		LanguageCode:  languageOf(cond.Context.Locale),
	})
	status.Latency = c.now().Sub(started)
	if err != nil {
		status.State = model.SourceFailed
		status.Err = apperror.From(err)
		return nil, status
	}

	// 営業時間は「公演当日」の曜日で解決する。翌日の曜日で解くと
	// 金曜深夜と土曜深夜で営業が違う店を取り違える。
	stayDate := cond.Event.EndsAt.In(cond.Context.TimeLocation())
	now := c.now()

	out := make([]*model.PlaceFact, 0, len(raw))
	for _, p := range raw {
		f, ok := normalizePlace(p, model.CategoryDining, stayDate, now)
		if !ok {
			continue
		}
		out = append(out, f)
	}

	status.ResultCount = len(out)
	status.State = model.SourceOK
	if len(raw) > 0 && len(out) == 0 {
		// 返ってきたのに 1 件も訳せない＝提供元の仕様が変わった疑い。
		// 0 件と同じ顔をさせず、degraded として気づけるようにする。
		status.State = model.SourceDegraded
	}
	return out, status
}

func (c *APICollector) searchHotels(ctx context.Context, cond *model.SearchCondition, radius float64) (
	[]*model.HotelFact, model.SourceStatus) {

	status := model.SourceStatus{Provider: model.ProviderRakutenTravel, State: model.SourceSkipped}
	if !cond.Lodging.Enabled || c.hotels == nil {
		return nil, status
	}

	checkIn, checkOut := stayDates(cond)
	started := c.now()
	raw, err := c.hotels.SearchVacant(ctx, rakutentravel.VacantQuery{
		Center:   cond.Event.Venue.Location,
		RadiusKm: radius / 1000,
		CheckIn:  checkIn,
		CheckOut: checkOut,
		Adults:   cond.Party.Adults,
		Rooms:    cond.Lodging.Rooms,
		// 楽天の料金条件は **1 名 1 泊あたり**。検索条件は 1 泊の総額なので割る。
		// ここを割らずに渡すと、上限 18000 円が「1 人 18000 円」になって
		// 予算外の宿ばかりが返る。
		MinChargePerPerson: perPerson(cond.Lodging.BudgetPerNight.Min, cond.Party.Guests()),
		MaxChargePerPerson: perPersonMax(cond.Lodging.BudgetPerNight, cond.Party.Guests()),
	})
	status.Latency = c.now().Sub(started)
	if err != nil {
		status.State = model.SourceFailed
		status.Err = apperror.From(err)
		return nil, status
	}

	now := c.now()
	out := make([]*model.HotelFact, 0, len(raw))
	for _, h := range raw {
		f, ok := normalizeHotel(h, now)
		if !ok {
			continue
		}
		out = append(out, f)
	}

	status.ResultCount = len(out)
	status.State = model.SourceOK
	if len(raw) > 0 && len(out) == 0 {
		status.State = model.SourceDegraded
	}
	return out, status
}

// routeMatrix は移動手段ごとの行列結果。終点の添字で引く。
type routeMatrix struct {
	// byMode[mode][destinationIndex] = 経路。求まらなかった終点は入らない。
	byMode map[model.TravelMode]map[int]*model.RouteFact
	// offset は終点配列における宿泊候補の開始位置。
	// 飲食と宿泊を 1 本の行列にまとめて投げているため、引くときに戻す。
	hotelOffset int
}

// measure は会場から全候補への所要時間を実測する。
//
// **1 リクエストにまとめるのが要点**。候補ごとに computeRoutes を投げると
// 単価の高い呼び出しが候補数ぶん走る。移動手段が 2 つあるときだけ
// 2 リクエストになるが、これは徒歩と公共交通で答えが別物なので避けられない。
func (c *APICollector) measure(ctx context.Context, cond *model.SearchCondition, venue model.Waypoint,
	places []*model.PlaceFact, hotels []*model.HotelFact) (*routeMatrix, model.SourceStatus, error) {

	status := model.SourceStatus{Provider: model.ProviderGoogleRoutes, State: model.SourceSkipped}
	if c.routes == nil {
		return &routeMatrix{byMode: map[model.TravelMode]map[int]*model.RouteFact{}}, status, nil
	}

	dests := make([]model.Waypoint, 0, len(places)+len(hotels))
	for _, p := range places {
		dests = append(dests, model.Waypoint{Name: p.Name, Location: p.Loc, GooglePlaceID: p.ProviderPlaceID})
	}
	for _, h := range hotels {
		dests = append(dests, model.Waypoint{Name: h.Name, Location: h.Loc})
	}

	modes := measurableModes(cond.Options.TransportModes)
	out := &routeMatrix{
		byMode:      make(map[model.TravelMode]map[int]*model.RouteFact, len(modes)),
		hotelOffset: len(places),
	}

	var (
		mu       sync.Mutex
		failures []error
	)
	started := c.now()

	g, gctx := errgroup.WithContext(ctx)
	for _, mode := range modes {
		g.Go(func() error {
			byIndex, err := c.matrixFor(gctx, cond, venue, dests, mode)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// 徒歩が取れても公共交通が落ちることはある。片方だけで組めるなら組む。
				failures = append(failures, err)
				return nil
			}
			out.byMode[mode] = byIndex
			return nil
		})
	}
	_ = g.Wait()

	status.Latency = c.now().Sub(started)
	status.ResultCount = out.count()

	switch {
	case len(out.byMode) == 0:
		// 全手段で失敗。候補があっても移動時間が無ければ時系列は組めない。
		status.State = model.SourceFailed
		status.Err = apperror.From(errors.Join(failures...))
		return nil, status, apperror.Wrap(
			fmt.Errorf("経路を 1 件も取得できませんでした: %w", errors.Join(failures...)),
			apperror.CodeUpstreamFailed, "collector.measure")
	case len(failures) > 0:
		status.State = model.SourceDegraded
		status.Err = apperror.From(errors.Join(failures...))
	default:
		status.State = model.SourceOK
	}
	return out, status, nil
}

func (c *APICollector) matrixFor(ctx context.Context, cond *model.SearchCondition, venue model.Waypoint,
	dests []model.Waypoint, mode model.TravelMode) (map[int]*model.RouteFact, error) {

	elements, err := c.routes.ComputeMatrix(ctx, googleroutes.MatrixQuery{
		Origin:       venue,
		Destinations: dests,
		Mode:         mode,
		// 出発は退場後。深夜の公共交通は本数で所要時間が変わるので、
		// 現在時刻ではなく **公演の退場時刻**で問う。
		DepartureAt: cond.Event.DepartureAt(),
		Language:    languageOf(cond.Context.Locale),
	})
	if err != nil {
		return nil, err
	}

	now := c.now()
	byIndex := make(map[int]*model.RouteFact, len(elements))
	for _, el := range elements {
		i := el.DestinationIndex
		if i < 0 || i >= len(dests) {
			continue // 送っていない終点の応答。無視する
		}
		// 鍵の To はこの時点で決められない（採番は採点の後）。
		// 仮の鍵で持ち、FactStore へ入れるときに candidateId で振り直す。
		key := model.NewRouteKey(model.RouteOriginVenue, "", mode)
		r, ok := normalizeMatrixElement(el, key, venue, dests[i], now)
		if !ok {
			continue
		}
		byIndex[i] = r
	}
	return byIndex, nil
}

func (m *routeMatrix) count() int {
	n := 0
	for _, byIndex := range m.byMode {
		n += len(byIndex)
	}
	return n
}

// lookupPlace / lookupHotel は採点に渡す経路の引き手。
//
// 徒歩を優先し、無ければ他の手段に落とす。徒歩で行ける店を
// わざわざ電車の所要時間で採点すると、隣のビルの店が沈む。
func (m *routeMatrix) lookupPlace(i int) (*model.RouteFact, bool) { return m.best(i) }

func (m *routeMatrix) lookupHotel(i int) (*model.RouteFact, bool) { return m.best(m.hotelOffset + i) }

func (m *routeMatrix) best(index int) (*model.RouteFact, bool) {
	if r, ok := m.byMode[model.TravelWalk][index]; ok {
		return r, true
	}
	for _, mode := range []model.TravelMode{model.TravelTransit, model.TravelDrive, model.TravelBicycle} {
		if r, ok := m.byMode[mode][index]; ok {
			return r, true
		}
	}
	return nil, false
}

// all は 1 つの終点について、求まったすべての手段の経路を返す。
func (m *routeMatrix) all(index int) []*model.RouteFact {
	out := make([]*model.RouteFact, 0, len(m.byMode))
	for _, byIndex := range m.byMode {
		if r, ok := byIndex[index]; ok {
			out = append(out, r)
		}
	}
	return out
}

// register は採点し、上位を FactStore に採番して入れる。
//
// **採番はここでしか行わない**。順序がそのまま LLM への提示順になるので、
// スコア順に入れることで「上から検討してよい」という前提を LLM に渡せる。
func (c *APICollector) register(store *FactStore, cond *model.SearchCondition,
	places []*model.PlaceFact, hotels []*model.HotelFact, matrix *routeMatrix) error {

	limit := cond.Options.MaxCandidatesPerCategory
	if limit <= 0 || limit > c.opt.MaxCandidatesPerCategory {
		limit = c.opt.MaxCandidatesPerCategory
	}

	for _, sp := range scorePlaces(places, matrix.lookupPlace, cond, limit) {
		id, err := store.AddPlace(sp.fact)
		if err != nil {
			return err
		}
		if err := putRoutes(store, id, matrix, indexOfPlace(places, sp.fact)); err != nil {
			return err
		}
	}

	arrival := lodgingArrival(cond)
	for _, sh := range scoreHotels(hotels, matrix.lookupHotel, cond, arrival, limit) {
		id, err := store.AddHotel(sh.fact)
		if err != nil {
			return err
		}
		idx := matrix.hotelOffset + indexOfHotel(hotels, sh.fact)
		if err := putRoutes(store, id, matrix, idx); err != nil {
			return err
		}
	}
	return nil
}

// putRoutes は採番済み ID を鍵に振り直して経路を保管する。
func putRoutes(store *FactStore, id model.CandidateID, matrix *routeMatrix, index int) error {
	for _, r := range matrix.all(index) {
		fixed := *r
		fixed.Key = model.NewRouteKey(model.RouteOriginVenue, string(id), r.Key.Mode)
		if err := store.PutRoute(&fixed); err != nil {
			return err
		}
	}
	return nil
}

func indexOfPlace(list []*model.PlaceFact, want *model.PlaceFact) int {
	for i, f := range list {
		if f == want {
			return i
		}
	}
	return -1
}

func indexOfHotel(list []*model.HotelFact, want *model.HotelFact) int {
	for i, f := range list {
		if f == want {
			return i
		}
	}
	return -1
}

// ── 補助 ──────────────────────────────────────────────

// searchRadiusMeters は歩ける時間から検索半径を起こす。
// 直線距離は実際の道のりより短いので、余裕を見て 1.3 倍する。
func searchRadiusMeters(maxWalk time.Duration) float64 {
	m := maxWalk.Minutes() * walkSpeedMetersPerMinute * 1.3
	switch {
	case m < minSearchRadiusMeters:
		return minSearchRadiusMeters
	case m > maxSearchRadiusMeters:
		return maxSearchRadiusMeters
	}
	return m
}

// stayDates は宿泊日程を決める。未指定なら公演当日 → 翌日。
func stayDates(cond *model.SearchCondition) (time.Time, time.Time) {
	if !cond.Lodging.CheckIn.IsZero() && !cond.Lodging.CheckOut.IsZero() {
		return cond.Lodging.CheckIn, cond.Lodging.CheckOut
	}
	// 終演が 25 時（翌 1 時）でも「公演当日の宿」を取りたいので、
	// 深夜帯の終演は前日を宿泊日とみなす。
	end := cond.Event.EndsAt.In(cond.Context.TimeLocation())
	day := startOfDay(end)
	if end.Hour() < 5 {
		day = day.AddDate(0, 0, -1)
	}
	return day, day.AddDate(0, 0, 1)
}

// lodgingArrival は宿への到着見込み時刻。チェックイン締切の判定に使う。
// 食事を挟むならその滞在時間ぶん遅れる。移動時間は候補ごとに違うので
// ここでは見込みで足す（確定判定は validator が行う）。
func lodgingArrival(cond *model.SearchCondition) time.Time {
	t := cond.Event.DepartureAt()
	if cond.Dining.Enabled {
		t = t.Add(cond.Dining.DesiredStay)
	}
	return t.Add(cond.Situation.MaxWalk)
}

// measurableModes は行列を投げる移動手段を選ぶ。
// 指定が無ければ徒歩だけ。**手段を増やすとリクエストが増える**ので、
// 要求されていない手段を気を利かせて足さない。
func measurableModes(modes []model.TravelMode) []model.TravelMode {
	out := make([]model.TravelMode, 0, len(modes))
	for _, m := range modes {
		if m.Valid() {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return []model.TravelMode{model.TravelWalk}
	}
	return out
}

func perPerson(total, guests int) int {
	if guests < 1 || total <= 0 {
		return 0
	}
	return total / guests
}

func perPersonMax(b model.MoneyRangeJPY, guests int) int {
	if !b.HasMax() {
		return 0
	}
	return perPerson(*b.Max, guests)
}

// languageOf はロケールから言語コードを取り出す。ja-JP → ja。
func languageOf(locale string) string {
	if len(locale) >= 2 {
		return locale[:2]
	}
	return "ja"
}

// nearestPlaces / nearestHotels は直線距離で上位 n 件に粗く切る。
// 実測（＝課金）に回す前の足切りで、順位付けそのものではない。
func nearestPlaces(list []*model.PlaceFact, origin model.Location, n int) []*model.PlaceFact {
	if len(list) <= n {
		return list
	}
	sorted := make([]*model.PlaceFact, len(list))
	copy(sorted, list)
	sort.SliceStable(sorted, func(i, j int) bool {
		return origin.DistanceMeters(sorted[i].Loc) < origin.DistanceMeters(sorted[j].Loc)
	})
	return sorted[:n]
}

func nearestHotels(list []*model.HotelFact, origin model.Location, n int) []*model.HotelFact {
	if len(list) <= n {
		return list
	}
	sorted := make([]*model.HotelFact, len(list))
	copy(sorted, list)
	sort.SliceStable(sorted, func(i, j int) bool {
		return origin.DistanceMeters(sorted[i].Loc) < origin.DistanceMeters(sorted[j].Loc)
	})
	return sorted[:n]
}
