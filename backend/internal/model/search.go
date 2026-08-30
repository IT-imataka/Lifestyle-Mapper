package model

import (
	"time"
	"unicode/utf8"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
)

// 既定値。openapi.yaml の default と一致させること。
const (
	DefaultExitBuffer               = 30 * time.Minute
	DefaultAdults                   = 2
	DefaultMaxWalk                  = 15 * time.Minute
	DefaultDesiredDiningStay        = 90 * time.Minute
	DefaultRooms                    = 1
	DefaultMaxCandidatesPerCategory = 8
	DefaultLocale                   = "ja-JP"
	DefaultCurrency                 = "JPY"
	DefaultTimezone                 = "Asia/Tokyo"
)

// SearchCondition はユーザーの検索条件。プラン生成パイプライン全体の入力。
type SearchCondition struct {
	Event      Event
	Party      Party
	Situation  Situation
	Dining     DiningPreference
	Lodging    LodgingPreference
	ReturnTrip ReturnTrip
	Options    Options
	Context    RequestContext
}

// Event は対象の公演。
type Event struct {
	EventID string // イベントマスタ参照時のみ。手入力なら空。
	Name    string
	Venue   Venue
	// EndsAt は **全計算の起点**。オフセット付きで受け取る。
	EndsAt time.Time
	// ExitBuffer は規制退場・物販待ちの余裕。
	ExitBuffer time.Duration
}

// DepartureAt は会場を離れられる最初の時刻。timecalc の起点になる。
func (e Event) DepartureAt() time.Time { return e.EndsAt.Add(e.ExitBuffer) }

type Venue struct {
	Name          string
	Address       string
	Location      Location
	GooglePlaceID string
}

func (v Venue) Waypoint() Waypoint {
	return Waypoint{Name: v.Name, Location: v.Location, GooglePlaceID: v.GooglePlaceID}
}

type Relationship string

const (
	RelationshipSolo    Relationship = "solo"
	RelationshipCouple  Relationship = "couple"
	RelationshipFriends Relationship = "friends"
	RelationshipFamily  Relationship = "family"
)

func (r Relationship) Valid() bool {
	switch r {
	case RelationshipSolo, RelationshipCouple, RelationshipFriends, RelationshipFamily:
		return true
	}
	return false
}

type Party struct {
	Adults       int
	Children     int
	Relationship Relationship
}

// Guests は費用計算に使う総人数。
func (p Party) Guests() int { return p.Adults + p.Children }

// Intent は宿泊するか帰るか。either なら Go が終電可否で判断する。
type Intent string

const (
	IntentStay   Intent = "stay"
	IntentReturn Intent = "return"
	IntentEither Intent = "either"
)

func (i Intent) Valid() bool {
	switch i {
	case IntentStay, IntentReturn, IntentEither:
		return true
	}
	return false
}

type Mood string

const (
	MoodCelebrate Mood = "celebrate"
	MoodRelaxed   Mood = "relaxed"
	MoodEfficient Mood = "efficient"
	MoodBudget    Mood = "budget"
)

func (m Mood) Valid() bool {
	switch m {
	case MoodCelebrate, MoodRelaxed, MoodEfficient, MoodBudget:
		return true
	}
	return false
}

// Situation は **このアプリの差別化点**。
// Moods と Notes は Go の絞り込みには一切使わず、LLM のプロンプトにのみ渡す。
// Notes はユーザーの自由記述なので、プロンプトインジェクション対策の対象。
type Situation struct {
	Intent  Intent
	Moods   []Mood
	Notes   string
	MaxWalk time.Duration
}

type DiningGenre string

const (
	GenreIzakaya          DiningGenre = "izakaya"
	GenreRamen            DiningGenre = "ramen"
	GenreYakiniku         DiningGenre = "yakiniku"
	GenreSushi            DiningGenre = "sushi"
	GenreCafe             DiningGenre = "cafe"
	GenreBar              DiningGenre = "bar"
	GenreFamilyRestaurant DiningGenre = "family_restaurant"
	GenreFastFood         DiningGenre = "fast_food"
	GenreItalian          DiningGenre = "italian"
	GenreChinese          DiningGenre = "chinese"
)

