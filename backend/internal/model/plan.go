package model

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ── プラン ID / 状態 ──────────────────────────────────

// PlanID は ULID ベースのプラン ID。
type PlanID string

var planIDPattern = regexp.MustCompile(`^pln_[0-9A-HJKMNP-TV-Z]{26}$`)

func NewPlanID() PlanID { return PlanID("pln_" + newULID(time.Now())) }

func (id PlanID) Valid() bool { return planIDPattern.MatchString(string(id)) }

func (id PlanID) String() string { return string(id) }

// PlanStatus は生成の進捗。SSE でも同じ値が流れる。
type PlanStatus string

const (
	StatusQueued     PlanStatus = "queued"
	StatusCollecting PlanStatus = "collecting"
	StatusComposing  PlanStatus = "composing"
	StatusCompleted  PlanStatus = "completed"
	// StatusPartial は一部プロバイダが落ちたが組めた状態。全体 500 より UX が良い。
	StatusPartial PlanStatus = "partial"
	StatusFailed  PlanStatus = "failed"
)

// Terminal はこれ以上状態が進まないことを示す。SSE のクローズ判定に使う。
func (s PlanStatus) Terminal() bool {
	switch s {
	case StatusCompleted, StatusPartial, StatusFailed:
		return true
	}
	return false
}

// PlanTTL は空室情報の鮮度保証。これを過ぎたプランは 410 で再検索を促す。
const PlanTTL = 15 * time.Minute

// ── プラン ────────────────────────────────────────────

// Plan は生成されたプラン 1 件。
type Plan struct {
	ID          PlanID
	Status      PlanStatus
	GeneratedAt time.Time
	ExpiresAt   time.Time
	ShareURL    string
	// Narrative は LLM 由来。llmNarrative=false のときと LLM 全滅時は nil。
	// nil でも Timeline だけで機能が成立することが、この分離の目的。
	Narrative    *PlanNarrative
	Summary      PlanSummary
	Timeline     Timeline
	Alternatives Alternatives
	Warnings     []Warning
}

func (p *Plan) Expired(now time.Time) bool {
	return !p.ExpiresAt.IsZero() && now.After(p.ExpiresAt)
}

func (p *Plan) AddWarning(w Warning) { p.Warnings = append(p.Warnings, w) }

// Segment は ID でセグメントを引く。
func (p *Plan) Segment(id string) (*Segment, bool) {
	for i := range p.Timeline {
		if p.Timeline[i].ID == id {
			return &p.Timeline[i], true
		}
	}
	return nil, false
}

// PlanNarrative は **LLM が生成した文章のみ**。事実は一切含まない。
type PlanNarrative struct {
	Title       string
	Summary     string
	ClosingNote string
	Vibe        string
}

// PlanSummary は **Go が確定した事実のみ**。LLM は書き込めない。
type PlanSummary struct {
	StartAt         time.Time
	EndAt           time.Time
	TotalWalk       time.Duration
	TotalWalkMeters int
	Cost            CostBreakdown
	Feasibility     Feasibility
}

// Feasibility は実行可能性。tight は乗り換え・入店の余裕が 5 分未満の箇所があることを示す。
type Feasibility string

const (
	FeasibilityOK         Feasibility = "ok"
	FeasibilityTight      Feasibility = "tight"
	FeasibilityInfeasible Feasibility = "infeasible"
)

// TightSlack は tight と判定する余裕時間の閾値。
const TightSlack = 5 * time.Minute

// CostBreakdown は円（整数）の内訳。Total は常に導出値にして食い違いを防ぐ。
type CostBreakdown struct {
	DiningJPY  int
	LodgingJPY int
	TransitJPY int
}

func (c CostBreakdown) TotalJPY() int { return c.DiningJPY + c.LodgingJPY + c.TransitJPY }

// ── タイムライン ──────────────────────────────────────

// SegmentType はセグメント種別。フロントの判別可能ユニオンの判別子と一致する。
type SegmentType string

const (
	SegmentBuffer  SegmentType = "buffer"
	SegmentMove    SegmentType = "move"
	SegmentDining  SegmentType = "dining"
	SegmentLodging SegmentType = "lodging"
)

// SegmentID は seg_1 形式の ID を組み立てる。order は 1 起点。
func SegmentID(order int) string { return fmt.Sprintf("seg_%d", order) }

