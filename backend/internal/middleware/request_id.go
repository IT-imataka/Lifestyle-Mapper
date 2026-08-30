package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// RequestIDHeader は受け取りと返却の両方に使うヘッダ名。
const RequestIDHeader = "X-Request-ID"

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID はリクエストに一意な ID を付け、context と応答ヘッダに載せる。
//
// meta.requestId / error.requestId として API に出るので、
// ユーザーからの問い合わせとログを 1 本の線で結べる。
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := sanitizeRequestID(r.Header.Get(RequestIDHeader))
			if id == "" {
				id = newRequestID()
			}
			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
		})
	}
}

// RequestIDFrom は context から ID を取り出す。無ければ空文字。
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// ID が採れなくてもリクエストは通す。追跡性より可用性を採る。
		return "req_unknown"
	}
	return "req_" + hex.EncodeToString(b[:])
}

// sanitizeRequestID は外から来た ID を英数字・ハイフン・アンダースコアに限り、64 文字で切る。
// 改行や制御文字をそのまま通すと、ログを偽装されたりヘッダを分割されたりする。
func sanitizeRequestID(v string) string {
	if len(v) > 64 {
		v = v[:64]
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		ok := c == '-' || c == '_' ||
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !ok {
			return ""
		}
	}
	return v
}
