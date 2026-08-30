package view

import (
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// このファイルは POST /v1/plans のリクエスト表現。
//
// **model に JSON タグを付けない規約の裏返し**として、リクエストの JSON 表現もこの層が持つ。
// 形は schemas/openapi.yaml の SearchRequest が正。
//
// 既定値を持つ項目のうち bool と数値はポインタで受ける。openapi の default は
// 「未指定なら既定値」であって「ゼロ値なら既定値」ではないため、
// `"llmNarrative": false` と項目の省略をゼロ値では区別できないのが理由。
// 数値・文字列の既定値は model.ApplyDefaults に集約し、ここでは埋めない。

// SearchRequest は検索条件のリクエストボディ。
type SearchRequest struct {
	Event      EventInput            `json:"event"`
	Party      *PartyInput           `json:"party"`
	Situation  SituationInput        `json:"situation"`
	Dining     *DiningPreference     `json:"dining"`
	Lodging    *LodgingPreference    `json:"lodging"`
	ReturnTrip *ReturnTripPreference `json:"returnTrip"`
	Options    *PlanOptions          `json:"options"`
	Context    *RequestContext       `json:"context"`
}

type EventInput struct {
	EventID *string    `json:"eventId"`
	Name    string     `json:"name"`
	Venue   VenueInput `json:"venue"`
	// EndsAt は **全計算の起点**。RFC3339 かつオフセット必須。
	EndsAt            string `json:"endsAt"`
	ExitBufferMinutes *int   `json:"exitBufferMinutes"`
}

type VenueInput struct {
	Name          string   `json:"name"`
	Address       string   `json:"address"`
	Location      Location `json:"location"`
	GooglePlaceID *string  `json:"googlePlaceId"`
}

type PartyInput struct {
	Adults       *int   `json:"adults"`
	Children     *int   `json:"children"`
	Relationship string `json:"relationship"`
}

// SituationInput の Mood / Notes は Go の絞り込みには一切使わず、LLM のプロンプトにのみ渡す。
type SituationInput struct {
	Intent        string              `json:"intent"`
	Mood          []string            `json:"mood"`
	Notes         string              `json:"notes"`
	PhysicalLimit *PhysicalLimitInput `json:"physicalLimit"`
}

type PhysicalLimitInput struct {
	MaxWalkMinutes *int `json:"maxWalkMinutes"`
}

type DiningPreference struct {
	Enabled            *bool          `json:"enabled"`
	Genres             []string       `json:"genres"`
	BudgetPerPersonJPY *MoneyRangeJPY `json:"budgetPerPersonJpy"`
	Requirements       []string       `json:"requirements"`
	DesiredStayMinutes *int           `json:"desiredStayMinutes"`
}

type LodgingPreference struct {
	Enabled           *bool          `json:"enabled"`
	CheckInDate       string         `json:"checkInDate"`
	CheckOutDate      string         `json:"checkOutDate"`
	Rooms             *int           `json:"rooms"`
	BudgetPerNightJPY *MoneyRangeJPY `json:"budgetPerNightJpy"`
	Requirements      []string       `json:"requirements"`
}

type ReturnTripPreference struct {
	Destination    *DestinationInput `json:"destination"`
	CheckLastTrain *bool             `json:"checkLastTrain"`
}

type DestinationInput struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type PlanOptions struct {
	TransportModes []string `json:"transportModes"`
	// MaxCandidatesPerCategory は **LLM 入力トークン量を直接決めるコスト制御ノブ**。
	MaxCandidatesPerCategory *int  `json:"maxCandidatesPerCategory"`
	LLMNarrative             *bool `json:"llmNarrative"`
}

type RequestContext struct {
	Locale   string `json:"locale"`
	Currency string `json:"currency"`
	Timezone string `json:"timezone"`
}

// MoneyRangeJPY は円（整数）の範囲。max 省略は上限なし。
type MoneyRangeJPY struct {
	Min *int `json:"min"`
	Max *int `json:"max"`
}

func (m *MoneyRangeJPY) toModel() model.MoneyRangeJPY {
	if m == nil {
		return model.MoneyRangeJPY{}
	}
	out := model.MoneyRangeJPY{Max: m.Max}
	if m.Min != nil {
		out.Min = *m.Min
	}
	return out
}

// 日付のみが意味を持つ項目（checkInDate / checkOutDate）の形式。
const dateLayout = "2006-01-02"

// 既知の制限: exitBufferMinutes / party.adults に **明示的な 0** を送った場合、
// model.ApplyDefaults がゼロ値を未指定とみなして既定値（30 分 / 2 人）に戻す。
// bool と違い数値の既定値はドメイン側に集約する方針を採ったための副作用で、
// adults は openapi の minimum が 1 なので実害はなく、exitBuffer 0 は 30 分に丸まる。

// ToModel は検索条件をドメインモデルへ移す。
//
// ここで見るのは **表現として解釈できるか**（時刻・日付の書式）だけで、
// 値の妥当性（範囲・列挙・項目間の整合）は model.SearchCondition.Validate の責務。
// 検証を二重に持つと、片方だけ直して食い違うのが目に見えているため分けている。
//
// 既定値は「未指定と明示的な false を区別できない bool」だけをここで埋め、
// 残りは呼び出し側が ApplyDefaults を通す。
func (r *SearchRequest) ToModel() (*model.SearchCondition, *apperror.Error) {
	var details []apperror.FieldError
	add := func(field, reason string) {
		details = append(details, apperror.FieldError{Field: field, Reason: reason})
	}

	cond := &model.SearchCondition{}

	// ── event ──
	cond.Event = model.Event{
		EventID: deref(r.Event.EventID),
		Name:    r.Event.Name,
		Venue: model.Venue{
			Name:          r.Event.Venue.Name,
			Address:       r.Event.Venue.Address,
			Location:      model.Location{Lat: r.Event.Venue.Location.Lat, Lng: r.Event.Venue.Location.Lng},
			GooglePlaceID: deref(r.Event.Venue.GooglePlaceID),
		},
	}
	if r.Event.EndsAt != "" {
		// オフセット無しを黙って UTC と解釈すると 9 時間ずれたプランを組んでしまう。
		// RFC3339 として読めない値はここで弾く。
		t, err := time.Parse(time.RFC3339, r.Event.EndsAt)
		if err != nil {
			add("event.endsAt", "RFC3339（オフセット付き）で指定してください。例: 2026-09-12T21:00:00+09:00")
		} else {
			cond.Event.EndsAt = t
		}
	}
	if r.Event.ExitBufferMinutes != nil {
		cond.Event.ExitBuffer = time.Duration(*r.Event.ExitBufferMinutes) * time.Minute
	}

	// ── party ──
	if r.Party != nil {
		cond.Party = model.Party{
			Adults:       derefInt(r.Party.Adults),
			Children:     derefInt(r.Party.Children),
			Relationship: model.Relationship(r.Party.Relationship),
		}
	}

	// ── situation ──
	cond.Situation = model.Situation{
		Intent: model.Intent(r.Situation.Intent),
		Notes:  r.Situation.Notes,
		Moods:  make([]model.Mood, 0, len(r.Situation.Mood)),
	}
	for _, m := range r.Situation.Mood {
		cond.Situation.Moods = append(cond.Situation.Moods, model.Mood(m))
	}
	if r.Situation.PhysicalLimit != nil && r.Situation.PhysicalLimit.MaxWalkMinutes != nil {
		cond.Situation.MaxWalk = time.Duration(*r.Situation.PhysicalLimit.MaxWalkMinutes) * time.Minute
	}

	// ── dining ──
	// 未指定なら有効。飲食も宿泊も既定で有効にしておき、要らないほうを false で落とさせる。
	cond.Dining.Enabled = true
	if r.Dining != nil {
		cond.Dining.Enabled = derefBool(r.Dining.Enabled, true)
		cond.Dining.BudgetPerPerson = r.Dining.BudgetPerPersonJPY.toModel()
		cond.Dining.Genres = make([]model.DiningGenre, 0, len(r.Dining.Genres))
		for _, g := range r.Dining.Genres {
			cond.Dining.Genres = append(cond.Dining.Genres, model.DiningGenre(g))
		}
		cond.Dining.Requirements = make([]model.DiningRequirement, 0, len(r.Dining.Requirements))
		for _, req := range r.Dining.Requirements {
			cond.Dining.Requirements = append(cond.Dining.Requirements, model.DiningRequirement(req))
		}
		if r.Dining.DesiredStayMinutes != nil {
			cond.Dining.DesiredStay = time.Duration(*r.Dining.DesiredStayMinutes) * time.Minute
		}
	}

	// ── lodging ──
	cond.Lodging.Enabled = true
	if r.Lodging != nil {
		cond.Lodging.Enabled = derefBool(r.Lodging.Enabled, true)
		cond.Lodging.Rooms = derefInt(r.Lodging.Rooms)
		cond.Lodging.BudgetPerNight = r.Lodging.BudgetPerNightJPY.toModel()
		cond.Lodging.Requirements = make([]model.LodgingRequirement, 0, len(r.Lodging.Requirements))
		for _, req := range r.Lodging.Requirements {
			cond.Lodging.Requirements = append(cond.Lodging.Requirements, model.LodgingRequirement(req))
		}
	}

	// ── returnTrip ──
	cond.ReturnTrip.CheckLastTrain = true
	if r.ReturnTrip != nil {
		cond.ReturnTrip.CheckLastTrain = derefBool(r.ReturnTrip.CheckLastTrain, true)
		if d := r.ReturnTrip.Destination; d != nil {
			cond.ReturnTrip.Destination = model.Destination{
				Type: model.DestinationType(d.Type), Name: d.Name,
			}
		}
	}

	// ── options ──
	cond.Options.LLMNarrative = true
	if r.Options != nil {
		cond.Options.LLMNarrative = derefBool(r.Options.LLMNarrative, true)
		cond.Options.MaxCandidatesPerCategory = derefInt(r.Options.MaxCandidatesPerCategory)
		cond.Options.TransportModes = make([]model.TravelMode, 0, len(r.Options.TransportModes))
		for _, m := range r.Options.TransportModes {
			cond.Options.TransportModes = append(cond.Options.TransportModes, model.TravelMode(m))
		}
	}

	// ── context ──
	if r.Context != nil {
		cond.Context = model.RequestContext{
			Locale: r.Context.Locale, Currency: r.Context.Currency, Timezone: r.Context.Timezone,
		}
	}

	// 日付は「日付だけが意味を持つ」ため、リクエストのタイムゾーンで解釈する。
	// UTC で読むと日本時間の 0〜9 時に前日として扱われる。
	loc := cond.Context.TimeLocation()
	if r.Lodging != nil {
		if t, ok := parseDate(r.Lodging.CheckInDate, loc); ok {
			cond.Lodging.CheckIn = t
		} else if r.Lodging.CheckInDate != "" {
			add("lodging.checkInDate", "YYYY-MM-DD 形式で指定してください")
		}
		if t, ok := parseDate(r.Lodging.CheckOutDate, loc); ok {
			cond.Lodging.CheckOut = t
		} else if r.Lodging.CheckOutDate != "" {
			add("lodging.checkOutDate", "YYYY-MM-DD 形式で指定してください")
		}
	}

	if len(details) > 0 {
		return nil, apperror.Validation(details...)
	}
	return cond, nil
}

func parseDate(s string, loc *time.Location) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(dateLayout, s, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func derefBool(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}
