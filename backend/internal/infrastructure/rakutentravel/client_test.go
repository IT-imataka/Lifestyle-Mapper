package rakutentravel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// redirect は本番の固定 URL 宛のリクエストをテストサーバへ向け直す。
type redirect struct{ base *url.URL }

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = r.base.Scheme, r.base.Host
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}

type capture struct {
	path    string
	params  url.Values
	calls   int
	callsAt []time.Time
}

// newClient は QPS を高めに設定したクライアントを返す。
// レート制限そのものは専用のテストで確かめる。
func newClient(t *testing.T, status int, response string) (*Client, *capture) {
	t.Helper()
	return newClientWithQPS(t, status, response, 1000)
}

func newClientWithQPS(t *testing.T, status int, response string, qps float64) (*Client, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.calls++
		got.callsAt = append(got.callsAt, time.Now())
		got.path = r.URL.Path
		got.params = r.URL.Query()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{
		ApplicationID: "test-app-id",
		AffiliateID:   "test-affiliate-id",
		Timeout:       time.Second,
		QPS:           qps,
		HTTPClient:    &http.Client{Transport: redirect{base: base}},
	}), got
}

func vacantQuery() VacantQuery {
	jst := time.FixedZone("JST", 9*3600)
	return VacantQuery{
		Center:             model.Location{Lat: 35.7056, Lng: 139.7519},
		RadiusKm:           1.56,
		CheckIn:            time.Date(2026, 9, 5, 0, 0, 0, 0, jst),
		CheckOut:           time.Date(2026, 9, 6, 0, 0, 0, 0, jst),
		Adults:             2,
		Rooms:              1,
		MinChargePerPerson: 4000,
		MaxChargePerPerson: 10000,
		Hits:               30,
	}
}

// ── 座標の単位（最も静かに壊れる箇所） ────────────────

func TestSearchVacantSendsCoordinatesInSeconds(t *testing.T) {
	// 座標の単位が度ではなく秒。度のまま送ると赤道上の海を検索して
	// 常に 0 件になり、しかもエラーは出ない。
	c, got := newClient(t, http.StatusOK, `{"pagingInfo":{"recordCount":0},"hotels":[]}`)
	if _, err := c.SearchVacant(context.Background(), vacantQuery()); err != nil {
		t.Fatal(err)
	}

	lat, err := strconv.ParseFloat(got.params.Get("latitude"), 64)
	if err != nil {
		t.Fatalf("緯度が数値ではありません: %q", got.params.Get("latitude"))
	}
	if diff := lat - 128540.16; diff > 0.01 || diff < -0.01 {
		t.Errorf("緯度 = %v 秒, want 128540.16 秒（35.7056 度）", lat)
	}
	lng, _ := strconv.ParseFloat(got.params.Get("longitude"), 64)
	if diff := lng - 503106.84; diff > 0.01 || diff < -0.01 {
		t.Errorf("経度 = %v 秒, want 503106.84 秒（139.7519 度）", lng)
	}
	// 世界測地系。既定は日本測地系で、指定を忘れると数百 m ずれる。
	if got.params.Get("datumType") != "1" {
		t.Errorf("datumType = %q, want 1", got.params.Get("datumType"))
	}
}

func TestDegreesFromSecondsRoundTrips(t *testing.T) {
	got := DegreesFromSeconds(128540.16)
	if diff := got - 35.7056; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("度への変換 = %v, want 35.7056", got)
	}
}

// ── リクエストの組み立て ──────────────────────────────

func TestSearchVacantBuildsQuery(t *testing.T) {
	c, got := newClient(t, http.StatusOK, `{"hotels":[]}`)
	if _, err := c.SearchVacant(context.Background(), vacantQuery()); err != nil {
		t.Fatal(err)
	}

	// バージョン日付込みの固定 URL。日付部分がインタフェースの版そのもの。
	if got.path != "/services/api/Travel/VacantHotelSearch/20170426" {
		t.Errorf("パス = %q", got.path)
	}
	// large でないと roomInfo が返らず、価格が出せない。
	if got.params.Get("responseType") != "large" {
		t.Errorf("responseType = %q, want large", got.params.Get("responseType"))
	}
	if got.params.Get("applicationId") != "test-app-id" {
		t.Errorf("applicationId = %q", got.params.Get("applicationId"))
	}
	// affiliateId を添えて呼ぶと、応答の URL 群がアフィリエイト URL に差し替わる。
	if got.params.Get("affiliateId") != "test-affiliate-id" {
		t.Errorf("affiliateId = %q", got.params.Get("affiliateId"))
	}
	if got.params.Get("checkinDate") != "2026-09-05" || got.params.Get("checkoutDate") != "2026-09-06" {
		t.Errorf("日程 = %q 〜 %q", got.params.Get("checkinDate"), got.params.Get("checkoutDate"))
	}
	if got.params.Get("adultNum") != "2" || got.params.Get("roomNum") != "1" {
		t.Errorf("人数・室数 = %q / %q", got.params.Get("adultNum"), got.params.Get("roomNum"))
	}
	if got.params.Get("minCharge") != "4000" || got.params.Get("maxCharge") != "10000" {
		t.Errorf("料金条件 = %q 〜 %q", got.params.Get("minCharge"), got.params.Get("maxCharge"))
	}
	if got.params.Get("format") != "json" {
		t.Errorf("format = %q", got.params.Get("format"))
	}
}

