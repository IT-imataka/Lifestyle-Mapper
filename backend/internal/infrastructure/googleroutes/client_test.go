package googleroutes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	path      string
	fieldMask string
	raw       []byte
	calls     int
}

func (c *capture) matrix(t *testing.T) matrixRequest {
	t.Helper()
	var body matrixRequest
	if err := json.Unmarshal(c.raw, &body); err != nil {
		t.Fatalf("送信本文を読めません: %v", err)
	}
	return body
}

func (c *capture) route(t *testing.T) routeRequest {
	t.Helper()
	var body routeRequest
	if err := json.Unmarshal(c.raw, &body); err != nil {
		t.Fatalf("送信本文を読めません: %v", err)
	}
	return body
}

func newClient(t *testing.T, status int, response string) (*Client, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.calls++
		got.path = r.URL.Path
		got.fieldMask = r.Header.Get("X-Goog-FieldMask")
		got.raw, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return New("test-key", time.Second, &http.Client{Transport: redirect{base: base}}), got
}

var (
	venue = model.Waypoint{
		Name: "東京ドーム", Location: model.Location{Lat: 35.7056, Lng: 139.7519},
		GooglePlaceID: "ChIJdome",
	}
	izakaya = model.Waypoint{Name: "居酒屋 ○○", Location: model.Location{Lat: 35.7061, Lng: 139.7522}}
	hotel   = model.Waypoint{Name: "○○ホテル", Location: model.Location{Lat: 35.7060, Lng: 139.7530}}
)

func matrixQuery(mode model.TravelMode, departAt time.Time) MatrixQuery {
	return MatrixQuery{
		Origin:       venue,
		Destinations: []model.Waypoint{izakaya, hotel},
		Mode:         mode,
		DepartureAt:  departAt,
	}
}

// ── 出発時刻の扱い（徒歩が主役のアプリで最も壊れやすい点） ──

func TestComputeMatrixOmitsDepartureTimeForWalk(t *testing.T) {
	// WALK / BICYCLE に departureTime を付けると INVALID_ARGUMENT で
	// リクエストごと落ちる。徒歩が主役なので、ここを間違えると全滅する。
	departAt := time.Date(2026, 9, 5, 21, 30, 0, 0, time.FixedZone("JST", 9*3600))

	for _, mode := range []model.TravelMode{model.TravelWalk, model.TravelBicycle} {
		t.Run(string(mode), func(t *testing.T) {
			c, got := newClient(t, http.StatusOK, `[]`)
			if _, err := c.ComputeMatrix(context.Background(), matrixQuery(mode, departAt)); err != nil {
				t.Fatal(err)
			}
			if body := got.matrix(t); body.DepartureTime != "" {
				t.Errorf("徒歩に出発時刻を送っています: %q", body.DepartureTime)
			}
			// 送信本文にキーごと現れないこと（omitempty が効いていること）。
			if strings.Contains(string(got.raw), "departureTime") {
				t.Errorf("本文に departureTime が残っています: %s", got.raw)
			}
		})
	}
}

func TestComputeMatrixSendsDepartureTimeForTimetabledModes(t *testing.T) {
	// 深夜の公共交通は本数で所要時間が変わる。出発時刻が無いと終電判定が意味を失う。
	departAt := time.Date(2026, 9, 5, 21, 30, 0, 0, time.FixedZone("JST", 9*3600))

	for _, mode := range []model.TravelMode{model.TravelTransit, model.TravelDrive} {
		t.Run(string(mode), func(t *testing.T) {
			c, got := newClient(t, http.StatusOK, `[]`)
			if _, err := c.ComputeMatrix(context.Background(), matrixQuery(mode, departAt)); err != nil {
				t.Fatal(err)
			}
			body := got.matrix(t)
			if body.DepartureTime != "2026-09-05T12:30:00Z" {
				t.Errorf("出発時刻 = %q, want 2026-09-05T12:30:00Z（UTC 正規化）", body.DepartureTime)
			}
		})
	}
}

