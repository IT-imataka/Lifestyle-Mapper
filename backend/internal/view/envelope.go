// Package view は API のレスポンス表現（DTO）を定義する。
//
// **JSON タグを持つのはこの層だけ**。model にタグを付けない規約により、
// DB カラムや外部 API の項目が増えても API 契約には漏れない。
//
// 形は schemas/openapi.yaml が正であり、フロントの型はそこから自動生成される。
// ここを変えたら openapi.yaml も同時に変えること（片側だけの変更は契約違反）。
package view

import (
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// Envelope は成功応答の外枠 {data, meta}。
type Envelope[T any] struct {
	Data T    `json:"data"`
	Meta Meta `json:"meta"`
}

// Meta は並行 fan-out の部分失敗を隠さず返すための観測情報。
type Meta struct {
	RequestID string `json:"requestId"`
	// Sources は必ず配列で返す（null と空配列をフロントに区別させない）。
	Sources []SourceStatus `json:"sources"`
	// LLM は使わなかった場合 null。
	LLM *LLMMeta `json:"llm"`
}

func NewMeta(requestID string, sources []model.SourceStatus, llm *model.LLMMeta) Meta {
	m := Meta{RequestID: requestID, Sources: make([]SourceStatus, 0, len(sources))}
	for _, s := range sources {
		m.Sources = append(m.Sources, newSourceStatus(s))
	}
	if llm != nil && llm.Provider != "" {
		v := newLLMMeta(*llm)
		m.LLM = &v
	}
	return m
}

type SourceStatus struct {
	Provider    string     `json:"provider"`
	Status      string     `json:"status"`
	LatencyMs   int        `json:"latencyMs"`
	Cached      bool       `json:"cached"`
	ResultCount int        `json:"resultCount"`
	Error       *ErrorBody `json:"error,omitempty"`
}

func newSourceStatus(s model.SourceStatus) SourceStatus {
	out := SourceStatus{
		Provider:    string(s.Provider),
		Status:      string(s.State),
		LatencyMs:   int(s.Latency.Milliseconds()),
		Cached:      s.Cached,
		ResultCount: s.ResultCount,
	}
	if s.Err != nil {
		body := newErrorBody(s.Err, "")
		out.Error = &body
	}
	return out
}

// LLMMeta は文章生成の実績。repairAttempts は**プロンプト品質の実測値**なので、
// 隠さず返してメトリクスに流せるようにする。
type LLMMeta struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	SchemaVersion  string `json:"schemaVersion"`
	PromptVersion  string `json:"promptVersion"`
	LatencyMs      int    `json:"latencyMs"`
	InputTokens    int    `json:"inputTokens"`
	OutputTokens   int    `json:"outputTokens"`
	RepairAttempts int    `json:"repairAttempts"`
}

func newLLMMeta(m model.LLMMeta) LLMMeta {
	return LLMMeta{
		Provider:       string(m.Provider),
		Model:          m.Model,
		SchemaVersion:  m.SchemaVersion,
		PromptVersion:  m.PromptVersion,
		LatencyMs:      int(m.Latency.Milliseconds()),
		InputTokens:    m.InputTokens,
		OutputTokens:   m.OutputTokens,
		RepairAttempts: m.RepairAttempts,
	}
}

// PlanCreated は POST /v1/plans の 202 応答。
type PlanCreated struct {
	PlanID    string `json:"planId"`
	Status    string `json:"status"`
	StreamURL string `json:"streamUrl,omitempty"`
}

func NewPlanCreated(id model.PlanID, status model.PlanStatus) PlanCreated {
	return PlanCreated{
		PlanID:    id.String(),
		Status:    string(status),
		StreamURL: "/v1/plans/" + id.String() + "/events",
	}
}

// rfc3339 は時刻をオフセット付きで出す。未設定は空文字（omitempty で消える）。
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func minutes(d time.Duration) int { return int(d.Minutes()) }
