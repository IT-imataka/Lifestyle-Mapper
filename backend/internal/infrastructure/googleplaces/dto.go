// Package googleplaces は Google Places API (New) の searchNearby を叩く。
//
// この層の責務は「HTTP を往復して JSON を構造体に載せる」ところまで。
// 提供元の語彙（PRICE_LEVEL_MODERATE / types / 曜日と時分）をドメインの語彙に
// 訳すのは service/plan/normalizer.go の仕事で、ここでは訳さない。
// 提供元の表現をそのまま保つことで、「API が実際に何を返したか」を
// 障害調査でそのまま読めるようにしている。
package googleplaces

import "github.com/taka/lifestyle-mapper/backend/internal/model"

// FieldMask は X-Goog-FieldMask に送る取得フィールド。
//
// **課金 SKU がこの内容で決まる**ため、増やすときは必ず単価を確認すること。
// 要らないフィールドを 1 つ足すだけで SKU が上位に跳ね、月額が数倍になる。
//
// 営業時間に regularOpeningHours を使い currentOpeningHours を使わないのは、
// 対象が「数週間先の公演日の深夜」だから。currentOpeningHours は今週ぶんしか
// 返らないので、来月の公演では常に空になり、深夜営業の店を全部落としてしまう。
// 曜日ベースの regularOpeningHours を公演日に解決するほうが正しい。
const FieldMask = "places.id," +
	"places.displayName," +
	"places.formattedAddress," +
	"places.location," +
	"places.rating," +
	"places.userRatingCount," +
	"places.priceLevel," +
	"places.types," +
	"places.nationalPhoneNumber," +
	"places.regularOpeningHours," +
	"places.photos"

// maxResultCount の API 上限。これを超える値を送るとリクエストごと 400 になる。
const maxResultCount = 20

// NearbyQuery は 1 回の searchNearby。
type NearbyQuery struct {
	// Center / RadiusMeters は会場を中心とした円。徒歩圏の上限から算出する。
	Center       model.Location
	RadiusMeters float64
	// IncludedTypes は Places の type（例: restaurant / bar / cafe）。
	// 空なら提供元の既定に任せる。
	IncludedTypes []string
	// MaxResults は 1〜20。0 なら 20。
	MaxResults int
	// LanguageCode / RegionCode は表示名と住所の言語。既定は ja / JP。
	LanguageCode string
	RegionCode   string
}

// searchNearbyRequest は API のリクエスト本文。
type searchNearbyRequest struct {
	IncludedTypes       []string            `json:"includedTypes,omitempty"`
	MaxResultCount      int                 `json:"maxResultCount"`
	LanguageCode        string              `json:"languageCode,omitempty"`
	RegionCode          string              `json:"regionCode,omitempty"`
	RankPreference      string              `json:"rankPreference,omitempty"`
	LocationRestriction locationRestriction `json:"locationRestriction"`
}

type locationRestriction struct {
	Circle circle `json:"circle"`
}

type circle struct {
	Center LatLng  `json:"center"`
	Radius float64 `json:"radius"`
}

// LatLng は Places / Routes 共通の座標表現。
type LatLng struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

func (l LatLng) Model() model.Location { return model.Location{Lat: l.Latitude, Lng: l.Longitude} }

// searchNearbyResponse は API の応答本文。
// 0 件のとき places は省略される（空配列ではなくキーごと無い）。
type searchNearbyResponse struct {
	Places []Place `json:"places"`
}

// Place は searchNearby が返す 1 件。FieldMask で要求したフィールドのみ埋まる。
type Place struct {
	ID                  string        `json:"id"`
	DisplayName         LocalizedText `json:"displayName"`
	FormattedAddress    string        `json:"formattedAddress"`
	Location            LatLng        `json:"location"`
	Rating              *float64      `json:"rating"`
	UserRatingCount     *int          `json:"userRatingCount"`
	PriceLevel          string        `json:"priceLevel"`
	Types               []string      `json:"types"`
	NationalPhoneNumber string        `json:"nationalPhoneNumber"`
	RegularOpeningHours *OpeningHours `json:"regularOpeningHours"`
	Photos              []Photo       `json:"photos"`
}

type LocalizedText struct {
	Text         string `json:"text"`
	LanguageCode string `json:"languageCode"`
}

// OpeningHours は曜日で繰り返す営業時間。
type OpeningHours struct {
	Periods []Period `json:"periods"`
	// WeekdayDescriptions は「月曜日: 17:00～翌 2:00」のような人間向け表記。
	// 判定には使わないが、営業時間の解釈を誤ったときの突き合わせに役立つ。
	WeekdayDescriptions []string `json:"weekdayDescriptions"`
}

// Period は開店から閉店までの 1 区間。
// Close が nil なら 24 時間営業（提供元は閉店点を省略する）。
type Period struct {
	Open  TimePoint  `json:"open"`
	Close *TimePoint `json:"close"`
}

// TimePoint は曜日と時分。Day は 0=日曜〜6=土曜。
//
// 深夜 2 時まで営業の店は Open{Day:1,Hour:17} → Close{Day:2,Hour:2} のように
// **閉店側の曜日が翌日になる**。この日跨ぎを潰すと深夜営業の店が全滅するので、
// 正規化では Day の差をそのまま日数として扱う。
type TimePoint struct {
	Day    int `json:"day"`
	Hour   int `json:"hour"`
	Minute int `json:"minute"`
}

// Photo は写真の参照。name は "places/xxx/photos/yyy" 形式。
//
// 表示 URL をここで組み立てないのは、Places の写真 URL に API キーが載るため。
// 自前 CDN 経由に差し替える責務は view 側にある。
type Photo struct {
	Name     string `json:"name"`
	WidthPx  int    `json:"widthPx"`
	HeightPx int    `json:"heightPx"`
}
