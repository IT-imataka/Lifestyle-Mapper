package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
	"github.com/taka/lifestyle-mapper/backend/internal/view"
)

// PlanSubscriber は 1 プランの生成イベントを購読する。
//
// 返すチャネルは生成の終了（plan / error イベント）で閉じる。
// ctx のキャンセルで購読を解除できることが実装の責務。
type PlanSubscriber interface {
	Subscribe(ctx context.Context, id model.PlanID) (<-chan model.PlanEvent, error)
}

// defaultHeartbeat は無音時に流すコメント行の間隔。
// ALB のアイドルタイムアウト（既定 60 秒）と、途中の Proxy による切断を避けるため。
const defaultHeartbeat = 20 * time.Second

// StreamController は GET /v1/plans/{planId}/events（SSE）。
//
// **体感速度の鍵**。外部 API 3 種 + LLM で 8〜15 秒かかるので、
// collecting → composing → completed を流しながら、確定した情報から順に描かせる。
type StreamController struct {
	sub       PlanSubscriber
	logger    *slog.Logger
	heartbeat time.Duration
}

func NewStreamController(sub PlanSubscriber, logger *slog.Logger, heartbeat time.Duration) *StreamController {
	if heartbeat <= 0 {
		heartbeat = defaultHeartbeat
	}
	return &StreamController{sub: sub, logger: loggerOr(logger), heartbeat: heartbeat}
}

func (c *StreamController) Events(w http.ResponseWriter, r *http.Request) {
	id := model.PlanID(r.PathValue("planId"))

	// 購読の可否は本文を書き始める前に決める。開始後は 404 を返す口がなくなる。
	events, err := c.sub.Subscribe(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}

	rc := http.NewResponseController(w)
	// SSE は分単位で開きっぱなしになるため、サーバの WriteTimeout を外す。
	// 外し忘れると生成の途中で必ず接続が切れる。
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		c.logger.DebugContext(r.Context(), "書き込み期限を解除できません", slog.String("error", err.Error()))
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// nginx / CloudFront に「溜めずに流せ」と伝える。溜められると SSE の意味が消える。
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	ticker := time.NewTicker(c.heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			// ブラウザが閉じた／再読み込みした。購読は Subscribe 側が ctx で解除する。
			return

		case <-ticker.C:
			// コメント行は EventSource には見えない。接続の維持だけが目的。
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			_ = rc.Flush()

		case e, ok := <-events:
			if !ok {
				return
			}
			ev, ok := view.NewStreamEvent(e, requestID(r))
			if !ok {
				continue
			}
			if err := writeSSE(w, ev); err != nil {
				c.logger.DebugContext(r.Context(), "SSE の送信に失敗しました",
					slog.String("requestId", requestID(r)), slog.String("error", err.Error()))
				return
			}
			_ = rc.Flush()
			if ev.Terminal() {
				// plan / error のあとは閉じる。フロントの再接続を誘わない。
				return
			}
		}
	}
}

// writeSSE は 1 イベントを text/event-stream の書式で書く。
func writeSSE(w http.ResponseWriter, ev view.StreamEvent) error {
	payload, err := json.Marshal(ev.Data)
	if err != nil {
		return err
	}
	// data は 1 行で書く。JSON に生の改行は入らないため分割は不要。
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, payload)
	return err
}
