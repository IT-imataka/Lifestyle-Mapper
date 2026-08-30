// Package googleroutes は Google Routes API の computeRouteMatrix と computeRoutes を叩く。
//
// **2 段構えが課金の要**。候補ごとに computeRoutes を N 回投げると単価の高い
// リクエストが N 倍になるので、
//
//	① computeRouteMatrix で「会場 → 全候補」を 1 リクエストにまとめ、粗い所要時間を得る
//	② 実際にプランへ採用された区間だけ computeRoutes で詳細（経路線・乗換）を取る
//
// という順にする。①は足切りのため、②は描画のため、と目的が違う。
package googleroutes

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// MatrixFieldMask は computeRouteMatrix の取得フィールド。
//
// condition を必ず含めること。**経路が存在しない要素でも duration は 0 で返る**ため、
// condition を見ないと「徒歩 0 分の隣」と誤認して、行けない候補を最上位に置いてしまう。
const MatrixFieldMask = "originIndex,destinationIndex,duration,distanceMeters,status,condition"

// RouteFieldMask は computeRoutes の取得フィールド。
// legs.steps.transitDetails は乗換案内の表示と終電判定に要る。
const RouteFieldMask = "routes.duration," +
	"routes.distanceMeters," +
	"routes.polyline.encodedPolyline," +
	"routes.legs.steps.transitDetails"

// ── 共通の座標表現 ────────────────────────────────────

type LatLng struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type location struct {
	LatLng LatLng `json:"latLng"`
}

type waypoint struct {
	Location *location `json:"location,omitempty"`
	// PlaceID があれば座標より優先される。会場は googlePlaceId を持つことがあり、
	// 建物の出入口に寄った経路になるぶん実測に近い。
	PlaceID string `json:"placeId,omitempty"`
}

func newWaypoint(w model.Waypoint) waypoint {
	if w.GooglePlaceID != "" {
		return waypoint{PlaceID: w.GooglePlaceID}
	}
	return waypoint{Location: &location{LatLng{Latitude: w.Location.Lat, Longitude: w.Location.Lng}}}
}

// ── computeRouteMatrix ────────────────────────────────

// MatrixQuery は「1 つの起点から複数の終点へ」を 1 リクエストで問う。
type MatrixQuery struct {
	Origin       model.Waypoint
	Destinations []model.Waypoint
	Mode         model.TravelMode
	// DepartureAt は出発時刻。TRANSIT / DRIVE でのみ意味を持つ。
	// **WALK に出発時刻を付けると API が 400 を返す**ので、送信前に落とす。
	DepartureAt time.Time
	Language    string
}

type matrixRequest struct {
	Origins       []matrixOrigin      `json:"origins"`
	Destinations  []matrixDestination `json:"destinations"`
	TravelMode    string              `json:"travelMode"`
	DepartureTime string              `json:"departureTime,omitempty"`
	LanguageCode  string              `json:"languageCode,omitempty"`
	Units         string              `json:"units,omitempty"`
}

type matrixOrigin struct {
	Waypoint waypoint `json:"waypoint"`
}

type matrixDestination struct {
	Waypoint waypoint `json:"waypoint"`
}

// MatrixElement は「起点 i から終点 j へ」の 1 要素。
//
// 応答は要素の JSON 配列で、**順序は保証されない**（サーバ側が求まった順に流す）。
// 必ず DestinationIndex で引き当てること。
type MatrixElement struct {
	OriginIndex      int           `json:"originIndex"`
	DestinationIndex int           `json:"destinationIndex"`
	DistanceMeters   int           `json:"distanceMeters"`
	Duration         string        `json:"duration"`
	Condition        string        `json:"condition"`
	Status           ElementStatus `json:"status"`
}

// ConditionRouteExists は経路が求まったことを示す唯一の値。
const ConditionRouteExists = "ROUTE_EXISTS"

// OK は使える要素かを返す。status.code が 0 以外なら要素単位の失敗で、
// 他の要素は生きている（**1 件の失敗で全体を捨てない**）。
func (e MatrixElement) OK() bool {
	return e.Status.Code == 0 && e.Condition == ConditionRouteExists
}

// TravelDuration は "480s" 形式の所要時間を解釈する。
func (e MatrixElement) TravelDuration() (time.Duration, error) { return ParseDuration(e.Duration) }

// ElementStatus は要素単位の失敗。Code が 0 以外なら、その終点だけが
// 失敗していて他の要素は生きている。
//
// MatrixElement の公開フィールドなので型も公開する。非公開のままだと
// service 側のテストが「1 件だけ失敗した行列」を組み立てられない。
type ElementStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ── computeRoutes ─────────────────────────────────────

