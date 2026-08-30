package model

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ── 候補 ID ───────────────────────────────────────────

// CandidateCategory は候補の種別。CandidateID の一部になる。
type CandidateCategory string

const (
	CategoryDining  CandidateCategory = "dining"
	CategoryLodging CandidateCategory = "lodging"
	CategorySpot    CandidateCategory = "spot"
)

func (c CandidateCategory) Valid() bool {
	switch c {
	case CategoryDining, CategoryLodging, CategorySpot:
		return true
	}
	return false
}

// CandidateID は FactStore が採番する候補 ID。
// **LLM が参照を許される唯一の識別子**であり、Hydrator は FactStore に
// 存在しない ID を含む LLM 出力を棄却する（幻覚の遮断点）。
type CandidateID string

var candidateIDPattern = regexp.MustCompile(`^cand_(dining|lodging|spot)_[0-9]{3}$`)

// NewCandidateID は cand_dining_003 形式の ID を組み立てる。seq は 1..999。
func NewCandidateID(cat CandidateCategory, seq int) (CandidateID, error) {
	if !cat.Valid() {
		return "", fmt.Errorf("未知の候補種別です: %q", cat)
	}
	if seq < 1 || seq > 999 {
		return "", fmt.Errorf("候補の連番が範囲外です: %d", seq)
	}
	return CandidateID(fmt.Sprintf("cand_%s_%03d", cat, seq)), nil
}

func (id CandidateID) Valid() bool { return candidateIDPattern.MatchString(string(id)) }

// Category は ID からカテゴリを取り出す。不正な ID では空文字を返す。
func (id CandidateID) Category() CandidateCategory {
	parts := strings.Split(string(id), "_")
	if len(parts) != 3 || parts[0] != "cand" {
		return ""
	}
	cat := CandidateCategory(parts[1])
	if !cat.Valid() {
		return ""
	}
	return cat
}

// Fact は FactStore が保持する検証済みの事実。
// LLM はこの中身を書き換えられず、CandidateID による参照しかできない。
type Fact interface {
	CandidateID() CandidateID
	Category() CandidateCategory
	DisplayName() string
	Coordinates() Location
}

var (
	_ Fact = (*PlaceFact)(nil)
	_ Fact = (*HotelFact)(nil)
)

// ── 飲食・スポット（Google Places API 由来） ──────────

// PlaceFact は Places API の生レスポンスを正規化した事実。
type PlaceFact struct {
	ID              CandidateID
	Cat             CandidateCategory // dining または spot
	ProviderPlaceID string            // Places の place id
	Name            string
	Genres          []string // Places の types を自前の分類語に写像したもの
	Rating          *float64
	UserRatingCount *int
	PriceLevel      *int // 0..4
	// EstimatedCostPerPersonJPY は PriceLevel から推定した 1 人あたりの目安額。
	// 提供元が実額を返さないため推定値であり、UI では「目安」と明示する。
	EstimatedCostPerPersonJPY *int
	Address                   string
	Loc                       Location
	// PhotoRef は Places の写真参照名。表示用 URL は自前 CDN 経由で組み立てるため、
	// ここに完成 URL は持たない（API キー秘匿とキャッシュのため）。
	PhotoRef    string
	PhoneNumber string
	Hours       OpeningHours
	FetchedAt   time.Time
	Cached      bool
}

func (f *PlaceFact) CandidateID() CandidateID    { return f.ID }
func (f *PlaceFact) Category() CandidateCategory { return f.Cat }
func (f *PlaceFact) DisplayName() string         { return f.Name }
func (f *PlaceFact) Coordinates() Location       { return f.Loc }
func (f *PlaceFact) HasGenre(genre string) bool  { return containsString(f.Genres, genre) }
func (f *PlaceFact) CostFor(guests int) int {
	if f.EstimatedCostPerPersonJPY == nil || guests < 1 {
		return 0
	}
	return *f.EstimatedCostPerPersonJPY * guests
}

// OpeningPeriod は営業区間 [Open, Close)。日をまたぐ深夜営業は Close が翌日になる。
type OpeningPeriod struct {
	Open  time.Time
	Close time.Time
}

