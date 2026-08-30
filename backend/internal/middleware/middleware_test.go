package middleware

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/view"
)

// discardLogger はテスト出力を汚さないためのロガー。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
}

func TestChainAppliesOutsideIn(t *testing.T) {
	var order []string
	mark := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(okHandler(), mark("A"), mark("B"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if strings.Join(order, ",") != "A,B" {
		t.Errorf("適用順 = %v, want [A B]", order)
	}
}

func TestRequestIDGeneratesAndEchoes(t *testing.T) {
	var seen string
	h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if seen == "" || !strings.HasPrefix(seen, "req_") {
		t.Errorf("生成された ID = %q", seen)
	}
	// 応答ヘッダにも同じ ID を載せる。ユーザーの問い合わせとログを結ぶ手がかりになる。
	if got := rec.Header().Get(RequestIDHeader); got != seen {
		t.Errorf("ヘッダの ID = %q, context = %q", got, seen)
	}

	// 呼び出し側の ID は引き継ぐ（BFF からの追跡が切れないように）。
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "req_from_bff")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "req_from_bff" {
		t.Errorf("引き継いだ ID = %q", seen)
	}

	// 制御文字入りはログ偽装・ヘッダ分割の材料になるので捨てて採番し直す。
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "req_bad\ninjected: 1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(seen, "\n") || seen == "req_bad\ninjected: 1" {
		t.Errorf("不正な ID をそのまま採用しました: %q", seen)
	}
}

func TestRecoverReturnsInternalErrorAndHidesStack(t *testing.T) {
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("接続先のポインタが nil でした")
	}), RequestID(), Recover(discardLogger()))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/plans", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	var body view.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("応答が JSON ではありません: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Code != string(apperror.CodeInternal) {
		t.Errorf("code = %q", body.Error.Code)
	}
	// panic の中身が外に出ていないこと。
	if strings.Contains(rec.Body.String(), "nil でした") {
		t.Error("panic の内容が応答に漏れています")
	}
	if body.Error.RequestID == "" {
		t.Error("requestId が入っていません")
	}
}

// 書き込み済みの応答（SSE の途中など）に 500 の JSON を継ぎ足さない。
func TestRecoverKeepsAlreadyWrittenResponse(t *testing.T) {
	h := Recover(discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: status\n"))
		panic("途中で壊れた")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/plans/pln_1/events", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200（書き込み済みなので変えられない）", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "INTERNAL_ERROR") {
		t.Errorf("本文に JSON が継ぎ足されました: %q", rec.Body.String())
	}
}

func TestCORSAllowsOnlyConfiguredOrigins(t *testing.T) {
	h := CORS(CORSConfig{AllowedOrigins: []string{"https://lifestyle-mapper.app"}})(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/v1/plans/pln_1", nil)
	req.Header.Set("Origin", "https://lifestyle-mapper.app")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://lifestyle-mapper.app" {
		t.Errorf("許可オリジンへの応答ヘッダ = %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/plans/pln_1", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("未許可オリジンにヘッダを付けました: %q", got)
	}
	// 拒否そのものはブラウザの仕事。サーバは通常どおり処理する。
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	// オリジンごとに応答が変わることをキャッシュに伝える。
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary = %q", rec.Header().Get("Vary"))
	}
}

func TestCORSPreflight(t *testing.T) {
	h := CORS(CORSConfig{AllowedOrigins: []string{"http://localhost:3000"}})(okHandler())

	req := httptest.NewRequest(http.MethodOptions, "/v1/plans", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), http.MethodPost) {
		t.Errorf("Allow-Methods = %q", rec.Header().Get("Access-Control-Allow-Methods"))
	}
	if rec.Body.Len() != 0 {
		t.Errorf("preflight で本文を返しました: %q", rec.Body.String())
	}
}

func TestRateLimitBlocksAfterBurst(t *testing.T) {
	h := Chain(okHandler(), RequestID(), RateLimit(RateLimitConfig{PerMinute: 2}))

	call := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/plans", nil)
		req.RemoteAddr = ip + ":50000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for i := 1; i <= 2; i++ {
		if got := call("203.0.113.1").Code; got != http.StatusOK {
			t.Fatalf("%d 回目 status = %d, want 200", i, got)
		}
	}
	rec := call("203.0.113.1")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("上限超過の status = %d", rec.Code)
	}
	// 待てば通ることを伝える。フロントが再試行の間隔を決められる。
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After がありません")
	}
	var body view.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != string(apperror.CodeRateLimited) {
		t.Errorf("本文 = %s", rec.Body.String())
	}

	// 制限は IP ごと。別のユーザーを巻き込まない。
	if got := call("203.0.113.9").Code; got != http.StatusOK {
		t.Errorf("別 IP の status = %d, want 200", got)
	}
}

// X-Forwarded-For を無条件に信じると、ヘッダ 1 行で制限を回避される。
func TestRateLimitTrustsProxyOnlyWhenConfigured(t *testing.T) {
	forge := func(h http.Handler, xff string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/plans", nil)
		req.RemoteAddr = "10.0.0.1:50000"
		req.Header.Set("X-Forwarded-For", xff)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	untrusted := RateLimit(RateLimitConfig{PerMinute: 1})(okHandler())
	forge(untrusted, "1.1.1.1")
	if got := forge(untrusted, "2.2.2.2"); got != http.StatusTooManyRequests {
		t.Errorf("XFF を詐称して制限を回避できました: status = %d", got)
	}

	trusted := RateLimit(RateLimitConfig{PerMinute: 1, TrustProxy: true})(okHandler())
	forge(trusted, "1.1.1.1")
	if got := forge(trusted, "2.2.2.2"); got != http.StatusOK {
		t.Errorf("ALB 背後で別クライアントを巻き込みました: status = %d", got)
	}
}

func TestLoggerRecordsStatus(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}), RequestID(), Logger(LoggerConfig{Logger: logger}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/plans", nil))

	out := buf.String()
	for _, want := range []string{"status=202", "path=/v1/plans", "requestId=req_"} {
		if !strings.Contains(out, want) {
			t.Errorf("ログに %q がありません: %s", want, out)
		}
	}
}

// SSE のために Flusher が包んだ先まで通ること。ここが切れると進捗が届かない。
func TestRecorderKeepsFlusher(t *testing.T) {
	var flushed bool
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		_, _ = w.Write([]byte("event: status\n\n"))
		if err := rc.Flush(); err == nil {
			flushed = true
		}
	}), Logger(LoggerConfig{Logger: discardLogger()}), Recover(discardLogger()))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/plans/pln_1/events", nil))
	if !flushed {
		t.Error("ミドルウェアが Flush を遮っています")
	}
}

func TestRateLimitEvictsIdleVisitors(t *testing.T) {
	l := &limiterSet{limit: 1, burst: 1, ttl: time.Minute, byIP: map[string]*visitor{}}
	now := time.Now()

	l.allow("203.0.113.1", now)
	if len(l.byIP) != 1 {
		t.Fatalf("記録数 = %d", len(l.byIP))
	}
	// TTL を過ぎた記録は次の呼び出しに相乗りして掃除される。
	l.allow("203.0.113.2", now.Add(2*time.Minute))
	if _, stale := l.byIP["203.0.113.1"]; stale {
		t.Errorf("古い記録が残っています: %v", l.byIP)
	}
}
