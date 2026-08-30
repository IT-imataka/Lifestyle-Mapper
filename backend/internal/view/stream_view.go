package view

import (
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// このファイルは GET /v1/plans/{planId}/events（SSE）の表現。
//
// **data の形は GET /v1/plans/{planId} と同一**にしてある。フロントが「初回描画」と
// 「ストリーム更新」で別の型を扱わずに済むことが、この設計の目的（設計書 2-B-2 の 3）。

// StreamEvent は SSE の 1 イベント。Name は openapi の event 名と一致する。
type StreamEvent struct {
	Name string
	// Data は JSON へ直列化してそのまま data: 行に載せる。
	Data any
}

// StatusPayload は event: status の data。
type StatusPayload struct {
	Status string `json:"status"`
}

// NewStreamEvent はドメインのイベントを SSE の 1 イベントに移す。
// 対応する中身が無いイベントは配信しない（ok=false）。
func NewStreamEvent(e model.PlanEvent, requestID string) (StreamEvent, bool) {
	switch e.Type {
	case model.EventStatus:
		return StreamEvent{Name: string(model.EventStatus), Data: StatusPayload{Status: string(e.Status)}}, true

	case model.EventSources:
		if e.Source == nil {
			return StreamEvent{}, false
		}
		return StreamEvent{Name: string(model.EventSources), Data: newSourceStatus(*e.Source)}, true

	case model.EventPlan:
		if e.Plan == nil {
			return StreamEvent{}, false
		}
		return StreamEvent{Name: string(model.EventPlan), Data: NewPlan(e.Plan)}, true

	case model.EventError:
		// エラーだけは外枠を被せず ErrorBody をそのまま流す（openapi の表の通り）。
		return StreamEvent{Name: string(model.EventError), Data: newErrorBody(e.Err, requestID)}, true
	}
	return StreamEvent{}, false
}

// Terminal はこのイベントの後にストリームを閉じるべきかを返す。
// plan / error の送信後、サーバは接続を閉じる。
func (e StreamEvent) Terminal() bool {
	return e.Name == string(model.EventPlan) || e.Name == string(model.EventError)
}