// OpeningHours は対象日の営業時間。
// Known が false は「提供元が営業時間を返さなかった」であり、「終日休業」ではない。
type OpeningHours struct {
	Known   bool
	Periods []OpeningPeriod
}

// IsOpenAt は指定時刻に営業しているかを返す。
// 判定対象は **到着予定時刻**であって現在時刻ではない（終演後の深夜が本番のため）。
func (h OpeningHours) IsOpenAt(t time.Time) bool {
	if !h.Known {
		return false
	}
	for _, p := range h.Periods {
		if !t.Before(p.Open) && t.Before(p.Close) {
			return true
		}
	}
	return false
}

// ClosesAfter は t を含む営業区間の閉店時刻を返す。
func (h OpeningHours) ClosesAfter(t time.Time) (time.Time, bool) {
	for _, p := range h.Periods {
		if !t.Before(p.Open) && t.Before(p.Close) {
			return p.Close, true
		}
	}
	return time.Time{}, false
}

// CanStay は到着時刻から希望滞在時間ぶん居られるかを返す。
// 「23 時閉店の店に 22:50 着」のような実質入れない候補をスコアラで落とすために使う。
func (h OpeningHours) CanStay(arrival time.Time, stay time.Duration) bool {
	closesAt, ok := h.ClosesAfter(arrival)
	if !ok {
		return false
	}
	return !arrival.Add(stay).After(closesAt)
}

// ── 宿泊（楽天トラベル API 由来） ─────────────────────

// VacancyStatus は空室状況。
type VacancyStatus string

const (
	VacancyAvailable VacancyStatus = "available"
	VacancyFewLeft   VacancyStatus = "few_left"
	VacancySoldOut   VacancyStatus = "sold_out"
)

// HotelFact は楽天トラベルの空室検索結果を正規化した事実。
type HotelFact struct {
	ID              CandidateID
	ProviderHotelID string
	Name            string
	ReviewAverage   *float64
	ReviewCount     *int
	Address         string
	Loc             Location
	PhotoURL        string
	// InformationURL は提供元が返した施設ページの URL。
	// affiliateId 付きで検索していればアフィリエイト URL になっている。
	// **自前で組み立て直さない**。ID が 1 文字違うだけで収益がゼロになる。
	InformationURL string
	// Plans は 1 ホテルの複数プラン。どれを採るかは scorer が決める。
	Plans     []HotelPlanFact
	FetchedAt time.Time
	Cached    bool
}

func (f *HotelFact) CandidateID() CandidateID    { return f.ID }
func (f *HotelFact) Category() CandidateCategory { return CategoryLodging }
func (f *HotelFact) DisplayName() string         { return f.Name }
func (f *HotelFact) Coordinates() Location       { return f.Loc }

// CheapestPlan は満室でないプランのうち最安のものを返す。
func (f *HotelFact) CheapestPlan() *HotelPlanFact {
	var best *HotelPlanFact
	for i := range f.Plans {
		p := &f.Plans[i]
		if p.VacancyStatus == VacancySoldOut {
			continue
		}
		if best == nil || p.TotalPriceJPY < best.TotalPriceJPY {
			best = p
		}
	}
	return best
}

// HotelPlanFact は宿泊プラン 1 件。価格・残室数は提供元の実測値であり LLM 不可侵。
type HotelPlanFact struct {
	ProviderPlanID    string
	PlanName          string
	RoomName          string
	TotalPriceJPY     int
	PricePerPersonJPY *int
	VacancyStatus     VacancyStatus
	// RemainingRooms は残室数。楽天は数そのものを返さないため通常は nil。
	// 「残りわずか」は VacancyStatus 側で表す。数えられないものを推測して埋めない。
	RemainingRooms *int
	// ReserveURL は提供元が返した予約ページの URL（アフィリエイト URL）。
	ReserveURL  string
	CheckInTime *ClockTime
	// CheckInDeadline は 26:00 のような 24 時超え表記を保持する。
	// 終演が遅い公演で「居酒屋を出てから間に合うか」の判定に直結する。
	CheckInDeadline *ClockTime
	CheckOutTime    *ClockTime
	Amenities       []string
}