func (g DiningGenre) Valid() bool {
	switch g {
	case GenreIzakaya, GenreRamen, GenreYakiniku, GenreSushi, GenreCafe,
		GenreBar, GenreFamilyRestaurant, GenreFastFood, GenreItalian, GenreChinese:
		return true
	}
	return false
}

type DiningRequirement string

const (
	DiningOpenLate       DiningRequirement = "open_late"
	DiningReservable     DiningRequirement = "reservable"
	DiningNonSmoking     DiningRequirement = "non_smoking"
	DiningPrivateRoom    DiningRequirement = "private_room"
	DiningAllYouCanDrink DiningRequirement = "all_you_can_drink"
	DiningVegetarianOK   DiningRequirement = "vegetarian_ok"
)

func (r DiningRequirement) Valid() bool {
	switch r {
	case DiningOpenLate, DiningReservable, DiningNonSmoking,
		DiningPrivateRoom, DiningAllYouCanDrink, DiningVegetarianOK:
		return true
	}
	return false
}

type DiningPreference struct {
	Enabled         bool
	Genres          []DiningGenre
	BudgetPerPerson MoneyRangeJPY
	Requirements    []DiningRequirement
	DesiredStay     time.Duration
}

type LodgingRequirement string

const (
	LodgingNoSmoking         LodgingRequirement = "no_smoking"
	LodgingLateCheckinOK     LodgingRequirement = "late_checkin_ok"
	LodgingWithBath          LodgingRequirement = "with_bath"
	LodgingLargeBath         LodgingRequirement = "large_bath"
	LodgingBreakfastIncluded LodgingRequirement = "breakfast_included"
	LodgingCoinLaundry       LodgingRequirement = "coin_laundry"
	LodgingFreeWifi          LodgingRequirement = "free_wifi"
)

func (r LodgingRequirement) Valid() bool {
	switch r {
	case LodgingNoSmoking, LodgingLateCheckinOK, LodgingWithBath, LodgingLargeBath,
		LodgingBreakfastIncluded, LodgingCoinLaundry, LodgingFreeWifi:
		return true
	}
	return false
}

// LodgingPreference の CheckIn / CheckOut は日付のみが意味を持つ（時刻部は無視する）。
type LodgingPreference struct {
	Enabled        bool
	CheckIn        time.Time
	CheckOut       time.Time
	Rooms          int
	BudgetPerNight MoneyRangeJPY
	Requirements   []LodgingRequirement
}

// Nights は宿泊数。
func (l LodgingPreference) Nights() int {
	if l.CheckIn.IsZero() || l.CheckOut.IsZero() {
		return 0
	}
	return int(l.CheckOut.Sub(l.CheckIn).Hours() / 24)
}

type DestinationType string

const (
	DestinationStation DestinationType = "station"
	DestinationAddress DestinationType = "address"
	DestinationAirport DestinationType = "airport"
)

func (d DestinationType) Valid() bool {
	switch d {
	case DestinationStation, DestinationAddress, DestinationAirport:
		return true
	}
	return false
}

type Destination struct {
	Type DestinationType
	Name string
}

// ReturnTrip は situation.intent が return / either のときのみ有効。
type ReturnTrip struct {
	Destination    Destination
	CheckLastTrain bool
}

type Options struct {
	TransportModes []TravelMode
	// MaxCandidatesPerCategory は **LLM 入力トークン量を直接決めるコスト制御ノブ**。
	MaxCandidatesPerCategory int
	// LLMNarrative が false なら LLM をスキップし、ルールベースで時系列のみ組む。
	LLMNarrative bool
}

func (o Options) AllowsMode(m TravelMode) bool {
	for _, v := range o.TransportModes {
		if v == m {
			return true
		}
	}
	return false
}

