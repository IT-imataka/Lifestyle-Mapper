package middleware

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/view"
)

// Recover は panic を 500 に変えてプロセスを守る。
//
// 個人開発では 1 か所の nil 参照でサーバごと落ちるのが一番痛い。
// スタックはログにだけ出し、応答には内部情報を一切載せない。
func Recover(logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &recorder{ResponseWriter: w}
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				// http.ErrAbortHandler は「意図的な中断」なので握り潰さず投げ直す。
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				requestID := RequestIDFrom(r.Context())
				logger.LogAttrs(r.Context(), slog.LevelError, "panic",
					slog.String("requestId", requestID),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("panic", fmt.Sprint(v)),
					slog.String("stack", string(debug.Stack())),
				)
				// SSE の途中など、既に本文を書き始めていたら差し替えられない。
				// 中途半端な JSON を継ぎ足すより、接続を切って気づかせるほうがよい。
				if rec.Written() {
					return
				}
				status, body := view.NewError(apperror.New(apperror.CodeInternal, ""), requestID)
				rec.Header().Set("Content-Type", "application/json; charset=utf-8")
				rec.WriteHeader(status)
				_ = json.NewEncoder(rec).Encode(body)
			}()
			next.ServeHTTP(rec, r)
		})
	}
}