func (p *HotelPlanFact) HasAmenity(a string) bool { return containsString(p.Amenities, a) }

// CheckInDeadlineAt は宿泊日を与えて最終チェックイン時刻を絶対時刻に解決する。
func (p *HotelPlanFact) CheckInDeadlineAt(stayDate time.Time) (time.Time, bool) {
	if p.CheckInDeadline == nil {
		return time.Time{}, false
	}
	return p.CheckInDeadline.On(stayDate), true
}

// CheckOutAt は宿泊日を与えてチェックアウト時刻（翌日）を絶対時刻に解決する。
func (p *HotelPlanFact) CheckOutAt(stayDate time.Time) (time.Time, bool) {
	if p.CheckOutTime == nil {
		return time.Time{}, false
	}
	return p.CheckOutTime.On(stayDate.AddDate(0, 0, 1)), true
}

// ── 経路（Google Routes API 由来） ────────────────────

// TravelMode は Google Routes API の travelMode に対応する。
type TravelMode string

const (
	TravelWalk    TravelMode = "WALK"
	TravelTransit TravelMode = "TRANSIT"
	TravelDrive   TravelMode = "DRIVE"
	TravelBicycle TravelMode = "BICYCLE"
)

func (m TravelMode) Valid() bool {
	switch m {
	case TravelWalk, TravelTransit, TravelDrive, TravelBicycle:
		return true
	}
	return false
}

// RouteOriginVenue は経路の起点が会場であることを示す予約語。
const RouteOriginVenue = "venue"

// RouteKey は経路の同一性。RouteMatrix の結果をここに引き当てる。
// From / To は RouteOriginVenue か CandidateID の文字列表現。
type RouteKey struct {
	From string
	To   string
	Mode TravelMode
}

func NewRouteKey(from, to string, mode TravelMode) RouteKey {
	return RouteKey{From: from, To: to, Mode: mode}
}

// RouteFact は Routes API の実測経路。距離・所要時間は LLM 不可侵。
type RouteFact struct {
	Key             RouteKey
	From            Waypoint
	To              Waypoint
	DistanceMeters  int
	Duration        time.Duration
	EncodedPolyline string
	MapsURL         string
	TransitLegs     []TransitLeg
	FareJPY         *int
	FetchedAt       time.Time
	Cached          bool
}

func (r *RouteFact) Mode() TravelMode { return r.Key.Mode }

// WithinWalkLimit は徒歩の上限時間に収まるかを返す。徒歩以外は常に true（上限は徒歩にのみ課す）。
func (r *RouteFact) WithinWalkLimit(limit time.Duration) bool {
	if r.Key.Mode != TravelWalk {
		return true
	}
	return r.Duration <= limit
}

// FirstDeparture は最初の公共交通区間の発車時刻を返す。終電判定に使う。
func (r *RouteFact) FirstDeparture() (time.Time, bool) {
	for _, leg := range r.TransitLegs {
		if !leg.DepartureAt.IsZero() {
			return leg.DepartureAt, true
		}
	}
	return time.Time{}, false
}

// TransitLeg は travelMode が TRANSIT のときの 1 区間。
type TransitLeg struct {
	LineName      string
	Headsign      string
	DepartureStop string
	ArrivalStop   string
	DepartureAt   time.Time
	ArrivalAt     time.Time
	NumStops      *int
	FareJPY       *int
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// ToGenres は正規化済みの分類語をドメインの列挙に写す。
// 未知の語もそのまま値として保つ（提供元の分類が増えたときに情報を落とさない）。
func ToGenres(list []string) []DiningGenre {
	out := make([]DiningGenre, 0, len(list))
	for _, v := range list {
		out = append(out, DiningGenre(v))
	}
	return out
}

// ToLodgingRequirements はアメニティの語をドメインの列挙に写す。
func ToLodgingRequirements(list []string) []LodgingRequirement {
	out := make([]LodgingRequirement, 0, len(list))
	for _, v := range list {
		out = append(out, LodgingRequirement(v))
	}
	return out
}