func TestComputeMatrixOmitsZeroDepartureTime(t *testing.T) {
	c, got := newClient(t, http.StatusOK, `[]`)
	if _, err := c.ComputeMatrix(context.Background(), matrixQuery(model.TravelTransit, time.Time{})); err != nil {
		t.Fatal(err)
	}
	if body := got.matrix(t); body.DepartureTime != "" {
		t.Errorf("ゼロ値の出発時刻を送っています: %q", body.DepartureTime)
	}
}

// ── 行列の組み立て ────────────────────────────────────

func TestComputeMatrixBuildsSingleOriginRequest(t *testing.T) {
	c, got := newClient(t, http.StatusOK, `[]`)
	if _, err := c.ComputeMatrix(context.Background(), matrixQuery(model.TravelWalk, time.Time{})); err != nil {
		t.Fatal(err)
	}

	if got.path != "/distanceMatrix/v2:computeRouteMatrix" {
		t.Errorf("パス = %q", got.path)
	}
	// condition を含まないと、経路が無い要素を「徒歩 0 分の隣」と誤認する。
	if got.fieldMask != MatrixFieldMask || !strings.Contains(got.fieldMask, "condition") {
		t.Errorf("FieldMask = %q", got.fieldMask)
	}

	body := got.matrix(t)
	// 会場 → 全候補を 1 リクエストにまとめる。起点は必ず 1 つ。
	if len(body.Origins) != 1 || len(body.Destinations) != 2 {
		t.Fatalf("起点 %d / 終点 %d, want 1 / 2", len(body.Origins), len(body.Destinations))
	}
	// placeId があれば座標より優先する。建物の出入口に寄った経路になる。
	if body.Origins[0].Waypoint.PlaceID != "ChIJdome" || body.Origins[0].Waypoint.Location != nil {
		t.Errorf("起点 = %+v, want placeId 指定", body.Origins[0].Waypoint)
	}
	dest := body.Destinations[0].Waypoint
	if dest.PlaceID != "" || dest.Location == nil || dest.Location.LatLng.Latitude != 35.7061 {
		t.Errorf("終点 = %+v, want 座標指定", dest)
	}
	if body.TravelMode != "WALK" || body.Units != "METRIC" || body.LanguageCode != "ja" {
		t.Errorf("本文 = %+v", body)
	}
}