func TestSearchVacantOmitsAffiliateWhenAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["affiliateId"]; ok {
			t.Errorf("空の affiliateId を送っています: %q", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"hotels":[]}`)
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)

	c := New(Options{
		ApplicationID: "app", QPS: 1000,
		HTTPClient: &http.Client{Transport: redirect{base: base}},
	})
	if _, err := c.SearchVacant(context.Background(), vacantQuery()); err != nil {
		t.Fatal(err)
	}
}

func TestSearchVacantClampsRadiusAndHits(t *testing.T) {
	// 範囲外を送るとリクエストごと弾かれる。
	tests := []struct {
		name       string
		radius     float64
		hits       int
		wantRadius string
		wantHits   string
	}{
		{"下限未満は 0.1km", 0.01, 30, "0.1", "30"},
		{"上限超過は 3.0km", 12, 30, "3.0", "30"},
		{"件数の上限超過は丸める", 1.5, 500, "1.5", "30"},
		{"件数の未指定は上限", 1.5, 0, "1.5", "30"},
		{"範囲内はそのまま", 2.4, 10, "2.4", "10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, got := newClient(t, http.StatusOK, `{"hotels":[]}`)
			q := vacantQuery()
			q.RadiusKm, q.Hits = tt.radius, tt.hits
			if _, err := c.SearchVacant(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			if got.params.Get("searchRadius") != tt.wantRadius {
				t.Errorf("searchRadius = %q, want %q", got.params.Get("searchRadius"), tt.wantRadius)
			}
			if got.params.Get("hits") != tt.wantHits {
				t.Errorf("hits = %q, want %q", got.params.Get("hits"), tt.wantHits)
			}
		})
	}
}

func TestSearchVacantRejectsBadQueryBeforeCalling(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*VacantQuery)
	}{
		{"中心が未設定", func(q *VacantQuery) { q.Center = model.Location{} }},
		{"チェックインが未設定", func(q *VacantQuery) { q.CheckIn = time.Time{} }},
		{"チェックアウトが未設定", func(q *VacantQuery) { q.CheckOut = time.Time{} }},
		{"日程が逆転", func(q *VacantQuery) { q.CheckOut = q.CheckIn.AddDate(0, 0, -1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, got := newClient(t, http.StatusOK, `{"hotels":[]}`)
			q := vacantQuery()
			tt.mut(&q)

			_, err := c.SearchVacant(context.Background(), q)
			var e *apperror.Error
			if !errors.As(err, &e) || e.Code != apperror.CodeInternal {
				t.Fatalf("コード = %v, want INTERNAL_ERROR", err)
			}
			if got.calls != 0 {
				t.Errorf("不正な条件で上流を呼んでいます: %d 回", got.calls)
			}
		})
	}
}

// ── 満室と障害の区別 ──────────────────────────────────

func TestSearchVacantTreatsNotFoundAsZeroResults(t *testing.T) {
	// 404 + not_found は 0 件であって障害ではない。ここを取り違えると、
	// 満室の夜に「サーバエラー」を出すことになる。
	c, _ := newClient(t, http.StatusNotFound,
		`{"error":"not_found","error_description":"検索結果が見つかりませんでした"}`)

	hotels, err := c.SearchVacant(context.Background(), vacantQuery())
	if err != nil {
		t.Fatalf("満室を障害として扱っています: %v", err)
	}
	if hotels != nil {
		t.Errorf("件数 = %d, want 0", len(hotels))
	}
}

func TestSearchVacantClassifiesRealFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   apperror.Code
	}{
		{
			"鍵の誤りは失敗",
			http.StatusUnauthorized,
			`{"error":"wrong_parameter","error_description":"applicationId is invalid"}`,
			apperror.CodeUpstreamFailed,
		},
		{
			"レート制限は独立",
			http.StatusTooManyRequests,
			`{"error":"too_many_requests"}`,
			apperror.CodeUpstreamRateLimited,
		},
		{"504 はタイムアウト", http.StatusGatewayTimeout, `{}`, apperror.CodeUpstreamTimeout},
		{"500 は失敗", http.StatusInternalServerError, `{}`, apperror.CodeUpstreamFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newClient(t, tt.status, tt.body)

			_, err := c.SearchVacant(context.Background(), vacantQuery())
			var e *apperror.Error
			if !errors.As(err, &e) || e.Code != tt.want {
				t.Fatalf("コード = %v, want %v", err, tt.want)
			}
			if e.Op != "upstream:"+Provider {
				t.Errorf("発生箇所 = %q", e.Op)
			}
		})
	}
}

func TestSearchVacantRejectsUnreadableBody(t *testing.T) {
	c, _ := newClient(t, http.StatusOK, `{"hotels":`)

	_, err := c.SearchVacant(context.Background(), vacantQuery())
	var e *apperror.Error
	if !errors.As(err, &e) || e.Code != apperror.CodeUpstreamFailed {
		t.Fatalf("コード = %v, want UPSTREAM_FAILED", err)
	}
}

// ── 応答の平坦化 ──────────────────────────────────────

// 楽天の応答は「1 要素だけのオブジェクトの配列」という独特な形をしている。
const vacantResponseJSON = `{
  "pagingInfo": {"recordCount": 1, "pageCount": 1, "page": 1},
  "hotels": [
    {"hotel": [
      {"hotelBasicInfo": {
        "hotelNo": 143637,
        "hotelName": "東京ドームホテル",
        "hotelInformationUrl": "https://travel.rakuten.co.jp/HOTEL/143637/143637.html",
        "planListUrl": "https://hb.afl.rakuten.co.jp/hgc/aff/?pc=plan",
        "reviewCount": 214,
        "reviewAverage": 4.12,
        "latitude": 128540.16,
        "longitude": 503106.84,
        "address1": "東京都",
        "address2": "文京区後楽1-3-61",
        "hotelImageUrl": "https://img.travel.rakuten.co.jp/image/tr/api/re/x.jpg"
      }},
      {"hotelRatingInfo": {"serviceAverage": 4.2, "locationAverage": 4.6}},
      {"hotelDetailInfo": {"checkinTime": "15:00", "checkoutTime": "11:00", "lastCheckInTime": "26:00"}},
      {"roomInfo": [
        {"roomBasicInfo": {
          "planId": 1234567,
          "planName": "【素泊まり】深夜チェックインOK",
          "roomName": "ツイン（禁煙）",
          "reserveUrl": "https://hb.afl.rakuten.co.jp/hgc/aff/?pc=reserve",
          "withBreakfastFlag": 0,
          "salesformFlag": 1
        }},
        {"dailyCharge": {"stayDate": "2026-09-05", "rakutenCharge": 9000, "total": 18000}}
      ]},
      {"roomInfo": [
        {"roomBasicInfo": {"planId": 7654321, "planName": "朝食付き", "roomName": "ダブル", "withBreakfastFlag": 1}},
        {"dailyCharge": {"stayDate": "2026-09-05", "rakutenCharge": 11000, "total": 22000}}
      ]}
    ]}
  ]
}`

func TestSearchVacantFlattensNestedResponse(t *testing.T) {
	c, _ := newClient(t, http.StatusOK, vacantResponseJSON)

	hotels, err := c.SearchVacant(context.Background(), vacantQuery())
	if err != nil {
		t.Fatal(err)
	}
	if len(hotels) != 1 {
		t.Fatalf("件数 = %d, want 1", len(hotels))
	}
	h := hotels[0]

	if h.Basic.HotelNo != 143637 || h.Basic.HotelName != "東京ドームホテル" {
		t.Errorf("基本情報 = %+v", h.Basic)
	}
	if h.Basic.Address() != "東京都文京区後楽1-3-61" {
		t.Errorf("住所 = %q", h.Basic.Address())
	}
	if h.Rating == nil || h.Rating.LocationAverage != 4.6 {
		t.Errorf("評価詳細 = %+v", h.Rating)
	}
	// Detail が無いと最終チェックイン時刻が取れず、深夜の判定ができなくなる。
	if h.Detail == nil || h.Detail.LastCheckInTime != "26:00" {
		t.Errorf("詳細 = %+v", h.Detail)
	}
	if len(h.Rooms) != 2 {
		t.Fatalf("プラン = %d, want 2", len(h.Rooms))
	}

	first := h.Rooms[0]
	if first.Basic.PlanID != 1234567 || first.Basic.SalesformFlag != 1 {
		t.Errorf("プラン基本情報 = %+v", first.Basic)
	}
	if got := first.TotalJPY(); got != 18000 {
		t.Errorf("合計 = %d, want 18000", got)
	}
	if per, ok := first.PerPersonJPY(); !ok || per != 9000 {
		t.Errorf("1 名あたり = %d (%v), want 9000", per, ok)
	}
	// 予約 URL は API が返した値をそのまま使う。組み直すと収益が黙って消える。
	if first.Basic.ReserveURL != "https://hb.afl.rakuten.co.jp/hgc/aff/?pc=reserve" {
		t.Errorf("予約 URL = %q", first.Basic.ReserveURL)
	}
	if h.Rooms[1].Basic.WithBreakfastFlag != 1 {
		t.Errorf("朝食フラグ = %d", h.Rooms[1].Basic.WithBreakfastFlag)
	}
}

func TestSearchVacantSkipsEntriesWithoutBasicInfo(t *testing.T) {
	// 名前も座標も無い箱を candidate にしない。
	c, _ := newClient(t, http.StatusOK, `{"hotels":[
		{"hotel":[{"hotelDetailInfo":{"checkinTime":"15:00"}}]},
		{"hotel":[{"hotelBasicInfo":{"hotelNo":0,"hotelName":"番号なし"}}]},
		{"hotel":[{"hotelBasicInfo":{"hotelNo":1,"hotelName":"○○ホテル"}}]}
	]}`)

	hotels, err := c.SearchVacant(context.Background(), vacantQuery())
	if err != nil {
		t.Fatal(err)
	}
	if len(hotels) != 1 || hotels[0].Basic.HotelName != "○○ホテル" {
		t.Errorf("平坦化の結果 = %+v", hotels)
	}
}

func TestPerPersonJPYIsUnknownWithoutRakutenCharge(t *testing.T) {
	// 総額しか返らないプランがある。人数で割った推定値を作らない。
	r := Room{Charges: []DailyCharge{{Total: 18000}}}
	if per, ok := r.PerPersonJPY(); ok {
		t.Errorf("1 名あたりを作り出しています: %d", per)
	}
	if got := r.TotalJPY(); got != 18000 {
		t.Errorf("合計 = %d, want 18000", got)
	}
}

// ── レート制限 ────────────────────────────────────────

func TestSearchVacantSerializesRequests(t *testing.T) {
	// 並行 fan-out の中でここだけ直列化される。バーストを許すと
	// 最初の数件で制限に当たり、以降が全滅する。
	const qps = 20 // 50ms 間隔
	c, got := newClientWithQPS(t, http.StatusOK, `{"hotels":[]}`, qps)

	start := time.Now()
	for range 3 {
		if _, err := c.SearchVacant(context.Background(), vacantQuery()); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)

	if got.calls != 3 {
		t.Fatalf("呼び出し = %d 回, want 3", got.calls)
	}
	// バースト 1 なので、2 回目以降は必ず間隔が空く。
	if floor := 2 * time.Second / qps; elapsed < floor {
		t.Errorf("所要 = %v, want %v 以上（1req/%v に均されていない）", elapsed, floor, time.Second/qps)
	}
}

func TestSearchVacantHonorsContextWhileWaiting(t *testing.T) {
	// 待たされるぶんは収集全体のタイムアウトに食い込む。待ち時間も ctx の管理下に置く。
	c, got := newClientWithQPS(t, http.StatusOK, `{"hotels":[]}`, 0.5) // 2 秒間隔

	if _, err := c.SearchVacant(context.Background(), vacantQuery()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.SearchVacant(ctx, vacantQuery())
	var e *apperror.Error
	if !errors.As(err, &e) || e.Code != apperror.CodeUpstreamTimeout {
		t.Fatalf("コード = %v, want UPSTREAM_TIMEOUT", err)
	}
	if got.calls != 1 {
		t.Errorf("待ち切れなかったのにリクエストを送っています: %d 回", got.calls)
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	c := New(Options{ApplicationID: "app"})
	if c.http.Timeout != 5*time.Second {
		t.Errorf("既定のタイムアウト = %v, want 5s", c.http.Timeout)
	}
	if c.limiter.Limit() != 1 || c.limiter.Burst() != 1 {
		t.Errorf("既定のレート = %v (burst %d), want 1 / 1", c.limiter.Limit(), c.limiter.Burst())
	}

	// 渡された http.Client は複製する。呼び出し側の設定を書き換えない。
	shared := &http.Client{Timeout: time.Minute}
	_ = New(Options{ApplicationID: "app", Timeout: time.Second, HTTPClient: shared})
	if shared.Timeout != time.Minute {
		t.Errorf("共有された http.Client を書き換えています: %v", shared.Timeout)
	}
}