// RouteQuery は確定した 1 区間の詳細を問う。
type RouteQuery struct {
	Origin      model.Waypoint
	Destination model.Waypoint
	Mode        model.TravelMode
	DepartureAt time.Time
	Language    string
}

type routeRequest struct {
	Origin        waypoint `json:"origin"`
	Destination   waypoint `json:"destination"`
	TravelMode    string   `json:"travelMode"`
	DepartureTime string   `json:"departureTime,omitempty"`
	LanguageCode  string   `json:"languageCode,omitempty"`
	Units         string   `json:"units,omitempty"`
	// PolylineEncoding は既定でも encoded だが、既定値の変更に巻き込まれないよう明示する。
	PolylineEncoding string `json:"polylineEncoding,omitempty"`
}

type routeResponse struct {
	Routes []Route `json:"routes"`
}

// Route は求まった経路 1 本。
type Route struct {
	DistanceMeters int      `json:"distanceMeters"`
	Duration       string   `json:"duration"`
	Polyline       polyline `json:"polyline"`
	Legs           []Leg    `json:"legs"`
}

func (r Route) TravelDuration() (time.Duration, error) { return ParseDuration(r.Duration) }

// EncodedPolyline は地図描画用の折れ線。
func (r Route) EncodedPolyline() string { return r.Polyline.EncodedPolyline }

type polyline struct {
	EncodedPolyline string `json:"encodedPolyline"`
}

type Leg struct {
	Steps []Step `json:"steps"`
}

// Step は経路の 1 手順。TransitDetails が非 nil の手順だけが乗車区間。
type Step struct {
	TransitDetails *TransitDetails `json:"transitDetails"`
}

// TransitDetails は公共交通の乗車区間。終電判定はここの発車時刻を見る。
type TransitDetails struct {
	StopDetails StopDetails `json:"stopDetails"`
	Headsign    string      `json:"headsign"`
	TransitLine TransitLine `json:"transitLine"`
	StopCount   int         `json:"stopCount"`
}

type StopDetails struct {
	DepartureStop stop   `json:"departureStop"`
	DepartureTime string `json:"departureTime"`
	ArrivalStop   stop   `json:"arrivalStop"`
	ArrivalTime   string `json:"arrivalTime"`
}

type stop struct {
	Name string `json:"name"`
}

// TransitLine は路線。Name が「JR中央・総武線」、NameShort が「総武線」のように入る。
type TransitLine struct {
	Name      string `json:"name"`
	NameShort string `json:"nameShort"`
}

// DisplayName は表示に使う路線名。短縮名があればそちらを優先する。
func (l TransitLine) DisplayName() string {
	if l.NameShort != "" {
		return l.NameShort
	}
	return l.Name
}

// ── 書式 ──────────────────────────────────────────────

// ParseDuration は protobuf Duration の "480s" / "480.5s" を解釈する。
// time.ParseDuration をそのまま使えるが、秒以外の単位が来たら仕様変更なので弾く。
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("所要時間が空です")
	}
	if !strings.HasSuffix(s, "s") {
		return 0, fmt.Errorf("秒表記ではない所要時間です: %q", s)
	}
	sec, err := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64)
	if err != nil {
		return 0, fmt.Errorf("所要時間を解釈できません: %q", s)
	}
	if sec < 0 {
		return 0, fmt.Errorf("所要時間が負です: %q", s)
	}
	return time.Duration(sec * float64(time.Second)), nil
}

// parseTime は RFC3339 の時刻を解釈する。空や不正はゼロ値で返す。
// 発車時刻が取れないことは珍しくなく、**それだけで区間を捨てるほうが損**なので、
// 呼び出し側はゼロ値を「不明」として扱う。
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// TransitLegs は経路から乗車区間だけを取り出し、ドメインの表現に写す。
// 提供元の構造（legs → steps → transitDetails）を service に漏らさないための
// 例外的な変換で、ここだけは model を返す。
func (r Route) TransitLegs() []model.TransitLeg {
	var out []model.TransitLeg
	for _, leg := range r.Legs {
		for _, step := range leg.Steps {
			d := step.TransitDetails
			if d == nil {
				continue
			}
			stops := d.StopCount
			out = append(out, model.TransitLeg{
				LineName:      d.TransitLine.DisplayName(),
				Headsign:      d.Headsign,
				DepartureStop: d.StopDetails.DepartureStop.Name,
				ArrivalStop:   d.StopDetails.ArrivalStop.Name,
				DepartureAt:   parseTime(d.StopDetails.DepartureTime),
				ArrivalAt:     parseTime(d.StopDetails.ArrivalTime),
				NumStops:      &stops,
			})
		}
	}
	return out
}
