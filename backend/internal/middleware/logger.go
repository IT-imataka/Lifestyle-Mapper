package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// LoggerConfig はアクセスログの設定。
type LoggerConfig struct {
	Logger *slog.Logger
	// TrustProxy が真なら X-Forwarded-For を発信元として記録する。
	TrustProxy bool
	// QuietPaths はログを Debug に落とすパス。
	// ALB のヘルスチェックが毎秒叩くため、既定で /health を含める。
	QuietPaths []string
}

// Logger は 1 リクエスト 1 行の構造化ログを出す。
//
// requestId を必ず含めるので、ユーザーに見えている error.requestId から
// サーバ側のログへ直行できる。RequestID より内側に置くこと。
func Logger(cfg LoggerConfig) Middleware {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	quiet := map[string]bool{"/health": true}
	for _, p := range cfg.QuietPaths {
		quiet[p] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w}

			next.ServeHTTP(rec, r)

			level := slog.LevelInfo
			switch {
			case quiet[r.URL.Path]:
				level = slog.LevelDebug
			case rec.Status() >= 500:
				level = slog.LevelError
			case rec.Status() >= 400:
				level = slog.LevelWarn
			}
			logger.LogAttrs(r.Context(), level, "http",
				slog.String("requestId", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.Status()),
				slog.Int("bytes", rec.bytes),
				slog.Int64("durationMs", time.Since(start).Milliseconds()),
				slog.String("ip", clientIP(r, cfg.TrustProxy)),
				slog.String("ua", r.UserAgent()),
			)
		})
	}
}
