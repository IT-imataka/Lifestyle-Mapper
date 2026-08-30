package view

import (
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// Plan は GET /v1/plans/{planId} の data。SSE の plan イベントも同じ形を流す。
//
// **narrative（LLM 由来）と事実フィールドを全階層で物理的に分離**している。
// レビューでは「narrative の外に LLM の文字列が入っていないか」だけを見れば済み、
// UI は LLM を止めたい日に narrative を無視するだけで機能を落とさず動く。
type Plan struct {
	PlanID       string         `json:"planId"`
	Status       string         `json:"status"`
	GeneratedAt  string         `json:"generatedAt"`
	ExpiresAt    string         `json:"expiresAt,omitempty"`
	ShareURL     string         `json:"shareUrl,omitempty"`
	Narrative    *PlanNarrative `json:"narrative"`
	Summary      PlanSummary    `json:"summary"`
	Timeline     []Segment      `json:"timeline"`
	Alternatives Alternatives   `json:"alternatives"`
	Warnings     []Warning      `json:"warnings"`
}

// NewPlan は確定したドメインのプランを応答表現に移す。
func NewPlan(p *model.Plan) Plan {
	if p == nil {
		return Plan{}
	}
	out := Plan{
		PlanID:       p.ID.String(),
		Status:       string(p.Status),
		GeneratedAt:  rfc3339(p.GeneratedAt),
		ExpiresAt:    rfc3339(p.ExpiresAt),
		ShareURL:     p.ShareURL,
		Summary:      newPlanSummary(p.Summary),
		Timeline:     make([]Segment, 0, len(p.Timeline)),
		Alternatives: newAlternatives(p.Alternatives),
		Warnings:     make([]Warning, 0, len(p.Warnings)),
	}
	if p.Narrative != nil {
		out.Narrative = &PlanNarrative{
			Title:       p.Narrative.Title,
			Summary:     p.Narrative.Summary,
			ClosingNote: p.Narrative.ClosingNote,
			Vibe:        p.Narrative.Vibe,
		}
	}
	for _, s := range p.Timeline {
		out.Timeline = append(out.Timeline, newSegment(s))
	}
	for _, w := range p.Warnings {
		out.Warnings = append(out.Warnings, Warning{
			Code: string(w.Code), Severity: string(w.Severity),
			SegmentID: w.SegmentID, Message: w.Message,
		})
	}
	return out
}

func NewPlanEnvelope(p *model.Plan, meta Meta) Envelope[Plan] {
	return Envelope[Plan]{Data: NewPlan(p), Meta: meta}
}

// PlanNarrative は **LLM が生成した文章のみ**。事実は一切含まない。
type PlanNarrative struct {
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	ClosingNote string `json:"closingNote,omitempty"`
	Vibe        string `json:"vibe,omitempty"`
}

// PlanSummary は **Go が確定した事実のみ**。LLM は書き込めない。
type PlanSummary struct {
	StartAt          string `json:"startAt"`
	EndAt            string `json:"endAt"`
	TotalWalkMinutes int    `json:"totalWalkMinutes"`
	TotalWalkMeters  int    `json:"totalWalkMeters"`
	EstimatedCostJPY Cost   `json:"estimatedCostJpy"`
	Feasibility      string `json:"feasibility"`
}

// Cost の total は常に導出値。内訳と食い違わせない。
type Cost struct {
	Dining  int `json:"dining"`
	Lodging int `json:"lodging"`
	Transit int `json:"transit"`
	Total   int `json:"total"`
}

func newPlanSummary(s model.PlanSummary) PlanSummary {
	return PlanSummary{
		StartAt:          rfc3339(s.StartAt),
		EndAt:            rfc3339(s.EndAt),
		TotalWalkMinutes: minutes(s.TotalWalk),
		TotalWalkMeters:  s.TotalWalkMeters,
		EstimatedCostJPY: Cost{
			Dining:  s.Cost.DiningJPY,
			Lodging: s.Cost.LodgingJPY,
			Transit: s.Cost.TransitJPY,
			Total:   s.Cost.TotalJPY(),
		},
		Feasibility: string(s.Feasibility),
	}
}

// Segment は type による判別可能ユニオン。詳細は 1 つだけが非 nil になる。
type Segment struct {
	ID              string            `json:"id"`
	Type            string            `json:"type"`
	StartAt         string            `json:"startAt"`
	EndAt           string            `json:"endAt"`
	DurationMinutes int               `json:"durationMinutes"`
	Narrative       *SegmentNarrative `json:"narrative"`

	Buffer  *BufferDetail  `json:"buffer,omitempty"`
	Move    *MoveDetail    `json:"move,omitempty"`
	Dining  *DiningDetail  `json:"dining,omitempty"`
	Lodging *LodgingDetail `json:"lodging,omitempty"`
	// Links は openapi の DiningSegment / LodgingSegment で必須、buffer / move では持たない。
	// 「リンクが 0 件」と「その種別にリンクという概念が無い」をポインタで区別する
	// （空スライス + omitempty では前者も項目ごと消えてしまう）。
	Links *[]Link `json:"links,omitempty"`
}

type SegmentNarrative struct {
	Headline string `json:"headline"`
	Reason   string `json:"reason,omitempty"`
	Tip      string `json:"tip,omitempty"`
}

func newSegment(s model.Segment) Segment {
	out := Segment{
		ID:              s.ID,
		Type:            string(s.Type),
		StartAt:         rfc3339(s.StartAt),
		EndAt:           rfc3339(s.EndAt),
		DurationMinutes: s.DurationMinutes(),
	}
	if s.Narrative != nil {
		out.Narrative = &SegmentNarrative{
			Headline: s.Narrative.Headline, Reason: s.Narrative.Reason, Tip: s.Narrative.Tip,
		}
	}
	switch {
	case s.Buffer != nil:
		out.Buffer = &BufferDetail{Kind: string(s.Buffer.Kind), AtPlaceName: s.Buffer.AtPlaceName}
	case s.Move != nil:
		v := newMoveDetail(s.Move)
		out.Move = &v
	case s.Dining != nil:
		// 営業判定の基準は **到着予定時刻**。現在時刻ではない。
		v := newDiningDetail(s.Dining, s.StartAt)
		out.Dining = &v
		out.Links = linksOrEmpty(s.Links)
	case s.Lodging != nil:
		v := newLodgingDetail(*s.Lodging)
		out.Lodging = &v
		out.Links = linksOrEmpty(s.Links)
	}
	return out
}

type BufferDetail struct {
	Kind        string `json:"kind"`
	AtPlaceName string `json:"atPlaceName,omitempty"`
}

// MoveDetail は **Google Routes API 由来（LLM 不可侵）**。
type MoveDetail struct {
	TravelMode      string       `json:"travelMode"`
	DistanceMeters  int          `json:"distanceMeters"`
	DurationMinutes int          `json:"durationMinutes"`
	From            Waypoint     `json:"from"`
	To              Waypoint     `json:"to"`
	EncodedPolyline string       `json:"encodedPolyline,omitempty"`
	MapsURL         string       `json:"mapsUrl,omitempty"`
	TransitLegs     []TransitLeg `json:"transitLegs"`
}

type Waypoint struct {
	Name     string   `json:"name"`
	Location Location `json:"location"`
}

type Location struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type TransitLeg struct {
	LineName      string `json:"lineName"`
	Headsign      string `json:"headsign,omitempty"`
	DepartureStop string `json:"departureStop"`
	ArrivalStop   string `json:"arrivalStop"`
	DepartureAt   string `json:"departureAt,omitempty"`
	ArrivalAt     string `json:"arrivalAt,omitempty"`
	NumStops      *int   `json:"numStops,omitempty"`
	FareJPY       *int   `json:"fareJpy,omitempty"`
}

func newMoveDetail(r *model.RouteFact) MoveDetail {
	out := MoveDetail{
		TravelMode:      string(r.Key.Mode),
		DistanceMeters:  r.DistanceMeters,
		DurationMinutes: minutes(r.Duration),
		From:            newWaypoint(r.From),
		To:              newWaypoint(r.To),
		EncodedPolyline: r.EncodedPolyline,
		MapsURL:         r.MapsURL,
		TransitLegs:     make([]TransitLeg, 0, len(r.TransitLegs)),
	}
	for _, leg := range r.TransitLegs {
		out.TransitLegs = append(out.TransitLegs, TransitLeg{
			LineName:      leg.LineName,
			Headsign:      leg.Headsign,
			DepartureStop: leg.DepartureStop,
			ArrivalStop:   leg.ArrivalStop,
			DepartureAt:   rfc3339(leg.DepartureAt),
			ArrivalAt:     rfc3339(leg.ArrivalAt),
			NumStops:      leg.NumStops,
			FareJPY:       leg.FareJPY,
		})
	}
	return out
}

func newWaypoint(w model.Waypoint) Waypoint {
	return Waypoint{Name: w.Name, Location: newLocation(w.Location)}
}

func newLocation(l model.Location) Location { return Location{Lat: l.Lat, Lng: l.Lng} }

// DiningDetail は **Google Places API 由来（LLM 不可侵）**。
type DiningDetail struct {
	CandidateID               string         `json:"candidateId"`
	ProviderPlaceID           string         `json:"providerPlaceId"`
	Name                      string         `json:"name"`
	Genres                    []string       `json:"genres"`
	Rating                    *float64       `json:"rating,omitempty"`
	UserRatingCount           *int           `json:"userRatingCount,omitempty"`
	PriceLevel                *int           `json:"priceLevel,omitempty"`
	EstimatedCostPerPersonJPY *int           `json:"estimatedCostPerPersonJpy,omitempty"`
	Address                   string         `json:"address,omitempty"`
	Location                  Location       `json:"location"`
	PhotoURL                  string         `json:"photoUrl,omitempty"`
	PhoneNumber               string         `json:"phoneNumber,omitempty"`
	OpeningStatus             *OpeningStatus `json:"openingStatus,omitempty"`
}

// OpeningStatus は到着予定時刻における営業判定。
type OpeningStatus struct {
	OpenNow  bool   `json:"openNow"`
	OpensAt  string `json:"opensAt,omitempty"`
	ClosesAt string `json:"closesAt,omitempty"`
}

// newDiningDetail は arrival に到着予定時刻を取る。
// ゼロ値なら営業判定を付けない（差し替え候補は到着時刻が決まっていないため）。
func newDiningDetail(p *model.PlaceFact, arrival time.Time) DiningDetail {
	out := DiningDetail{
		CandidateID:               string(p.ID),
		ProviderPlaceID:           p.ProviderPlaceID,
		Name:                      p.Name,
		Genres:                    append([]string{}, p.Genres...),
		Rating:                    p.Rating,
		UserRatingCount:           p.UserRatingCount,
		PriceLevel:                p.PriceLevel,
		EstimatedCostPerPersonJPY: p.EstimatedCostPerPersonJPY,
		Address:                   p.Address,
		Location:                  newLocation(p.Loc),
		PhoneNumber:               p.PhoneNumber,
	}
	// 写真は自前 CDN 経由でしか出さない（API キー露出とキャッシュのため）。
	// 参照名から URL を組む役目は infrastructure 側にあり、view は組み立てない。
	if !arrival.IsZero() && p.Hours.Known {
		status := OpeningStatus{OpenNow: p.Hours.IsOpenAt(arrival)}
		if closesAt, ok := p.Hours.ClosesAfter(arrival); ok {
			status.ClosesAt = rfc3339(closesAt)
		}
		out.OpeningStatus = &status
	}
	return out
}

// LodgingDetail は **楽天トラベル API 由来（LLM 不可侵）**。
type LodgingDetail struct {
	CandidateID     string            `json:"candidateId"`
	ProviderHotelID string            `json:"providerHotelId"`
	Name            string            `json:"name"`
	ReviewAverage   *float64          `json:"reviewAverage,omitempty"`
	ReviewCount     *int              `json:"reviewCount,omitempty"`
	Address         string            `json:"address,omitempty"`
	Location        Location          `json:"location"`
	PhotoURL        string            `json:"photoUrl,omitempty"`
	Plan            LodgingPlanDetail `json:"plan"`
}

type LodgingPlanDetail struct {
	ProviderPlanID    string   `json:"providerPlanId"`
	PlanName          string   `json:"planName"`
	RoomName          string   `json:"roomName,omitempty"`
	TotalPriceJPY     int      `json:"totalPriceJpy"`
	PricePerPersonJPY *int     `json:"pricePerPersonJpy,omitempty"`
	VacancyStatus     string   `json:"vacancyStatus"`
	RemainingRooms    *int     `json:"remainingRooms,omitempty"`
	CheckInTime       string   `json:"checkInTime,omitempty"`
	CheckInDeadline   string   `json:"checkInDeadline,omitempty"`
	CheckOutTime      string   `json:"checkOutTime,omitempty"`
	Amenities         []string `json:"amenities"`
}

func newLodgingDetail(c model.LodgingChoice) LodgingDetail {
	h := c.Hotel
	out := LodgingDetail{
		CandidateID:     string(h.ID),
		ProviderHotelID: h.ProviderHotelID,
		Name:            h.Name,
		ReviewAverage:   h.ReviewAverage,
		ReviewCount:     h.ReviewCount,
		Address:         h.Address,
		Location:        newLocation(h.Loc),
		PhotoURL:        h.PhotoURL,
	}
	if c.Plan != nil {
		out.Plan = LodgingPlanDetail{
			ProviderPlanID:    c.Plan.ProviderPlanID,
			PlanName:          c.Plan.PlanName,
			RoomName:          c.Plan.RoomName,
			TotalPriceJPY:     c.Plan.TotalPriceJPY,
			PricePerPersonJPY: c.Plan.PricePerPersonJPY,
			VacancyStatus:     string(c.Plan.VacancyStatus),
			RemainingRooms:    c.Plan.RemainingRooms,
			CheckInTime:       clock(c.Plan.CheckInTime),
			// 26:00 のような 24 時超え表記は提供元仕様のまま返す。
			CheckInDeadline: clock(c.Plan.CheckInDeadline),
			CheckOutTime:    clock(c.Plan.CheckOutTime),
			Amenities:       append([]string{}, c.Plan.Amenities...),
		}
	}
	return out
}

func clock(c *model.ClockTime) string {
	if c == nil {
		return ""
	}
	return c.String()
}

// Link は**収益がかかっている唯一の出口**。素の URL 文字列を置かない。
type Link struct {
	Kind       string `json:"kind"`
	Provider   string `json:"provider"`
	Label      string `json:"label"`
	URL        string `json:"url"`
	TrackingID string `json:"trackingId,omitempty"`
}

// linksOrEmpty は必ず配列を返す。予約リンクが 1 件も無くても null にしない。
func linksOrEmpty(links []model.Link) *[]Link {
	out := newLinks(links)
	if out == nil {
		out = []Link{}
	}
	return &out
}

func newLinks(links []model.Link) []Link {
	if len(links) == 0 {
		return nil
	}
	out := make([]Link, 0, len(links))
	for _, l := range links {
		out = append(out, Link{
			Kind: string(l.Kind), Provider: string(l.Provider),
			Label: l.Label, URL: l.URL, TrackingID: l.TrackingID,
		})
	}
	return out
}

// Alternatives は UI の AlternativePicker が使う差し替え候補。
type Alternatives struct {
	Dining  []DiningAlternative  `json:"dining"`
	Lodging []LodgingAlternative `json:"lodging"`
}

type DiningAlternative struct {
	DiningDetail
	ExcludedReason string `json:"excludedReason,omitempty"`
	Links          []Link `json:"links,omitempty"`
}

type LodgingAlternative struct {
	LodgingDetail
	ExcludedReason string `json:"excludedReason,omitempty"`
	Links          []Link `json:"links,omitempty"`
}

func newAlternatives(a model.Alternatives) Alternatives {
	out := Alternatives{
		Dining:  make([]DiningAlternative, 0, len(a.Dining)),
		Lodging: make([]LodgingAlternative, 0, len(a.Lodging)),
	}
	for _, d := range a.Dining {
		out.Dining = append(out.Dining, DiningAlternative{
			// 到着時刻が決まらないので営業判定は付けない（false を「閉店」と誤読させない）。
			DiningDetail:   newDiningDetail(d.Place, time.Time{}),
			ExcludedReason: d.ExcludedReason,
			Links:          newLinks(d.Links),
		})
	}
	for _, l := range a.Lodging {
		out.Lodging = append(out.Lodging, LodgingAlternative{
			LodgingDetail:  newLodgingDetail(l.Choice),
			ExcludedReason: l.ExcludedReason,
			Links:          newLinks(l.Links),
		})
	}
	return out
}

type Warning struct {
	Code      string `json:"code"`
	Severity  string `json:"severity"`
	SegmentID string `json:"segmentId,omitempty"`
	Message   string `json:"message"`
}