// Segment はタイムラインの 1 コマ。
//
// 詳細フィールドは Type に対応する 1 つだけが非 nil になる。事実側は FactStore の
// ポインタをそのまま指すため、値の写しは持たない（真実の源を二重化しない）。
type Segment struct {
	ID      string
	Type    SegmentType
	StartAt time.Time
	EndAt   time.Time
	// Narrative は LLM 由来（nil 可）。ここ以外に LLM の文字列は入らない。
	Narrative *SegmentNarrative

	Buffer  *BufferDetail
	Move    *RouteFact
	Dining  *PlaceFact
	Lodging *LodgingChoice

	Links []Link
}

// Duration は開始・終了から導出する。分の値を別に持つと必ず食い違うため保持しない。
func (s Segment) Duration() time.Duration { return s.EndAt.Sub(s.StartAt) }

// DurationMinutes は view が使う分表現。
func (s Segment) DurationMinutes() int { return int(s.Duration().Minutes()) }

// CandidateID はこのセグメントが参照する候補 ID を返す。buffer / move では空。
func (s Segment) CandidateID() CandidateID {
	switch {
	case s.Dining != nil:
		return s.Dining.ID
	case s.Lodging != nil && s.Lodging.Hotel != nil:
		return s.Lodging.Hotel.ID
	}
	return ""
}

// Validate は Type と詳細フィールドの対応を検査する。
// Hydrator が組み立てた直後に必ず通し、型と実体のずれをここで止める。
func (s Segment) Validate() error {
	if s.ID == "" {
		return errors.New("セグメント ID が空です")
	}
	if s.EndAt.Before(s.StartAt) {
		return fmt.Errorf("%s: 終了時刻が開始時刻より前です", s.ID)
	}
	filled := 0
	for _, present := range []bool{s.Buffer != nil, s.Move != nil, s.Dining != nil, s.Lodging != nil} {
		if present {
			filled++
		}
	}
	if filled != 1 {
		return fmt.Errorf("%s: 詳細は種別に対応する 1 つだけを埋めてください（%d 個埋まっています）", s.ID, filled)
	}
	switch s.Type {
	case SegmentBuffer:
		if s.Buffer == nil {
			return fmt.Errorf("%s: type=buffer に buffer がありません", s.ID)
		}
	case SegmentMove:
		if s.Move == nil {
			return fmt.Errorf("%s: type=move に move がありません", s.ID)
		}
	case SegmentDining:
		if s.Dining == nil {
			return fmt.Errorf("%s: type=dining に dining がありません", s.ID)
		}
	case SegmentLodging:
		if s.Lodging == nil || s.Lodging.Hotel == nil || s.Lodging.Plan == nil {
			return fmt.Errorf("%s: type=lodging にホテルと宿泊プランが揃っていません", s.ID)
		}
	default:
		return fmt.Errorf("%s: 未知のセグメント種別です: %q", s.ID, s.Type)
	}
	for _, l := range s.Links {
		if err := l.Validate(); err != nil {
			return fmt.Errorf("%s: %w", s.ID, err)
		}
	}
	return nil
}

// SegmentNarrative は **LLM 由来**。UI から消しても機能が落ちない層。
type SegmentNarrative struct {
	Headline string
	Reason   string
	Tip      string
}

// BufferKind は待機の理由。
type BufferKind string

const (
	BufferExitCongestion BufferKind = "exit_congestion"
	BufferGoodsQueue     BufferKind = "goods_queue"
	BufferSpareTime      BufferKind = "spare_time"
)

type BufferDetail struct {
	Kind        BufferKind
	AtPlaceName string
}

// LodgingChoice は「どのホテルの、どのプランを採ったか」。
// Plan は Hotel.Plans の要素を指す。
type LodgingChoice struct {
	Hotel *HotelFact
	Plan  *HotelPlanFact
}

// ── セグメント生成 ────────────────────────────────────

func NewBufferSegment(order int, startAt time.Time, d time.Duration, kind BufferKind, atPlace string) Segment {
	return Segment{
		ID:      SegmentID(order),
		Type:    SegmentBuffer,
		StartAt: startAt,
		EndAt:   startAt.Add(d),
		Buffer:  &BufferDetail{Kind: kind, AtPlaceName: atPlace},
	}
}

