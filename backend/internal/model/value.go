// Package model はドメインモデルを定義する。
//
// 不変のルール:
//   - **JSON タグを一切持たない**。API 表現は view/ の DTO が担う。
//     これにより DB カラムや外部 API の項目を足しても API 契約に漏れない。
//   - 時間は time.Duration、時刻は time.Time で持つ。分への変換は view の責務。
//   - 金額はすべて円の整数。
package model

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ── 座標 ──────────────────────────────────────────────

// Location は緯度経度。
type Location struct {
	Lat float64
	Lng float64
}

func (l Location) Valid() bool {
	return l.Lat >= -90 && l.Lat <= 90 && l.Lng >= -180 && l.Lng <= 180 && !l.IsZero()
}

// IsZero は未設定（0,0 のギニア湾）を示す。日本国内サービスなので 0,0 は常に未設定とみなす。
func (l Location) IsZero() bool { return l.Lat == 0 && l.Lng == 0 }

// DistanceMeters は大円距離（Haversine）を返す。
// Routes API を叩く前の粗い足切りに使い、確定した距離は必ず RouteFact の実測値を用いる。
func (l Location) DistanceMeters(to Location) int {
	const earthRadiusM = 6371000.0
	lat1, lat2 := l.Lat*math.Pi/180, to.Lat*math.Pi/180
	dLat := lat2 - lat1
	dLng := (to.Lng - l.Lng) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return int(math.Round(2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(a)))))
}

// Waypoint は経路の端点。
type Waypoint struct {
	Name          string
	Location      Location
	GooglePlaceID string
}

// ── 金額 ──────────────────────────────────────────────

// MoneyRangeJPY は円（整数）の範囲。Max が nil なら上限なし。
type MoneyRangeJPY struct {
	Min int
	Max *int
}

func (m MoneyRangeJPY) HasMax() bool { return m.Max != nil }

func (m MoneyRangeJPY) Contains(jpy int) bool {
	if jpy < m.Min {
		return false
	}
	return m.Max == nil || jpy <= *m.Max
}

// Valid は min <= max を検査する。
func (m MoneyRangeJPY) Valid() bool {
	if m.Min < 0 {
		return false
	}
	return m.Max == nil || *m.Max >= m.Min
}

// ── 時刻表記 ──────────────────────────────────────────

// ClockTime は 0 時起点の分数で表した時刻。
// 楽天トラベルのチェックイン最終時刻 "26:00"（翌 2 時）のような 24 時超え表記を
// 提供元仕様のまま保持するため、1440 以上の値を許容する。
type ClockTime int

// ParseClockTime は "15:00" / "26:00" を解釈する。
func ParseClockTime(s string) (ClockTime, bool) {
	s = strings.TrimSpace(s)
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	hh, err := strconv.Atoi(h)
	if err != nil || hh < 0 || hh > 47 {
		return 0, false
	}
	mm, err := strconv.Atoi(m)
	if err != nil || mm < 0 || mm > 59 {
		return 0, false
	}
	return ClockTime(hh*60 + mm), true
}

func (c ClockTime) String() string { return fmt.Sprintf("%02d:%02d", int(c)/60, int(c)%60) }

// On は基準日の 0 時に加算した絶対時刻を返す。26:00 は翌日 2 時に解決される。
func (c ClockTime) On(date time.Time) time.Time {
	y, m, d := date.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, date.Location()).Add(time.Duration(c) * time.Minute)
}

// ── ID 生成 ───────────────────────────────────────────

// crockford は ULID が使う Crockford Base32（I / L / O / U を除く）。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID は 48bit のミリ秒 + 80bit の乱数からなる 26 文字の ULID を返す。
// 生成順に辞書順が保たれるため、そのまま時系列ソートのキーになる。
// 外部依存を増やさないため自前実装している。
func newULID(t time.Time) string {
	var b [16]byte
	ms := uint64(t.UTC().UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand の失敗は復旧不能。ID の一意性が壊れた状態で走らせるより落とす。
		panic("model: 乱数の取得に失敗しました: " + err.Error())
	}
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])

	out := make([]byte, 26)
	// 128bit を上位 2bit のゼロ埋めで 130bit とみなし、上位から 5bit ずつ取り出す。
	for i := 0; i < 26; i++ {
		shift := uint(125 - 5*i)
		var v uint64
		if shift >= 64 {
			v = hi >> (shift - 64)
		} else {
			v = lo>>shift | hi<<(64-shift)
		}
		out[i] = crockford[v&0x1f]
	}
	return string(out)
}
