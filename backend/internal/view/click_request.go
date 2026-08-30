package view

import (
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// ClickRequest は POST /v1/clicks のリクエストボディ。
//
// フロントの AffiliateLink からのみ呼ばれ、sendBeacon / keepalive で送られる。
// **リンク遷移を止めないことが最優先**なので、検証に落ちても API は 204 を返す。
// 「捨てるかどうか」は model.ClickEvent.Valid が決める。
type ClickRequest struct {
	PlanID     string  `json:"planId"`
	SegmentID  *string `json:"segmentId"`
	TrackingID string  `json:"trackingId"`
	Provider   string  `json:"provider"`
	ClickedAt  *string `json:"clickedAt"`
}

// ToModel はクリックをドメインモデルへ移す。
// clickedAt が無い・読めない場合はサーバ時刻で補う（計測を落とさないため）。
func (r ClickRequest) ToModel(now time.Time, requestID string) model.ClickEvent {
	e := model.ClickEvent{
		PlanID:     model.PlanID(r.PlanID),
		SegmentID:  deref(r.SegmentID),
		TrackingID: r.TrackingID,
		Provider:   model.LinkProvider(r.Provider),
		ClickedAt:  now,
		RequestID:  requestID,
	}
	if r.ClickedAt != nil {
		if t, err := time.Parse(time.RFC3339, *r.ClickedAt); err == nil {
			e.ClickedAt = t
		}
	}
	return e
}
