package googleroutes

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/googleapi"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

const (
	matrixEndpoint = "https://routes.googleapis.com/distanceMatrix/v2:computeRouteMatrix"
	routeEndpoint  = "https://routes.googleapis.com/directions/v2:computeRoutes"
)

// Provider はエラーと観測情報に載る識別子。
const Provider = "google_routes"

// maxMatrixElements は 1 リクエストで問える要素数の上限。
// 起点 1 × 候補 20 が実際の最大なので余裕があるが、
// 上限を超えるとリクエストごと 400 になるため送信前に検査する。
const maxMatrixElements = 100

// Client は Routes API を叩く。
type Client struct {
	api *googleapi.Client
}

func New(apiKey string, timeout time.Duration, hc *http.Client) *Client {
	return &Client{api: googleapi.New(apiKey, Provider, timeout, hc)}
}

// ComputeMatrix は会場から全候補への所要時間をまとめて求める。
//
// 返る要素の順序は保証されない。呼び出し側は DestinationIndex で引き当てること。
// 経路が求まらなかった要素も混ざる（OK() が偽）が、それは失敗ではなく
// 「そこへは行けない」という事実なので、そのまま返す。
func (c *Client) ComputeMatrix(ctx context.Context, q MatrixQuery) ([]MatrixElement, error) {
	if len(q.Destinations) == 0 {
		return nil, nil
	}
	if n := len(q.Destinations); n > maxMatrixElements {
		return nil, apperror.Internal(
			fmt.Errorf("経路行列の要素数が上限を超えました: %d > %d", n, maxMatrixElements),
			"googleroutes.ComputeMatrix")
	}
	if !q.Mode.Valid() {
		return nil, apperror.Internal(
			fmt.Errorf("未対応の移動手段です: %q", q.Mode), "googleroutes.ComputeMatrix")
	}

	dests := make([]matrixDestination, 0, len(q.Destinations))
	for _, d := range q.Destinations {
		dests = append(dests, matrixDestination{Waypoint: newWaypoint(d)})
	}

	body := matrixRequest{
		Origins:       []matrixOrigin{{Waypoint: newWaypoint(q.Origin)}},
		Destinations:  dests,
		TravelMode:    string(q.Mode),
		DepartureTime: departureTime(q.Mode, q.DepartureAt),
		LanguageCode:  orDefault(q.Language, "ja"),
		Units:         "METRIC",
	}

	// 応答は要素の JSON 配列（オブジェクトで包まれない）。
	var out []MatrixElement
	if err := c.api.PostJSON(ctx, matrixEndpoint, MatrixFieldMask, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ComputeRoute は確定した 1 区間の詳細を求める。
// **プランに採用された区間にだけ使う**。候補全件に投げると課金が跳ねる。
func (c *Client) ComputeRoute(ctx context.Context, q RouteQuery) (*Route, error) {
	if !q.Mode.Valid() {
		return nil, apperror.Internal(
			fmt.Errorf("未対応の移動手段です: %q", q.Mode), "googleroutes.ComputeRoute")
	}

	body := routeRequest{
		Origin:           newWaypoint(q.Origin),
		Destination:      newWaypoint(q.Destination),
		TravelMode:       string(q.Mode),
		DepartureTime:    departureTime(q.Mode, q.DepartureAt),
		LanguageCode:     orDefault(q.Language, "ja"),
		Units:            "METRIC",
		PolylineEncoding: "ENCODED_POLYLINE",
	}

	var res routeResponse
	if err := c.api.PostJSON(ctx, routeEndpoint, RouteFieldMask, body, &res); err != nil {
		return nil, err
	}
	if len(res.Routes) == 0 {
		// 「経路が無い」は上流の失敗ではない。呼び出し側が別の手段に切り替えられるよう
		// nil を返し、エラーにはしない。
		return nil, nil
	}
	return &res.Routes[0], nil
}

// departureTime は出発時刻を送ってよい移動手段だけで書式化する。
//
// WALK / BICYCLE に departureTime を付けると INVALID_ARGUMENT で
// **リクエストごと落ちる**。徒歩が主役のアプリなので、ここを間違えると全滅する。
func departureTime(mode model.TravelMode, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	switch mode {
	case model.TravelTransit, model.TravelDrive:
		return t.UTC().Format(time.RFC3339)
	}
	return ""
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// DirectionsURL は「地図で見る」リンクの URL を組み立てる。
//
// API キーを含まない公開 URL なので、フロントにそのまま出してよい。
// 座標ではなく placeId があればそちらを使う（駅ビル内の店で入口が正しく出る）。
func DirectionsURL(from, to model.Waypoint, mode model.TravelMode) string {
	q := url.Values{}
	q.Set("api", "1")
	q.Set("origin", waypointParam(from))
	q.Set("destination", waypointParam(to))
	q.Set("travelmode", travelModeParam(mode))
	if from.GooglePlaceID != "" {
		q.Set("origin_place_id", from.GooglePlaceID)
	}
	if to.GooglePlaceID != "" {
		q.Set("destination_place_id", to.GooglePlaceID)
	}
	return "https://www.google.com/maps/dir/?" + q.Encode()
}

func waypointParam(w model.Waypoint) string {
	if w.Location.Valid() {
		return strconv.FormatFloat(w.Location.Lat, 'f', 6, 64) + "," +
			strconv.FormatFloat(w.Location.Lng, 'f', 6, 64)
	}
	return w.Name
}

// travelModeParam は Routes API の語彙を Google マップ URL の語彙に写す。
// 同じ会社の同じ概念だが、値の綴りが違う。
func travelModeParam(m model.TravelMode) string {
	switch m {
	case model.TravelWalk:
		return "walking"
	case model.TravelTransit:
		return "transit"
	case model.TravelDrive:
		return "driving"
	case model.TravelBicycle:
		return "bicycling"
	}
	return "walking"
}
