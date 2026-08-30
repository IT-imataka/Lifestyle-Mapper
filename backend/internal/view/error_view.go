package view

import (
	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
)

// ErrorEnvelope は失敗応答の外枠。成功と同じ枠に混ぜない。
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	Details   []FieldError `json:"details,omitempty"`
	RequestID string       `json:"requestId,omitempty"`
}

// FieldError はどの入力項目がなぜ弾かれたか。field は openapi のリクエスト JSON のパスに一致する。
type FieldError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// NewError は任意の error を応答用に正規化し、HTTP ステータスと本文を返す。
//
// **controller はこの関数だけを通す**。ドメインエラー ⇔ ステータスの対応表は
// apperror にしかなく、ここで自前の分岐を足さない。
// 内部原因（cause）と発生箇所（Op）はログ専用なので本文には出さない。
func NewError(err error, requestID string) (int, ErrorEnvelope) {
	e := apperror.From(err)
	return e.HTTPStatus(), ErrorEnvelope{Error: newErrorBody(e, requestID)}
}

func newErrorBody(e *apperror.Error, requestID string) ErrorBody {
	if e == nil {
		return ErrorBody{}
	}
	body := ErrorBody{Code: string(e.Code), Message: e.Message, RequestID: requestID}
	for _, d := range e.Details {
		body.Details = append(body.Details, FieldError{Field: d.Field, Reason: d.Reason})
	}
	return body
}
