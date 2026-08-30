package middleware

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/view"
)

// RateLimitConfig は 1 IP あたりの上限。
type RateLimitConfig struct {
	// PerMinute は 1 分あたりの許容リクエスト数。
	PerMinute int
	// Burst は瞬間的に許す件数。0 なら PerMinute と同じ（1 分ぶんを使い切れる）。
	Burst int
	// TrustProxy が真なら X-Forwarded-For を発信元とみなす（ALB の背後で必要）。
	TrustProxy bool
	// TTL はこの時間だけ使われなかった IP の記録を捨てる。
	TTL time.Duration
}

// RateLimit は IP 単位のトークンバケットで流量を絞る。
//
// **LLM 課金を守る最後の砦**。1 リクエストで外部 API 3 種 + LLM が動くため、
// 素朴なループで叩かれると 1 分で無視できない金額になる。
//
// 状態はプロセス内に持つ。複数タスクに増やしたら Redis へ移すこと（それまでは
// インスタンス数ぶん上限が緩むだけなので、実害が出る前に移せばよい）。
func RateLimit(cfg RateLimitConfig) Middleware {
	if cfg.PerMinute < 1 {
		cfg.PerMinute = 1
	}
	if cfg.Burst < 1 {
		cfg.Burst = cfg.PerMinute
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 10 * time.Minute
	}
	l := &limiterSet{
		limit: rate.Limit(float64(cfg.PerMinute) / 60.0),
		burst: cfg.Burst,
		ttl:   cfg.TTL,
		byIP:  make(map[string]*visitor),
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if l.allow(clientIP(r, cfg.TrustProxy), time.Now()) {
				next.ServeHTTP(w, r)
				return
			}
			// 待てば通ることを伝える。フロントは再試行の間隔をここから決められる。
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(cfg.PerMinute)))
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			status, body := view.NewError(apperror.RateLimited("middleware.RateLimit"), RequestIDFrom(r.Context()))
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		})
	}
}

func retryAfterSeconds(perMinute int) int {
	s := 60 / perMinute
	if s < 1 {
		return 1
	}
	return s
}

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type limiterSet struct {
	limit rate.Limit
	burst int
	ttl   time.Duration

	mu        sync.Mutex
	byIP      map[string]*visitor
	lastSweep time.Time
}

func (l *limiterSet) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 掃除は専用の goroutine を持たず、この呼び出しに相乗りさせる。
	// 停止処理を持たないぶん、サーバの終了で取りこぼす後始末が無い。
	if now.Sub(l.lastSweep) > l.ttl {
		for ip, v := range l.byIP {
			if now.Sub(v.lastSeen) > l.ttl {
				delete(l.byIP, ip)
			}
		}
		l.lastSweep = now
	}

	v, ok := l.byIP[ip]
	if !ok {
		v = &visitor{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.byIP[ip] = v
	}
	v.lastSeen = now
	return v.limiter.AllowN(now, 1)
}
