package googleplaces

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// redirect は本番の固定 URL 宛のリクエストをテストサーバへ向け直す。
// エンドポイントを差し替え可能にするために本番コードへ穴を開けたくないので、
// トランスポート側で寄せている。
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
	body      searchNearbyRequest
	calls     int
}

func newClient(t *testing.T, status int, response string) (*Client, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.calls++
		got.path = r.URL.Path
		got.fieldMask = r.Header.Get("X-Goog-FieldMask")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)

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

func query() NearbyQuery {
	return NearbyQuery{
		Center:        model.Location{Lat: 35.7056, Lng: 139.7519},
		RadiusMeters:  1560,
		IncludedTypes: []string{"izakaya_placeholder", "ramen_restaurant"},
	}
}

func TestSearchNearbyBuildsRequest(t *testing.T) {
	c, got := newClient(t, http.StatusOK, `{"places":[]}`)
	if _, err := c.SearchNearby(context.Background(), query()); err != nil {
		t.Fatal(err)
	}

	// Places API (New) の URL。旧 API とは別サービスで互換が無い。
	if got.path != "/v1/places:searchNearby" {
		t.Errorf("パス = %q", got.path)
	}
	// FieldMask が課金 SKU を決める。定数と一致していること。
	if got.fieldMask != FieldMask {
		t.Errorf("FieldMask = %q, want %q", got.fieldMask, FieldMask)
	}
	// 距離順に固定する。評価順にすると徒歩 20 分の名店が上位を占める。
	if got.body.RankPreference != "DISTANCE" {
		t.Errorf("並び順 = %q, want DISTANCE", got.body.RankPreference)
	}
	circle := got.body.LocationRestriction.Circle
	if circle.Center.Latitude != 35.7056 || circle.Center.Longitude != 139.7519 {
		t.Errorf("検索の中心 = %+v", circle.Center)
	}
	if circle.Radius != 1560 {
		t.Errorf("検索半径 = %v, want 1560", circle.Radius)
	}
	if len(got.body.IncludedTypes) != 2 {
		t.Errorf("検索 type = %v", got.body.IncludedTypes)
	}
	if got.body.LanguageCode != "ja" || got.body.RegionCode != "JP" {
		t.Errorf("言語・地域 = %q / %q, want ja / JP", got.body.LanguageCode, got.body.RegionCode)
	}
}

func TestSearchNearbyClampsResultCount(t *testing.T) {
	// 上限を超える値を送るとリクエストごと 400 になる。
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"未指定は上限まで", 0, maxResultCount},
		{"上限超過は丸める", 50, maxResultCount},
		{"負値も丸める", -1, maxResultCount},
		{"範囲内はそのまま", 8, 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, got := newClient(t, http.StatusOK, `{"places":[]}`)
			q := query()
			q.MaxResults = tt.in
			if _, err := c.SearchNearby(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			if got.body.MaxResultCount != tt.want {
				t.Errorf("maxResultCount = %d, want %d", got.body.MaxResultCount, tt.want)
			}
		})
	}
}

func TestSearchNearbyTreatsEmptyResultAsSuccess(t *testing.T) {
	// 0 件のとき places はキーごと省略される。深夜の会場周辺では普通に起きる。
	c, _ := newClient(t, http.StatusOK, `{}`)

	places, err := c.SearchNearby(context.Background(), query())
	if err != nil {
		t.Fatalf("0 件を障害として扱っています: %v", err)
	}
	if len(places) != 0 {
		t.Errorf("件数 = %d, want 0", len(places))
	}
}

func TestSearchNearbyReturnsProviderRepresentation(t *testing.T) {
	// ドメインへの翻訳はしない。提供元の表現をそのまま保つことで、
	// 「API が実際に何を返したか」を障害調査でそのまま読める。
	c, _ := newClient(t, http.StatusOK, `{"places":[{
		"id":"ChIJxxx",
		"displayName":{"text":"居酒屋 ○○","languageCode":"ja"},
		"location":{"latitude":35.7061,"longitude":139.7522},
		"priceLevel":"PRICE_LEVEL_MODERATE",
		"types":["japanese_restaurant","restaurant"],
		"rating":4.3,
		"userRatingCount":812,
		"regularOpeningHours":{"periods":[
			{"open":{"day":5,"hour":17,"minute":0},"close":{"day":6,"hour":2,"minute":0}}
		]},
		"photos":[{"name":"places/ChIJxxx/photos/abc","widthPx":4032,"heightPx":3024}]
	}]}`)

	places, err := c.SearchNearby(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	if len(places) != 1 {
		t.Fatalf("件数 = %d, want 1", len(places))
	}
	p := places[0]
	if p.ID != "ChIJxxx" || p.DisplayName.Text != "居酒屋 ○○" {
		t.Errorf("基本項目 = %+v", p)
	}
	// 提供元の語彙のまま保つ（訳すのは normalizer の仕事）。
	if p.PriceLevel != "PRICE_LEVEL_MODERATE" {
		t.Errorf("価格帯 = %q, want PRICE_LEVEL_MODERATE", p.PriceLevel)
	}
	if p.Rating == nil || *p.Rating != 4.3 || p.UserRatingCount == nil || *p.UserRatingCount != 812 {
		t.Errorf("評価 = %v / %v", p.Rating, p.UserRatingCount)
	}
	// 日跨ぎの営業時間が潰れていないこと。
	if p.RegularOpeningHours == nil || len(p.RegularOpeningHours.Periods) != 1 {
		t.Fatalf("営業時間 = %+v", p.RegularOpeningHours)
	}
	period := p.RegularOpeningHours.Periods[0]
	if period.Close == nil || period.Open.Day != 5 || period.Close.Day != 6 || period.Close.Hour != 2 {
		t.Errorf("営業区間 = %+v", period)
	}
	if len(p.Photos) != 1 || p.Photos[0].Name != "places/ChIJxxx/photos/abc" {
		t.Errorf("写真 = %+v", p.Photos)
	}
}

func TestSearchNearbyRejectsInvalidQueryBeforeCalling(t *testing.T) {
	// 座標や半径の誤りは自分側の不具合。上流を叩いて課金してから気づかない。
	tests := []struct {
		name string
		q    NearbyQuery
	}{
		{"中心が未設定", NearbyQuery{RadiusMeters: 1000}},
		{"中心が範囲外", NearbyQuery{Center: model.Location{Lat: 120, Lng: 139}, RadiusMeters: 1000}},
		{"半径がゼロ", NearbyQuery{Center: model.Location{Lat: 35.7, Lng: 139.7}}},
		{"半径が負", NearbyQuery{Center: model.Location{Lat: 35.7, Lng: 139.7}, RadiusMeters: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, got := newClient(t, http.StatusOK, `{}`)
			_, err := c.SearchNearby(context.Background(), tt.q)

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

func TestSearchNearbyPropagatesUpstreamError(t *testing.T) {
	// collector が部分失敗として切り捨てられるよう、必ず UPSTREAM_* で返す。
	c, _ := newClient(t, http.StatusTooManyRequests, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"quota"}}`)

	_, err := c.SearchNearby(context.Background(), query())
	var e *apperror.Error
	if !errors.As(err, &e) || e.Code != apperror.CodeUpstreamRateLimited {
		t.Fatalf("コード = %v, want UPSTREAM_RATE_LIMITED", err)
	}
	if e.Op != "upstream:"+Provider {
		t.Errorf("発生箇所 = %q", e.Op)
	}
}