func TestComputeMatrixDecodesBareArray(t *testing.T) {
	// 応答は要素の JSON 配列（オブジェクトに包まれない）。順序も保証されない。
	c, _ := newClient(t, http.StatusOK, `[
		{"originIndex":0,"destinationIndex":1,"distanceMeters":640,"duration":"540s","condition":"ROUTE_EXISTS"},
		{"originIndex":0,"destinationIndex":0,"distanceMeters":420,"duration":"330s","condition":"ROUTE_EXISTS"},
		{"originIndex":0,"destinationIndex":2,"condition":"ROUTE_NOT_FOUND"}
	]`)

	got, err := c.ComputeMatrix(context.Background(), matrixQuery(model.TravelWalk, time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("要素 = %d, want 3", len(got))
	}
	// 順序に依存せず DestinationIndex で引き当てられること。
	if got[0].DestinationIndex != 1 || got[1].DestinationIndex != 0 {
		t.Errorf("応答の順序が書き換えられています: %+v", got)
	}
	if !got[0].OK() || !got[1].OK() {
		t.Error("求まった経路を使えない要素として扱っています")
	}
	// 経路が無い要素は失敗ではなく「そこへは行けない」という事実。そのまま返す。
	if got[2].OK() {
		t.Error("経路の無い要素を使える要素として扱っています")
	}
	if d, err := got[1].TravelDuration(); err != nil || d != 330*time.Second {
		t.Errorf("所要時間 = %v (%v), want 5m30s", d, err)
	}
}

func TestComputeMatrixRejectsBadQueryBeforeCalling(t *testing.T) {
	t.Run("終点なしは呼ばない", func(t *testing.T) {
		c, got := newClient(t, http.StatusOK, `[]`)
		q := matrixQuery(model.TravelWalk, time.Time{})
		q.Destinations = nil

		out, err := c.ComputeMatrix(context.Background(), q)
		if err != nil || out != nil {
			t.Errorf("終点なしで %v / %v", out, err)
		}
		if got.calls != 0 {
			t.Errorf("空の行列を問い合わせています: %d 回", got.calls)
		}
	})

	t.Run("要素数の上限超過", func(t *testing.T) {
		c, got := newClient(t, http.StatusOK, `[]`)
		q := matrixQuery(model.TravelWalk, time.Time{})
		q.Destinations = make([]model.Waypoint, maxMatrixElements+1)

		_, err := c.ComputeMatrix(context.Background(), q)
		var e *apperror.Error
		if !errors.As(err, &e) || e.Code != apperror.CodeInternal {
			t.Fatalf("コード = %v, want INTERNAL_ERROR", err)
		}
		if got.calls != 0 {
			t.Errorf("上限超過のまま呼んでいます: %d 回", got.calls)
		}
	})

	t.Run("未対応の移動手段", func(t *testing.T) {
		c, got := newClient(t, http.StatusOK, `[]`)
		_, err := c.ComputeMatrix(context.Background(), matrixQuery("HOVERBOARD", time.Time{}))

		var e *apperror.Error
		if !errors.As(err, &e) || e.Code != apperror.CodeInternal {
			t.Fatalf("コード = %v, want INTERNAL_ERROR", err)
		}
		if got.calls != 0 {
			t.Errorf("未対応の手段で呼んでいます: %d 回", got.calls)
		}
	})
}

// ── 区間の詳細 ────────────────────────────────────────

func TestComputeRouteExtractsTransitLegs(t *testing.T) {
	c, got := newClient(t, http.StatusOK, `{"routes":[{
		"distanceMeters":8200,
		"duration":"1380s",
		"polyline":{"encodedPolyline":"abc_def"},
		"legs":[{"steps":[
			{"transitDetails":null},
			{"transitDetails":{
				"stopDetails":{
					"departureStop":{"name":"水道橋"},
					"departureTime":"2026-09-05T13:12:00Z",
					"arrivalStop":{"name":"新宿"},
					"arrivalTime":"2026-09-05T13:24:00Z"
				},
				"headsign":"三鷹方面",
				"transitLine":{"name":"JR中央・総武線","nameShort":"総武線"},
				"stopCount":4
			}}
		]}]
	}]}`)

	route, err := c.ComputeRoute(context.Background(), RouteQuery{
		Origin: venue, Destination: izakaya, Mode: model.TravelTransit,
		DepartureAt: time.Date(2026, 9, 5, 21, 30, 0, 0, time.FixedZone("JST", 9*3600)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/directions/v2:computeRoutes" || got.fieldMask != RouteFieldMask {
		t.Errorf("パス = %q / FieldMask = %q", got.path, got.fieldMask)
	}
	if body := got.route(t); body.PolylineEncoding != "ENCODED_POLYLINE" {
		t.Errorf("経路線の形式 = %q", body.PolylineEncoding)
	}

	if route == nil {
		t.Fatal("経路が返りません")
	}
	if route.EncodedPolyline() != "abc_def" || route.DistanceMeters != 8200 {
		t.Errorf("経路 = %+v", route)
	}
	if d, err := route.TravelDuration(); err != nil || d != 23*time.Minute {
		t.Errorf("所要時間 = %v (%v), want 23m", d, err)
	}

	legs := route.TransitLegs()
	if len(legs) != 1 {
		t.Fatalf("乗車区間 = %d, want 1（transitDetails の無い手順は除く）", len(legs))
	}
	leg := legs[0]
	// 短縮名を優先する。「JR中央・総武線」より「総武線」のほうが UI で読める。
	if leg.LineName != "総武線" || leg.Headsign != "三鷹方面" {
		t.Errorf("路線 = %q / %q", leg.LineName, leg.Headsign)
	}
	if leg.DepartureStop != "水道橋" || leg.ArrivalStop != "新宿" {
		t.Errorf("駅 = %q → %q", leg.DepartureStop, leg.ArrivalStop)
	}
	// 終電判定はこの発車時刻を見る。
	if leg.DepartureAt.IsZero() || !leg.DepartureAt.Equal(time.Date(2026, 9, 5, 13, 12, 0, 0, time.UTC)) {
		t.Errorf("発車時刻 = %v", leg.DepartureAt)
	}
	if leg.NumStops == nil || *leg.NumStops != 4 {
		t.Errorf("停車数 = %v, want 4", leg.NumStops)
	}
}

func TestComputeRouteTreatsNoRouteAsNil(t *testing.T) {
	// 「経路が無い」は上流の失敗ではない。呼び出し側が別の手段に切り替えられるようにする。
	c, _ := newClient(t, http.StatusOK, `{"routes":[]}`)

	route, err := c.ComputeRoute(context.Background(), RouteQuery{
		Origin: venue, Destination: izakaya, Mode: model.TravelWalk,
	})
	if err != nil {
		t.Fatalf("経路なしを障害として扱っています: %v", err)
	}
	if route != nil {
		t.Errorf("経路 = %+v, want nil", route)
	}
}

func TestTransitLegsToleratesMissingDepartureTime(t *testing.T) {
	// 発車時刻が取れないことは珍しくなく、それだけで区間を捨てるほうが損。
	var r Route
	if err := json.Unmarshal([]byte(`{"legs":[{"steps":[{"transitDetails":{
		"stopDetails":{"departureStop":{"name":"水道橋"},"arrivalStop":{"name":"新宿"}},
		"transitLine":{"name":"JR中央・総武線"}
	}}]}]}`), &r); err != nil {
		t.Fatal(err)
	}

	legs := r.TransitLegs()
	if len(legs) != 1 {
		t.Fatalf("乗車区間 = %d, want 1", len(legs))
	}
	if !legs[0].DepartureAt.IsZero() {
		t.Errorf("不明な発車時刻に値が入っています: %v", legs[0].DepartureAt)
	}
	// 短縮名が無ければ正式名に落とす。
	if legs[0].LineName != "JR中央・総武線" {
		t.Errorf("路線名 = %q", legs[0].LineName)
	}
}

// ── 書式 ──────────────────────────────────────────────

func TestParseDurationAcceptsProtobufSeconds(t *testing.T) {
	ok := map[string]time.Duration{
		"480s":   8 * time.Minute,
		"480.5s": 480500 * time.Millisecond,
		"0s":     0,
	}
	for in, want := range ok {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v (%v), want %v", in, got, err, want)
		}
	}

	// 秒以外の単位が来たら仕様変更。黙って解釈しない。
	for _, in := range []string{"", "8m", "480", "abcs", "-30s"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) がエラーになりません", in)
		}
	}
}

func TestDirectionsURLPrefersPlaceID(t *testing.T) {
	// 駅ビル内の店で入口が正しく出る。
	got := DirectionsURL(venue, izakaya, model.TravelWalk)

	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("origin_place_id") != "ChIJdome" {
		t.Errorf("起点の placeId = %q", q.Get("origin_place_id"))
	}
	if q.Get("origin") != "35.705600,139.751900" {
		t.Errorf("起点 = %q", q.Get("origin"))
	}
	if q.Get("destination_place_id") != "" {
		t.Errorf("placeId の無い終点に値が入っています: %q", q.Get("destination_place_id"))
	}
	if q.Get("travelmode") == "" || q.Get("api") != "1" {
		t.Errorf("クエリ = %v", q)
	}
	// API キーを含まない公開 URL。フロントにそのまま出せること。
	if strings.Contains(got, "key=") {
		t.Errorf("URL に鍵が載っています: %s", got)
	}
}
