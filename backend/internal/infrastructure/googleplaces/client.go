package googleplaces

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/googleapi"
)

// endpoint は Places API (New) の近傍検索。
// 旧 Places API（maps.googleapis.com/maps/api/place）とは別サービスで、
// 認証もフィールド指定も互換が無い。URL を写し間違えると全件 404 になる。
const endpoint = "https://places.googleapis.com/v1/places:searchNearby"

// Provider はエラーと観測情報に載る識別子。
const Provider = "google_places"

// Client は searchNearby を叩く。
type Client struct {
	api *googleapi.Client
}

// New は Places クライアントを作る。timeout が 0 以下なら 5 秒。
func New(apiKey string, timeout time.Duration, hc *http.Client) *Client {
	return &Client{api: googleapi.New(apiKey, Provider, timeout, hc)}
}

// SearchNearby は円の内側の候補を返す。
//
// 返すのは提供元の表現そのままで、ドメインへの翻訳はしない。
// エラーは必ず *apperror.Error（UPSTREAM_*）で、collector が
// 部分失敗として切り捨てるか全体を落とすかを判断できる形にする。
func (c *Client) SearchNearby(ctx context.Context, q NearbyQuery) ([]Place, error) {
	if !q.Center.Valid() {
		return nil, apperror.Internal(
			fmt.Errorf("検索の中心座標が不正です: %+v", q.Center), "googleplaces.SearchNearby")
	}
	if q.RadiusMeters <= 0 {
		return nil, apperror.Internal(
			fmt.Errorf("検索半径が不正です: %v", q.RadiusMeters), "googleplaces.SearchNearby")
	}

	body := searchNearbyRequest{
		IncludedTypes:  q.IncludedTypes,
		MaxResultCount: clampResultCount(q.MaxResults),
		LanguageCode:   orDefault(q.LanguageCode, "ja"),
		RegionCode:     orDefault(q.RegionCode, "JP"),
		// 距離順に固定する。終演後の深夜に歩ける範囲かどうかが最優先で、
		// 評価順にすると徒歩 20 分の名店が上位を占めて使えない候補集合になる。
		RankPreference: "DISTANCE",
		LocationRestriction: locationRestriction{
			Circle: circle{
				Center: LatLng{Latitude: q.Center.Lat, Longitude: q.Center.Lng},
				Radius: q.RadiusMeters,
			},
		},
	}

	var res searchNearbyResponse
	if err := c.api.PostJSON(ctx, endpoint, FieldMask, body, &res); err != nil {
		return nil, err
	}
	// 0 件は失敗ではない。深夜の会場周辺で該当が無いことは普通に起きる。
	return res.Places, nil
}

func clampResultCount(n int) int {
	if n < 1 || n > maxResultCount {
		return maxResultCount
	}
	return n
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