type RequestContext struct {
	Locale   string
	Currency string
	Timezone string
}

// TimeLocation はタイムゾーン名を解決する。未設定・解決不能なら Asia/Tokyo に倒す。
func (c RequestContext) TimeLocation() *time.Location {
	name := c.Timezone
	if name == "" {
		name = DefaultTimezone
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		loc, err = time.LoadLocation(DefaultTimezone)
		if err != nil {
			return time.UTC
		}
	}
	return loc
}

// ApplyDefaults はゼロ値の項目に既定値を埋める。
//
// bool（Dining.Enabled / Lodging.Enabled / Options.LLMNarrative / ReturnTrip.CheckLastTrain）は
// 「未指定」と「明示的な false」をゼロ値で区別できないため、ここでは触らない。
// これらの既定値は JSON デコード側（controller）でポインタ受けして適用する。
func (s *SearchCondition) ApplyDefaults() {
	if s.Event.ExitBuffer == 0 {
		s.Event.ExitBuffer = DefaultExitBuffer
	}
	if s.Party.Adults == 0 {
		s.Party.Adults = DefaultAdults
	}
	if s.Party.Relationship == "" {
		s.Party.Relationship = RelationshipFriends
	}
	if s.Situation.MaxWalk == 0 {
		s.Situation.MaxWalk = DefaultMaxWalk
	}
	if s.Dining.DesiredStay == 0 {
		s.Dining.DesiredStay = DefaultDesiredDiningStay
	}
	if s.Lodging.Rooms == 0 {
		s.Lodging.Rooms = DefaultRooms
	}
	if len(s.Options.TransportModes) == 0 {
		s.Options.TransportModes = []TravelMode{TravelWalk, TravelTransit}
	}
	if s.Options.MaxCandidatesPerCategory == 0 {
		s.Options.MaxCandidatesPerCategory = DefaultMaxCandidatesPerCategory
	}
	if s.Context.Locale == "" {
		s.Context.Locale = DefaultLocale
	}
	if s.Context.Currency == "" {
		s.Context.Currency = DefaultCurrency
	}
	if s.Context.Timezone == "" {
		s.Context.Timezone = DefaultTimezone
	}
}