// NewMoveSegment は移動時間に RouteFact の実測値をそのまま使う。
// LLM の提案した滞在時間は移動には一切反映しない。
func NewMoveSegment(order int, startAt time.Time, route *RouteFact) Segment {
	return Segment{
		ID:      SegmentID(order),
		Type:    SegmentMove,
		StartAt: startAt,
		EndAt:   startAt.Add(route.Duration),
		Move:    route,
	}
}

func NewDiningSegment(order int, startAt time.Time, stay time.Duration, place *PlaceFact, links []Link) Segment {
	return Segment{
		ID:      SegmentID(order),
		Type:    SegmentDining,
		StartAt: startAt,
		EndAt:   startAt.Add(stay),
		Dining:  place,
		Links:   links,
	}
}

// NewLodgingSegment はチェックインから翌朝のチェックアウトまでを 1 コマにする。
func NewLodgingSegment(order int, checkInAt, checkOutAt time.Time, choice LodgingChoice, links []Link) Segment {
	return Segment{
		ID:      SegmentID(order),
		Type:    SegmentLodging,
		StartAt: checkInAt,
		EndAt:   checkOutAt,
		Lodging: &choice,
		Links:   links,
	}
}

// ── Timeline ──────────────────────────────────────────

// Timeline は時系列順のセグメント列。
type Timeline []Segment

func (t Timeline) StartAt() time.Time {
	if len(t) == 0 {
		return time.Time{}
	}
	return t[0].StartAt
}

func (t Timeline) EndAt() time.Time {
	if len(t) == 0 {
		return time.Time{}
	}
	return t[len(t)-1].EndAt
}

// Validate は時系列としての整合性を検査する。
// 返すのは平の error で、リトライするか諦めるかの判断は validator.go が行う。
func (t Timeline) Validate() error {
	if len(t) == 0 {
		return errors.New("タイムラインが空です")
	}
	var errs []error
	seen := make(map[string]struct{}, len(t))
	usedCandidates := make(map[CandidateID]struct{}, len(t))
	for i, s := range t {
		if err := s.Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, dup := seen[s.ID]; dup {
			errs = append(errs, fmt.Errorf("セグメント ID が重複しています: %s", s.ID))
		}
		seen[s.ID] = struct{}{}

		// 同じ候補を 2 回訪れるプランは提示しない。
		if id := s.CandidateID(); id != "" {
			if _, dup := usedCandidates[id]; dup {
				errs = append(errs, fmt.Errorf("%s: 同じ候補が 2 回使われています: %s", s.ID, id))
			}
			usedCandidates[id] = struct{}{}
		}

		// 時刻は連続していなければならない。隙間があるなら buffer を挟むのが正しい。
		if i > 0 && !s.StartAt.Equal(t[i-1].EndAt) {
			errs = append(errs, fmt.Errorf("%s: 前のセグメントの終了時刻と連続していません（%s → %s）",
				s.ID, t[i-1].EndAt.Format(time.RFC3339), s.StartAt.Format(time.RFC3339)))
		}
	}
	return errors.Join(errs...)
}

// TotalWalk は徒歩の合計時間と距離を返す。
func (t Timeline) TotalWalk() (time.Duration, int) {
	var d time.Duration
	var meters int
	for _, s := range t {
		if s.Move != nil && s.Move.Key.Mode == TravelWalk {
			d += s.Move.Duration
			meters += s.Move.DistanceMeters
		}
	}
	return d, meters
}

// EstimatedCost は人数を与えて概算費用を集計する。
// 飲食は 1 人あたりの目安 x 人数、宿泊はプランの総額、交通は経路の運賃。
func (t Timeline) EstimatedCost(guests int) CostBreakdown {
	var c CostBreakdown
	for _, s := range t {
		switch {
		case s.Dining != nil:
			c.DiningJPY += s.Dining.CostFor(guests)
		case s.Lodging != nil && s.Lodging.Plan != nil:
			c.LodgingJPY += s.Lodging.Plan.TotalPriceJPY
		case s.Move != nil:
			c.TransitJPY += routeFare(s.Move)
		}
	}
	return c
}

// routeFare は経路の運賃を返す。経路全体の運賃が無ければ区間ごとの合算に落とす。
func routeFare(r *RouteFact) int {
	if r.FareJPY != nil {
		return *r.FareJPY
	}
	fare := 0
	for _, leg := range r.TransitLegs {
		if leg.FareJPY != nil {
			fare += *leg.FareJPY
		}
	}
	return fare
}

