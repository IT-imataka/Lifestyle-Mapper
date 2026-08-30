package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CORSConfig は許可するオリジンと preflight の設定。
type CORSConfig struct {
	// AllowedOrigins は完全一致で照合する。config が本番でのワイルドカードを禁じている。
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	MaxAge         time.Duration
}

// CORS は許可されたオリジンにだけ応答ヘッダを付ける。
//
// フロントは通常 BFF（app/api/）経由でサーバ間通信するため、ここが効くのは
// ブラウザから直接叩く経路だけ。**許可しないオリジンにはヘッダを付けない**のが基本で、
// 拒否そのものはブラウザに任せる（サーバが 403 を返す必要はない）。
func CORS(cfg CORSConfig) Middleware {
	allowed := make(map[string]bool, len(cfg.AllowedOrigins))
	wildcard := false
	for _, o := range cfg.AllowedOrigins {
		o = strings.TrimRight(strings.TrimSpace(o), "/")
		if o == "*" {
			wildcard = true
		}
		allowed[o] = true
	}

	methods := strings.Join(orDefault(cfg.AllowedMethods, []string{http.MethodGet, http.MethodPost, http.MethodOptions}), ", ")
	headers := strings.Join(orDefault(cfg.AllowedHeaders, []string{"Content-Type", RequestIDHeader}), ", ")
	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = 10 * time.Minute
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			// オリジンごとに応答が変わることをキャッシュに伝える。
			w.Header().Add("Vary", "Origin")

			if origin != "" && (wildcard || allowed[origin]) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Expose-Headers", RequestIDHeader)
			}

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Add("Vary", "Access-Control-Request-Method")
				w.Header().Set("Access-Control-Allow-Methods", methods)
				w.Header().Set("Access-Control-Allow-Headers", headers)
				w.Header().Set("Access-Control-Max-Age", strconv.Itoa(int(maxAge.Seconds())))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func orDefault(v, def []string) []string {
	if len(v) == 0 {
		return def
	}
	return v
}
