// Package apperror はドメインエラーの語彙と、HTTP ステータスへの対応表を提供する。
//
// 方針:
//   - エラーコードは API 契約（schemas/openapi.yaml の ErrorBody.code）に出る文字列と一致させる。
//   - JSON タグは持たない。レスポンス整形は view/error_view.go の責務。
//   - HTTP ステータスは openapi.yaml が宣言している 400 / 404 / 410 / 429 / 500 の
//     範囲に収める。上流 API や LLM の失敗は本来 HTTP には出ず、
//     `status: "partial"` + warnings として返るため（設計書 2-B-2）、
//     ここでの 500 は「万一そのまま外に出てしまった場合」の保険にすぎない。
package apperror

import (
	"errors"
	"fmt"
	"net/http"
)

// Code は API 契約に露出するエラーコード。
type Code string

const (
	// ── クライアント起因 ──
	CodeValidationFailed Code = "VALIDATION_FAILED"
	CodePlanNotFound     Code = "PLAN_NOT_FOUND"
	// CodeNotFound は URL そのものが存在しない場合。プランの不在とは区別する
	// （フロントが「再検索を促す」か「不具合として扱う」かを取り違えないため）。
	CodeNotFound    Code = "NOT_FOUND"
	CodePlanExpired Code = "PLAN_EXPIRED"
	CodeRateLimited Code = "RATE_LIMITED"

	// ── 上流 API 起因（通常は meta.sources に載り、HTTP には出ない） ──
	CodeUpstreamTimeout     Code = "UPSTREAM_TIMEOUT"
	CodeUpstreamFailed      Code = "UPSTREAM_FAILED"
	CodeUpstreamRateLimited Code = "UPSTREAM_RATE_LIMITED"
	CodeNoCandidatesFound   Code = "NO_CANDIDATES_FOUND"

	// ── LLM 起因（Validator のリトライ判定に使う） ──
	CodeLLMUnavailable   Code = "LLM_UNAVAILABLE"
	CodeLLMInvalidOutput Code = "LLM_INVALID_OUTPUT"
	// CodeUnknownCandidate は LLM が FactStore に無い candidateId を書いた場合。
	// ハルシネーションを物理的に遮断する Hydrator の防壁がここで作動する。
	CodeUnknownCandidate Code = "UNKNOWN_CANDIDATE"
	CodeTimelineInvalid  Code = "TIMELINE_INVALID"

	// ── その他 ──
	CodeInternal Code = "INTERNAL_ERROR"
)

// httpStatus はドメインエラー ⇔ HTTP ステータスの唯一の対応表。
// controller はこの表だけを参照し、自前で分岐しない。
var httpStatus = map[Code]int{
	CodeValidationFailed: http.StatusBadRequest,
	CodePlanNotFound:     http.StatusNotFound,
	CodeNotFound:         http.StatusNotFound,
	CodePlanExpired:      http.StatusGone,
	CodeRateLimited:      http.StatusTooManyRequests,

	CodeUpstreamTimeout:     http.StatusInternalServerError,
	CodeUpstreamFailed:      http.StatusInternalServerError,
	CodeUpstreamRateLimited: http.StatusInternalServerError,
	CodeNoCandidatesFound:   http.StatusInternalServerError,

	CodeLLMUnavailable:   http.StatusInternalServerError,
	CodeLLMInvalidOutput: http.StatusInternalServerError,
	CodeUnknownCandidate: http.StatusInternalServerError,
	CodeTimelineInvalid:  http.StatusInternalServerError,

	CodeInternal: http.StatusInternalServerError,
}

// defaultMessage はユーザーに見せる日本語メッセージ。
// 上流の生エラー文をそのまま出さないための既定値で、内部情報は cause 側に隠す。
var defaultMessage = map[Code]string{
	CodeValidationFailed:    "検索条件が不正です",
	CodePlanNotFound:        "プランが見つかりません",
	CodeNotFound:            "対象が見つかりません",
	CodePlanExpired:         "プランの有効期限が切れています。もう一度検索してください",
	CodeRateLimited:         "リクエストが多すぎます。しばらく待ってからお試しください",
	CodeUpstreamTimeout:     "外部サービスの応答がありません",
	CodeUpstreamFailed:      "外部サービスの取得に失敗しました",
	CodeUpstreamRateLimited: "外部サービスのレート制限に達しました",
	CodeNoCandidatesFound:   "条件に合う候補が見つかりませんでした",
	CodeLLMUnavailable:      "プランの文章生成が利用できません",
	CodeLLMInvalidOutput:    "プランの生成結果が不正です",
	CodeUnknownCandidate:    "プランの生成結果が不正です",
	CodeTimelineInvalid:     "プランの時系列を組み立てられませんでした",
	CodeInternal:            "サーバ内部でエラーが発生しました",
}

// retryable は「同じ操作をもう一度叩けば直る可能性がある」コード。collector の再試行判定に使う。
var retryable = map[Code]bool{
	CodeUpstreamTimeout:     true,
	CodeUpstreamFailed:      true,
	CodeUpstreamRateLimited: true,
	CodeLLMUnavailable:      true,
}

