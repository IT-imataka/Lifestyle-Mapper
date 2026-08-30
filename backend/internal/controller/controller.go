// Package controller は HTTP 境界を担う。
//
// 不変のルール:
//   - **ロジックを持たない**。やることは「入力の解釈 → service 呼び出し → view で整形」の 3 つだけ。
//     判断（候補の選定・時刻の計算・整合性の検査）は service にしか置かない。
//   - 依存は interface で受け、この層で定義する（利用側に interface を置く Go の作法）。
//     実装は cmd/api が組み立てて注入する。
//   - エラーはすべて view.NewError を通す。ステータスの分岐をここに書かない。
package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/middleware"
	"github.com/taka/lifestyle-mapper/backend/internal/view"
)

// maxRequestBody はリクエスト本文の上限。
// 検索条件は大きくても数 KB で、notes も 200 字に制限されている。
const maxRequestBody = 64 << 10

func requestID(r *http.Request) string { return middleware.RequestIDFrom(r.Context()) }

// writeJSON は成功応答を書く。
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	// ここで失敗するのは接続断がほとんど。ヘッダは送信済みで打つ手がないので握る。
	_ = json.NewEncoder(w).Encode(body)
}

// writeError はドメインエラーを HTTP に写す。**controller の唯一のエラー出口**。
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, body := view.NewError(err, requestID(r))
	writeJSON(w, status, body)
}

// decodeJSON はリクエスト本文を DTO に読み込む。
//
// 未知のフィールドを弾くのは openapi が全オブジェクトに additionalProperties: false を
// 宣言しているため。typo した項目が黙って無視され「指定したのに効かない」と悩む時間をなくす。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *apperror.Error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// 本文に 2 つ目の JSON 値が続いていたら、送り手が意図と違うものを組んでいる。
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return apperror.Validation(apperror.FieldError{
			Field: "", Reason: "リクエスト本文は 1 つの JSON オブジェクトにしてください",
		})
	}
	return nil
}

// decodeError は json の失敗を、フロントが項目に対応付けられる形に翻訳する。
func decodeError(err error) *apperror.Error {
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	var tooLarge *http.MaxBytesError

	switch {
	case errors.Is(err, io.EOF):
		return apperror.Validation(apperror.FieldError{Field: "", Reason: "リクエスト本文が空です"})
	case errors.As(err, &tooLarge):
		return apperror.Validation(apperror.FieldError{
			Field: "", Reason: fmt.Sprintf("リクエスト本文が大きすぎます（上限 %d バイト）", tooLarge.Limit),
		})
	case errors.As(err, &typeErr):
		return apperror.Validation(apperror.FieldError{
			Field:  typeErr.Field,
			Reason: fmt.Sprintf("%s ではなく %s で指定してください", typeErr.Value, typeErr.Type),
		})
	case errors.As(err, &syntaxErr):
		return apperror.Validation(apperror.FieldError{
			Field:  "",
			Reason: fmt.Sprintf("JSON として解釈できません（%d バイト目）", syntaxErr.Offset),
		})
	}
	// DisallowUnknownFields の違反はここに来る（型付きエラーが用意されていない）。
	return apperror.Validation(apperror.FieldError{Field: "", Reason: unknownFieldReason(err)})
}

func unknownFieldReason(err error) string {
	const prefix = "json: unknown field "
	msg := err.Error()
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return "未知の項目が含まれています: " + msg[len(prefix):]
	}
	return "リクエスト本文を解釈できません"
}

func loggerOr(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
