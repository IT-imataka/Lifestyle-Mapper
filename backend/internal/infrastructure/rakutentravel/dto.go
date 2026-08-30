// Package rakutentravel は楽天トラベルの空室検索（VacantHotelSearch）を叩く。
//
// 他の 2 社と決定的に違う点が 3 つある。
//
//	① 座標の単位が「度」ではなく「秒」。35.7056 度は 128540.16 秒として送る。
//	   ここを間違えると赤道上の海を検索して常に 0 件になる（しかもエラーは出ない）。
//	② 0 件が HTTP 404 + error:"not_found" で返る。**これは失敗ではない**。
//	   満室の日に 500 を返すのは事実と違う。
//	③ 概ね 1req/sec のレート制限がある。並行 fan-out の中でここだけ直列化する。
//
// 予約 URL は自分で組み立てず、必ず API が返した値を使う。applicationId に
// affiliateId を添えて呼ぶと、応答の URL 群がアフィリエイト URL に差し替わる。
// 手で組むと ID の 1 文字違いで**収益が黙ってゼロになる**。
package rakutentravel

import (
	"strconv"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// endpoint はバージョン日付込みの固定 URL。日付部分がインタフェースの版そのもの。
const endpoint = "https://app.rakuten.co.jp/services/api/Travel/VacantHotelSearch/20170426"

// Provider はエラーと観測情報に載る識別子。
const Provider = "rakuten_travel"

// 検索半径の許容範囲（km）。範囲外を送るとリクエストごと弾かれる。
const (
	minSearchRadiusKm = 0.1
	maxSearchRadiusKm = 3.0
)

// maxHits は 1 ページあたりの取得件数の上限。
const maxHits = 30

// VacantQuery は 1 回の空室検索。
type VacantQuery struct {
	// Center は会場座標（度）。送信時に秒へ変換する。
	Center model.Location
	// RadiusKm は 0.1〜3.0。範囲外はクランプする。
	RadiusKm float64

	CheckIn  time.Time
	CheckOut time.Time

	// Adults は 1 室あたりの大人人数。楽天の adultNum に対応する。
	Adults int
	Rooms  int

	// MinChargePerPerson / MaxChargePerPerson は **1 名 1 泊あたり**の料金。
	// 検索条件の予算は 1 泊の総額なので、人数で割ってから渡すこと。
	MinChargePerPerson int
	MaxChargePerPerson int

	Hits int
}

// values は API のクエリ文字列を組み立てる。
func (q VacantQuery) values(applicationID, affiliateID string) map[string]string {
	v := map[string]string{
		"format":        "json",
		"applicationId": applicationID,
		// datumType=1 は世界測地系。Google 由来の座標をそのまま使うので必須。
		// 既定は日本測地系で、指定を忘れると数百 m ずれる。
		"datumType":    "1",
		"latitude":     formatSeconds(q.Center.Lat),
		"longitude":    formatSeconds(q.Center.Lng),
		"searchRadius": strconv.FormatFloat(clampRadius(q.RadiusKm), 'f', 1, 64),
		"checkinDate":  q.CheckIn.Format("2006-01-02"),
		"checkoutDate": q.CheckOut.Format("2006-01-02"),
		// large でないと roomInfo（プラン別の料金）が返らず、価格が出せない。
		"responseType": "large",
		"hits":         strconv.Itoa(clampHits(q.Hits)),
	}
	if affiliateID != "" {
		v["affiliateId"] = affiliateID
	}
	if q.Adults > 0 {
		v["adultNum"] = strconv.Itoa(q.Adults)
	}
	if q.Rooms > 0 {
		v["roomNum"] = strconv.Itoa(q.Rooms)
	}
	if q.MinChargePerPerson > 0 {
		v["minCharge"] = strconv.Itoa(q.MinChargePerPerson)
	}
	if q.MaxChargePerPerson > 0 {
		v["maxCharge"] = strconv.Itoa(q.MaxChargePerPerson)
	}
	return v
}

func clampRadius(km float64) float64 {
	switch {
	case km < minSearchRadiusKm:
		return minSearchRadiusKm
	case km > maxSearchRadiusKm:
		return maxSearchRadiusKm
	}
	return km
}

func clampHits(n int) int {
	if n < 1 || n > maxHits {
		return maxHits
	}
	return n
}

// ── 座標の単位変換 ────────────────────────────────────

// secondsPerDegree は 1 度あたりの秒数。
const secondsPerDegree = 3600.0

// formatSeconds は度を秒表記の文字列にする。
func formatSeconds(deg float64) string {
	return strconv.FormatFloat(deg*secondsPerDegree, 'f', 2, 64)
}

// DegreesFromSeconds は応答の秒表記を度に戻す。
// 応答側も秒なので、正規化の前に必ず通すこと。
func DegreesFromSeconds(sec float64) float64 { return sec / secondsPerDegree }

// ── 応答 ──────────────────────────────────────────────

// vacantResponse は API の応答本文。
//
// hotels の要素が「1 要素だけのオブジェクトの配列」という独特な形をしている。
// hotel[0] に hotelBasicInfo、以降に hotelRatingInfo / hotelDetailInfo / roomInfo が
// 順不同で並ぶため、キーの有無で振り分ける。
type vacantResponse struct {
	PagingInfo pagingInfo `json:"pagingInfo"`
	Hotels     []struct {
		Hotel []hotelElement `json:"hotel"`
	} `json:"hotels"`
}

type pagingInfo struct {
	RecordCount int `json:"recordCount"`
	PageCount   int `json:"pageCount"`
	Page        int `json:"page"`
}

type hotelElement struct {
	HotelBasicInfo  *HotelBasicInfo  `json:"hotelBasicInfo"`
	HotelRatingInfo *HotelRatingInfo `json:"hotelRatingInfo"`
	HotelDetailInfo *HotelDetailInfo `json:"hotelDetailInfo"`
	RoomInfo        []roomElement    `json:"roomInfo"`
}

type roomElement struct {
	RoomBasicInfo *RoomBasicInfo `json:"roomBasicInfo"`
	DailyCharge   *DailyCharge   `json:"dailyCharge"`
}

// Hotel は 1 ホテルぶんを平坦化したもの。client が組み立てて返す。
type Hotel struct {
	Basic  HotelBasicInfo
	Rating *HotelRatingInfo
	// Detail はチェックイン時刻を持つが、**応答に含まれないことがある**。
	// nil のときは「不明」であって「制限なし」ではない。
	Detail *HotelDetailInfo
	Rooms  []Room
}

// Room は 1 プラン（部屋 × 料金プラン）と、その日別料金。
type Room struct {
	Basic   RoomBasicInfo
	Charges []DailyCharge
}

// TotalJPY は宿泊期間の合計金額を返す。日別料金の合計であり、
// 提供元が合計を返さない日程でも自分で足し上げられるようにしている。
func (r Room) TotalJPY() int {
	total := 0
	for _, c := range r.Charges {
		total += c.Total
	}
	return total
}

// PerPersonJPY は 1 名あたりの合計を返す。取れなければ false。
func (r Room) PerPersonJPY() (int, bool) {
	sum, found := 0, false
	for _, c := range r.Charges {
		if c.RakutenCharge > 0 {
			sum += c.RakutenCharge
			found = true
		}
	}
	return sum, found
}

// HotelBasicInfo はホテルの基本情報。
//
// 各 URL は affiliateId 付きで呼べばアフィリエイト URL になっている。
// **この文字列をそのまま使うこと**。自前で組み立て直さない。
type HotelBasicInfo struct {
	HotelNo             int     `json:"hotelNo"`
	HotelName           string  `json:"hotelName"`
	HotelInformationURL string  `json:"hotelInformationUrl"`
	PlanListURL         string  `json:"planListUrl"`
	ReviewCount         int     `json:"reviewCount"`
	ReviewAverage       float64 `json:"reviewAverage"`
	HotelMinCharge      int     `json:"hotelMinCharge"`
	// Latitude / Longitude は秒。度に直すには DegreesFromSeconds を通す。
	Latitude          float64 `json:"latitude"`
	Longitude         float64 `json:"longitude"`
	PostalCode        string  `json:"postalCode"`
	Address1          string  `json:"address1"`
	Address2          string  `json:"address2"`
	TelephoneNo       string  `json:"telephoneNo"`
	Access            string  `json:"access"`
	NearestStation    string  `json:"nearestStation"`
	HotelImageURL     string  `json:"hotelImageUrl"`
	HotelThumbnailURL string  `json:"hotelThumbnailUrl"`
	HotelSpecial      string  `json:"hotelSpecial"`
}

// Address は住所を 1 本にまとめる。楽天は都道府県（address1）と
// 市区町村以降（address2）を分けて返す。
func (h HotelBasicInfo) Address() string { return h.Address1 + h.Address2 }

// Location は座標を度に直して返す。
func (h HotelBasicInfo) Location() model.Location {
	return model.Location{
		Lat: DegreesFromSeconds(h.Latitude),
		Lng: DegreesFromSeconds(h.Longitude),
	}
}

// ProviderID は施設番号を文字列にしたもの。FactStore の重複判定キーになる。
func (h HotelBasicInfo) ProviderID() string { return strconv.Itoa(h.HotelNo) }

type HotelRatingInfo struct {
	ServiceAverage   float64 `json:"serviceAverage"`
	LocationAverage  float64 `json:"locationAverage"`
	RoomAverage      float64 `json:"roomAverage"`
	EquipmentAverage float64 `json:"equipmentAverage"`
	BathAverage      float64 `json:"bathAverage"`
	MealAverage      float64 `json:"mealAverage"`
}

// HotelDetailInfo はチェックイン・チェックアウト時刻。
// LastCheckInTime が "26:00" のような 24 時超え表記で来るため、
// 文字列のまま受けて model.ParseClockTime に解釈させる。
type HotelDetailInfo struct {
	AreaName        string `json:"areaName"`
	CheckinTime     string `json:"checkinTime"`
	CheckoutTime    string `json:"checkoutTime"`
	LastCheckInTime string `json:"lastCheckInTime"`
}

// RoomBasicInfo は料金プラン 1 件。
type RoomBasicInfo struct {
	RoomClass         string `json:"roomClass"`
	RoomName          string `json:"roomName"`
	PlanID            int64  `json:"planId"`
	PlanName          string `json:"planName"`
	PointRate         int    `json:"pointRate"`
	WithDiningFlag    int    `json:"withDiningFlag"`
	WithBreakfastFlag int    `json:"withBreakfastFlag"`
	WithDinnerFlag    int    `json:"withDinnerFlag"`
	Payment           string `json:"payment"`
	// ReserveURL は予約ページ。affiliateId 付きで呼べばアフィリエイト URL になる。
	ReserveURL string `json:"reserveUrl"`
	// SalesformFlag が 1 なら「残りわずか」。楽天は残室数そのものを返さないため、
	// 残室 2 室といった数字は出せない。出せないものを推測して出さない。
	SalesformFlag int `json:"salesformFlag"`
}

func (r RoomBasicInfo) ProviderPlanID() string { return strconv.FormatInt(r.PlanID, 10) }

// DailyCharge は 1 泊ぶんの料金。
type DailyCharge struct {
	StayDate string `json:"stayDate"`
	// RakutenCharge は 1 名あたり、Total は合計。
	RakutenCharge int `json:"rakutenCharge"`
	Total         int `json:"total"`
	ChargeFlag    int `json:"chargeFlag"`
}

// errorResponse は楽天のエラー本文。HTTP 4xx とともに返る。
type errorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// errNotFound は「条件に合う空室が無い」を示す楽天のエラー語。
// 業務上の 0 件であり、システム障害ではない。
const errNotFound = "not_found"