// Validate は既定値適用後の検索条件を検査し、違反があれば VALIDATION_FAILED を返す。
// field 名は openapi.yaml のリクエスト JSON のパスに合わせる（フロントがそのまま項目に対応付けられる）。
func (s *SearchCondition) Validate(now time.Time) *apperror.Error {
	var d []apperror.FieldError
	add := func(field, reason string) { d = append(d, apperror.FieldError{Field: field, Reason: reason}) }

	// ── event ──
	if utf8.RuneCountInString(s.Event.Name) > 120 {
		add("event.name", "120 文字以内で入力してください")
	}
	if s.Event.Venue.Name == "" {
		add("event.venue.name", "会場名は必須です")
	}
	if !s.Event.Venue.Location.Valid() {
		add("event.venue.location", "会場の緯度経度が不正です")
	}
	switch {
	case s.Event.EndsAt.IsZero():
		add("event.endsAt", "終演時刻は必須です")
	case s.Event.EndsAt.Before(now):
		add("event.endsAt", "過去の日時は指定できません")
	}
	if s.Event.ExitBuffer < 0 || s.Event.ExitBuffer > 120*time.Minute {
		add("event.exitBufferMinutes", "0〜120 分で指定してください")
	}

	// ── party ──
	if s.Party.Adults < 1 || s.Party.Adults > 20 {
		add("party.adults", "1〜20 人で指定してください")
	}
	if s.Party.Children < 0 || s.Party.Children > 20 {
		add("party.children", "0〜20 人で指定してください")
	}
	if !s.Party.Relationship.Valid() {
		add("party.relationship", "未対応の同行者区分です")
	}

	// ── situation ──
	if !s.Situation.Intent.Valid() {
		add("situation.intent", "stay / return / either のいずれかを指定してください")
	}
	if len(s.Situation.Moods) > 4 {
		add("situation.mood", "4 つ以内で指定してください")
	}
	for _, m := range s.Situation.Moods {
		if !m.Valid() {
			add("situation.mood", "未対応の値が含まれています: "+string(m))
			break
		}
	}
	if utf8.RuneCountInString(s.Situation.Notes) > 200 {
		add("situation.notes", "200 文字以内で入力してください")
	}
	if s.Situation.MaxWalk < time.Minute || s.Situation.MaxWalk > 60*time.Minute {
		add("situation.physicalLimit.maxWalkMinutes", "1〜60 分で指定してください")
	}

	// ── dining ──
	if s.Dining.Enabled {
		if len(s.Dining.Genres) > 5 {
			add("dining.genres", "5 つ以内で指定してください")
		}
		for _, g := range s.Dining.Genres {
			if !g.Valid() {
				add("dining.genres", "未対応のジャンルが含まれています: "+string(g))
				break
			}
		}
		for _, r := range s.Dining.Requirements {
			if !r.Valid() {
				add("dining.requirements", "未対応の条件が含まれています: "+string(r))
				break
			}
		}
		if !s.Dining.BudgetPerPerson.Valid() {
			add("dining.budgetPerPersonJpy", "予算の下限が上限を超えています")
		}
		if s.Dining.DesiredStay < 15*time.Minute || s.Dining.DesiredStay > 240*time.Minute {
			add("dining.desiredStayMinutes", "15〜240 分で指定してください")
		}
	}

	// ── lodging ──
	if s.Lodging.Enabled {
		if s.Lodging.Rooms < 1 || s.Lodging.Rooms > 10 {
			add("lodging.rooms", "1〜10 室で指定してください")
		}
		if !s.Lodging.BudgetPerNight.Valid() {
			add("lodging.budgetPerNightJpy", "予算の下限が上限を超えています")
		}
		if !s.Lodging.CheckIn.IsZero() && !s.Lodging.CheckOut.IsZero() &&
			!s.Lodging.CheckOut.After(s.Lodging.CheckIn) {
			add("lodging.checkOutDate", "チェックイン日より後の日付を指定してください")
		}
		for _, r := range s.Lodging.Requirements {
			if !r.Valid() {
				add("lodging.requirements", "未対応の条件が含まれています: "+string(r))
				break
			}
		}
	}

	// ── returnTrip ──
	if s.Situation.Intent != IntentStay && s.ReturnTrip.Destination.Name != "" {
		if !s.ReturnTrip.Destination.Type.Valid() {
			add("returnTrip.destination.type", "station / address / airport のいずれかを指定してください")
		}
		if utf8.RuneCountInString(s.ReturnTrip.Destination.Name) > 120 {
			add("returnTrip.destination.name", "120 文字以内で入力してください")
		}
	}

	// ── options ──
	if len(s.Options.TransportModes) == 0 {
		add("options.transportModes", "1 つ以上指定してください")
	}
	for _, m := range s.Options.TransportModes {
		if !m.Valid() {
			add("options.transportModes", "未対応の移動手段が含まれています: "+string(m))
			break
		}
	}
	if s.Options.MaxCandidatesPerCategory < 3 || s.Options.MaxCandidatesPerCategory > 20 {
		add("options.maxCandidatesPerCategory", "3〜20 件で指定してください")
	}

	// ── context ──
	if s.Context.Currency != DefaultCurrency {
		add("context.currency", "現在は JPY のみ対応しています")
	}
	if _, err := time.LoadLocation(s.Context.Timezone); err != nil {
		add("context.timezone", "未知のタイムゾーンです")
	}

	// 宿泊も飲食も無効なら、組むものが何も無い。
	if !s.Dining.Enabled && !s.Lodging.Enabled {
		add("dining.enabled", "飲食か宿泊のどちらかは有効にしてください")
	}

	if len(d) == 0 {
		return nil
	}
	return apperror.Validation(d...)
}
