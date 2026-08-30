package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
	"github.com/taka/lifestyle-mapper/backend/internal/view"
)

// ClickRecorder はアフィリエイトのクリックを記録する。
type ClickRecorder interface {
	Record(ctx context.Context, e model.ClickEvent) error
}

// ClickController は POST /v1/clicks。
//
// フロントの AffiliateLink から sendBeacon / keepalive で送られ、
// **ユーザーは既に予約サイトへ遷移し始めている**。だから何が起きても 204 を返す。
// 計測の失敗でリンクの体験を落とすのは本末転倒で、失う価値の順序が逆になる。
type ClickController struct {
	recorder ClickRecorder
	logger   *slog.Logger
	now      func() time.Time
}

func NewClickController(recorder ClickRecorder, logger *slog.Logger, now func() time.Time) *ClickController {
	if now == nil {
		now = time.Now
	}
	return &ClickController{recorder: recorder, logger: loggerOr(logger), now: now}
}

func (c *ClickController) Record(w http.ResponseWriter, r *http.Request) {
	// 応答は常に 204。異常はログに残して、原因調査は後からできるようにする。
	defer w.WriteHeader(http.StatusNoContent)

	var req view.ClickRequest
	// ここでは decodeJSON を使わない。未知フィールドで弾くと、フロントの
	// 計測項目を増やしたときに古いサーバへの送信が丸ごと落ちる。
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err := dec.Decode(&req); err != nil {
		c.logger.WarnContext(r.Context(), "クリックの本文を解釈できません",
			slog.String("requestId", requestID(r)), slog.String("error", err.Error()))
		return
	}

	e := req.ToModel(c.now(), requestID(r))
	if !e.Valid() {
		c.logger.WarnContext(r.Context(), "計測に必要な項目が欠けたクリックを破棄しました",
			slog.String("requestId", requestID(r)), slog.String("planId", e.PlanID.String()))
		return
	}
	if c.recorder == nil {
		return
	}
	if err := c.recorder.Record(r.Context(), e); err != nil {
		// 収益に直結する数字なので、落ちたことは必ず気づける形で残す。
		c.logger.ErrorContext(r.Context(), "クリックの記録に失敗しました",
			slog.String("requestId", requestID(r)),
			slog.String("trackingId", e.TrackingID),
			slog.String("error", err.Error()))
	}
}