// Slack は index 番目のセグメントの終了時刻から締切（閉店・終電・チェックイン期限）までの
// 余裕を返す。負なら間に合っていない。TightSlack を下回る箇所があれば feasibility は tight。
func (t Timeline) Slack(index int, deadline time.Time) time.Duration {
	if index < 0 || index >= len(t) || deadline.IsZero() {
		return 0
	}
	return deadline.Sub(t[index].EndAt)
}

// ── リンク ────────────────────────────────────────────

// LinkKind はリンクの種類。
type LinkKind string

const (
	LinkAffiliate LinkKind = "affiliate"
	LinkOfficial  LinkKind = "official"
	LinkTel       LinkKind = "tel"
	LinkMap       LinkKind = "map"
)

// LinkProvider はリンクの提供元。
type LinkProvider string

const (
	LinkProviderRakutenTravel LinkProvider = "rakuten_travel"
	LinkProviderHotpepper     LinkProvider = "hotpepper"
	LinkProviderGurunavi      LinkProvider = "gurunavi"
	LinkProviderGoogleMaps    LinkProvider = "google_maps"
	LinkProviderDirect        LinkProvider = "direct"
)

// NewTrackingID はクリック計測 ID を採番する。
//
// リンク 1 本ごとに新しく振る。プラン ID や候補 ID を流用すると、
// 同じプランを 2 回開いたクリックが同一視されて計測が壊れる。
func NewTrackingID() string { return "trk_" + newULID(time.Now()) }

// Link は **収益がかかっている唯一の出口**。
// 素の URL 文字列を置かずこの構造に統一することで、ASP 追加時の全画面改修を防ぎ、
// rel="sponsored" とクリック計測をフロントの 1 コンポーネントで担保する。
type Link struct {
	Kind       LinkKind
	Provider   LinkProvider
	Label      string
	URL        string
	TrackingID string
}

// Validate はアフィリエイトリンクに計測 ID が付いていることを保証する。
// ここを緩めると計測漏れ＝収益の取りこぼしに直結する。
func (l Link) Validate() error {
	if l.URL == "" {
		return errors.New("リンクの URL が空です")
	}
	if l.Kind == LinkAffiliate && l.TrackingID == "" {
		return fmt.Errorf("アフィリエイトリンクに trackingId がありません: %s", l.Provider)
	}
	return nil
}

// ── 差し替え候補・警告 ────────────────────────────────

// Alternatives は UI の AlternativePicker が使う差し替え候補。
type Alternatives struct {
	Dining  []DiningAlternative
	Lodging []LodgingAlternative
}

type DiningAlternative struct {
	Place *PlaceFact
	// ExcludedReason は LLM の unusedCandidateNotes 由来。採用しなかった理由。
	ExcludedReason string
	Links          []Link
}

type LodgingAlternative struct {
	Choice         LodgingChoice
	ExcludedReason string
	Links          []Link
}

// WarningCode は UI に出す警告の種別。
type WarningCode string

const (
	WarnVacancyLow        WarningCode = "VACANCY_LOW"
	WarnLastTrainMissed   WarningCode = "LAST_TRAIN_MISSED"
	WarnNoLodgingFound    WarningCode = "NO_LODGING_FOUND"
	WarnNoDiningFound     WarningCode = "NO_DINING_FOUND"
	WarnWalkLimitExceeded WarningCode = "WALK_LIMIT_EXCEEDED"
	WarnBudgetExceeded    WarningCode = "BUDGET_EXCEEDED"
	WarnProviderDegraded  WarningCode = "PROVIDER_DEGRADED"
	WarnLLMUnavailable    WarningCode = "LLM_UNAVAILABLE"
	WarnTightSchedule     WarningCode = "TIGHT_SCHEDULE"
)

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Warning はプランに添える注意書き。SegmentID が空なら plan 全体に対する警告。
type Warning struct {
	Code      WarningCode
	Severity  Severity
	SegmentID string
	Message   string
}

func NewWarning(code WarningCode, sev Severity, segmentID, message string) Warning {
	return Warning{Code: code, Severity: sev, SegmentID: segmentID, Message: message}
}
