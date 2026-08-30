// Package middleware は HTTP の横断的関心事を担う。
//
// 方針:
//   - **ビジネスロジックを持たない**。ここに条件分岐を足したくなったら controller か service を疑う。
//   - 適用順は router.New が唯一の正。個々のミドルウェアは順序に依存する処理を持たない。
//   - 応答本文の形は view/ の DTO に従う（ここでも独自の JSON を組まない）。
package middleware

import (
	"net"
	"net/http"
	"strings"
)

// Middleware は http.Handler を包む共通形。
type Middleware func(http.Handler) http.Handler

// Chain は与えられた順に **外側から** 適用する。
//
//	Chain(h, A, B) => A(B(h))
//
// 先に書いたものが先にリクエストを見る、という読み順にするための順序。
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// recorder は応答のステータスと転送量を記録する ResponseWriter。
//
// SSE のために Flush と Unwrap を通す。ここで包んだせいで
// http.Flusher / http.NewResponseController が使えなくなると、
// 進捗が届かず「8〜15 秒の無反応」に戻ってしまう。
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Status は書き込まれた応答ステータス。未書き込みなら 200 とみなす。
func (r *recorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// Written は既に応答を書き始めたかを返す。Recover が本文を差し替えてよいかの判断に使う。
func (r *recorder) Written() bool { return r.status != 0 }

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap により http.NewResponseController が元の ResponseWriter に届く。
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// clientIP はレート制限とログのための発信元を求める。
//
// trustProxy が真のときだけ X-Forwarded-For を信じる。ALB の背後では最左が実クライアントだが、
// 直接公開された環境で信じると **ヘッダ 1 行でレート制限を回避される**。
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, found := strings.Cut(xff, ","); found {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
