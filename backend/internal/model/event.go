package model

import (
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
)

// このファイルはプラン生成の過程で起きた「出来事」を扱う。
// SSE の配信単位であると同時に、event_repository が永続化する監査ログでもある。

// PlanEventType は SSE のイベント名と一致する。
type PlanEventType string

const (
	EventStatus  PlanEventType = "status"
	EventSources PlanEventType = "sources"
	EventPlan    PlanEventType = "plan"
	EventError   PlanEventType = "error"
)

// PlanEvent は生成パイプラインが発行するイベント 1 件。
// Type に対応するフィールドのみが埋まる。
type PlanEvent struct {
	PlanID     PlanID
	Type       PlanEventType
	OccurredAt time.Time

	Status PlanStatus      // Type == EventStatus
	Source *SourceStatus   // Type == EventSources
	Plan   *Plan           // Type == EventPlan
	Err    *apperror.Error // Type == EventError
}

// Terminal はこのイベントの後にストリームを閉じるべきかを返す。
func (e PlanEvent) Terminal() bool {
	return e.Type == EventPlan || e.Type == EventError
}

func NewStatusEvent(id PlanID, s PlanStatus, at time.Time) PlanEvent {
	return PlanEvent{PlanID: id, Type: EventStatus, OccurredAt: at, Status: s}
}

func NewSourceEvent(id PlanID, s SourceStatus, at time.Time) PlanEvent {
	return PlanEvent{PlanID: id, Type: EventSources, OccurredAt: at, Source: &s}
}

func NewPlanEvent(p *Plan, at time.Time) PlanEvent {
	return PlanEvent{PlanID: p.ID, Type: EventPlan, OccurredAt: at, Plan: p}
}

func NewErrorEvent(id PlanID, err error, at time.Time) PlanEvent {
	return PlanEvent{PlanID: id, Type: EventError, OccurredAt: at, Err: apperror.From(err)}
}

// ── 外部プロバイダの取得結果 ──────────────────────────

// Provider は事実の取得元。
type Provider string

const (
	ProviderGooglePlaces  Provider = "google_places"
	ProviderGoogleRoutes  Provider = "google_routes"
	ProviderRakutenTravel Provider = "rakuten_travel"
)

// SourceState は各プロバイダの取得結果。
type SourceState string

const (
	SourceOK       SourceState = "ok"
	SourceDegraded SourceState = "degraded" // 取れたが不完全、または遅延で一部打ち切り
	SourceFailed   SourceState = "failed"
	SourceSkipped  SourceState = "skipped" // 条件により呼ばなかった（宿泊無効など）
)

// SourceStatus は並行 fan-out の結果を隠さず返すための観測情報。
// 楽天が落ちても飲食だけ返して partial にするほうが、全体 500 より UX が良い。
type SourceStatus struct {
	Provider    Provider
	State       SourceState
	Latency     time.Duration
	Cached      bool
	ResultCount int
	Err         *apperror.Error
}

// Degraded は「呼んだが使える結果が得られなかった」かを返す。partial 判定に使う。
func (s SourceStatus) Degraded() bool {
	return s.State == SourceDegraded || s.State == SourceFailed
}

// ── LLM 呼び出しの観測情報 ────────────────────────────

// LLMProvider は文章生成の担い手。rule_based は LLM 全滅時のフォールバック。
type LLMProvider string

const (
	LLMAnthropic LLMProvider = "anthropic"
	LLMGoogle    LLMProvider = "google"
	LLMRuleBased LLMProvider = "rule_based"
)

// LLMMeta は 1 プランぶんの LLM 呼び出し実績。
type LLMMeta struct {
	Provider      LLMProvider
	Model         string
	SchemaVersion string // 例: plan_output.v1
	PromptVersion string // 例: plan_v1
	Latency       time.Duration
	InputTokens   int
	OutputTokens  int
	// RepairAttempts は Validator 不合格による再生成回数。
	// **プロンプト品質の実測値**なのでメトリクスに流して改善サイクルを回す。
	RepairAttempts int
}

// ── アフィリエイトクリック ────────────────────────────

// ClickEvent は POST /v1/clicks で記録するクリック。
// 計測のためにユーザー体験を止めないため、検証に失敗しても API は 204 を返す。
type ClickEvent struct {
	PlanID     PlanID
	SegmentID  string
	TrackingID string
	Provider   LinkProvider
	ClickedAt  time.Time
	RequestID  string
}

// Valid は記録に値するクリックかを返す。偽なら破棄するが、応答は 204 のまま。
func (c ClickEvent) Valid() bool {
	return c.PlanID.Valid() && c.TrackingID != ""
}
