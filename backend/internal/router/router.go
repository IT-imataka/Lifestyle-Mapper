// Package router は URL とハンドラの対応、およびミドルウェアの適用順を定める。
//
// **このファイルが API の地図**。openapi.yaml の paths と 1 対 1 で対応させ、
// どちらか片方だけを増やさないこと。
package router

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/controller"
	"github.com/taka/lifestyle-mapper/backend/internal/middleware"
	"github.com/taka/lifestyle-mapper/backend/internal/view"
)

// Deps は各コントローラ。cmd/api が組み立てて渡す。
type Deps struct {
	Plan   *controller.PlanController
	Stream *controller.StreamController
	Click  *controller.ClickController
	Health *controller.HealthController
}

// Config はミドルウェアの設定。値は config.Config から写す。
type Config struct {
	CORSOrigins []string
	// RateLimitPerMinute は 1 IP あたりの上限。LLM 課金を守る最後の砦。
	RateLimitPerMinute int
	// TrustProxy は ALB / CloudFront の背後で真にする。
	// 直接公開している環境で真にすると、ヘッダ 1 行でレート制限を回避される。
	TrustProxy bool
	Logger     *slog.Logger
}

// New はルーティング済みのハンドラを返す。
//
// ミドルウェアの適用順（外側から）:
//
//		RequestID → Logger → Recover → CORS → [/v1 のみ] RateLimit
//
//	  - RequestID が最初なのは、以降すべてのログと応答本文に ID を載せるため。
//	  - Logger が Recover の外なのは、panic で 500 になった事実もアクセスログに残すため。
//	  - RateLimit を /v1 だけに掛けるのは、ALB のヘルスチェックが数秒ごとに叩く
//	    /health を制限に巻き込まないため。
func New(cfg Config, deps Deps) http.Handler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// ── /v1: 業務エンドポイント ──
	v1 := http.NewServeMux()
	if deps.Plan != nil {
		v1.HandleFunc("POST /v1/plans", deps.Plan.Create)
		v1.HandleFunc("GET /v1/plans/{planId}", deps.Plan.Get)
	}
	if deps.Stream != nil {
		v1.HandleFunc("GET /v1/plans/{planId}/events", deps.Stream.Events)
	}
	if deps.Click != nil {
		v1.HandleFunc("POST /v1/clicks", deps.Click.Record)
	}

	rateLimited := middleware.RateLimit(middleware.RateLimitConfig{
		PerMinute:  cfg.RateLimitPerMinute,
		TrustProxy: cfg.TrustProxy,
	})(v1)

	root := http.NewServeMux()
	root.Handle("/v1/", rateLimited)
	if deps.Health != nil {
		root.HandleFunc("GET /health", deps.Health.Get)
	}
	// ServeMux 既定の 404 は text/plain。フロントが常に同じ形の JSON を読めるよう差し替える。
	root.HandleFunc("/", notFound)

	return middleware.Chain(root,
		middleware.RequestID(),
		middleware.Logger(middleware.LoggerConfig{Logger: logger, TrustProxy: cfg.TrustProxy}),
		middleware.Recover(logger),
		middleware.CORS(middleware.CORSConfig{
			AllowedOrigins: cfg.CORSOrigins,
			MaxAge:         10 * time.Minute,
		}),
	)
}

func notFound(w http.ResponseWriter, r *http.Request) {
	status, body := view.NewError(
		apperror.New(apperror.CodeNotFound, "そのようなエンドポイントはありません"),
		middleware.RequestIDFrom(r.Context()),
	)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