// repairable は「LLM に組み直させれば直る可能性がある」コード。
// validator.go はこれが真のときだけ 1 回だけ再生成する（設計書 ⑤'）。
var repairable = map[Code]bool{
	CodeLLMInvalidOutput: true,
	CodeUnknownCandidate: true,
	CodeTimelineInvalid:  true,
}

// FieldError は ErrorBody.details の 1 要素。どの入力項目がなぜ弾かれたかを示す。
type FieldError struct {
	Field  string // 例: "event.endsAt"
	Reason string // 例: "過去の日時は指定できません"
}

// Error はアプリケーション全域で受け渡すドメインエラー。
type Error struct {
	Code    Code
	Message string       // ユーザー向け（日本語・内部情報を含めない）
	Details []FieldError // VALIDATION_FAILED のときのみ埋まる
	Op      string       // 発生箇所。ログ専用で API には出さない
	cause   error        // 内部原因。ログ専用で API には出さない
}

// New はコードに対応する既定メッセージでエラーを作る。message が空でなければそちらを優先する。
func New(code Code, message string) *Error {
	if message == "" {
		message = defaultMessage[code]
	}
	return &Error{Code: code, Message: message}
}

// Errorf は書式付きメッセージでエラーを作る。message はユーザーに見えるため内部情報を埋めないこと。
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap は原因エラーを内部に隠したまま、ユーザー向けの既定メッセージを被せる。
func Wrap(cause error, code Code, op string) *Error {
	if cause == nil {
		return nil
	}
	// すでにドメインエラーなら、コードを上書きせず発生箇所だけ足す。
	var e *Error
	if errors.As(cause, &e) {
		wrapped := *e
		if wrapped.Op == "" {
			wrapped.Op = op
		}
		wrapped.cause = cause
		return &wrapped
	}
	return &Error{Code: code, Message: defaultMessage[code], Op: op, cause: cause}
}

// Validation は入力検証エラーを項目単位の理由付きで作る。
func Validation(details ...FieldError) *Error {
	return &Error{
		Code:    CodeValidationFailed,
		Message: defaultMessage[CodeValidationFailed],
		Details: details,
	}
}

func NotFound(op string) *Error    { return New(CodePlanNotFound, "").WithOp(op) }
func Expired(op string) *Error     { return New(CodePlanExpired, "").WithOp(op) }
func RateLimited(op string) *Error { return New(CodeRateLimited, "").WithOp(op) }

// Internal は想定外の失敗を包む。cause はログにのみ出る。
func Internal(cause error, op string) *Error {
	return Wrap(cause, CodeInternal, op)
}

// Upstream は外部 API の失敗を、プロバイダ名を発生箇所に残した形で包む。
func Upstream(cause error, code Code, provider string) *Error {
	return Wrap(cause, code, "upstream:"+provider)
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	s := string(e.Code)
	if e.Op != "" {
		s = e.Op + ": " + s
	}
	if e.cause != nil {
		return s + ": " + e.cause.Error()
	}
	return s + ": " + e.Message
}

// Unwrap により errors.Is / errors.As で原因側（context.DeadlineExceeded 等）まで辿れる。
func (e *Error) Unwrap() error { return e.cause }

// Is はコードが同じ *Error を等価とみなす。errors.Is(err, apperror.New(code, "")) が使える。
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Code == e.Code
}

// HTTPStatus は対応表を引く。未登録のコードは 500 に倒す。
func (e *Error) HTTPStatus() int {
	if e == nil {
		return http.StatusInternalServerError
	}
	if s, ok := httpStatus[e.Code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Retryable は同一操作の再試行で回復しうるかを返す。
func (e *Error) Retryable() bool { return e != nil && retryable[e.Code] }

// Repairable は LLM への組み直し依頼で回復しうるかを返す。
func (e *Error) Repairable() bool { return e != nil && repairable[e.Code] }

func (e *Error) WithOp(op string) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Op = op
	return &c
}

func (e *Error) WithCause(cause error) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.cause = cause
	return &c
}

func (e *Error) WithDetails(details ...FieldError) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Details = append(append([]FieldError(nil), c.Details...), details...)
	return &c
}

// From は任意の error をドメインエラーに正規化する。
// ドメインエラーでなければ内部エラー扱いにし、生のメッセージをユーザーに漏らさない。
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeInternal, Message: defaultMessage[CodeInternal], cause: err}
}

// CodeOf は err のドメインコードを返す。ドメインエラーでなければ INTERNAL_ERROR。
func CodeOf(err error) Code { return From(err).Code }

// HasCode は err の連鎖に指定コードが含まれるかを返す。
func HasCode(err error, code Code) bool {
	var e *Error
	for errors.As(err, &e) {
		if e.Code == code {
			return true
		}
		if e.cause == nil {
			return false
		}
		err = e.cause
	}
	return false
}

// HTTPStatusOf は任意の error に対する応答ステータスを返す。controller の唯一の入口。
func HTTPStatusOf(err error) int { return From(err).HTTPStatus() }
