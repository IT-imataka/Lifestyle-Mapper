package apperror

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestHTTPStatusMapping(t *testing.T) {
	cases := map[Code]int{
		CodeValidationFailed: http.StatusBadRequest,
		CodePlanNotFound:     http.StatusNotFound,
		CodePlanExpired:      http.StatusGone,
		CodeRateLimited:      http.StatusTooManyRequests,
		CodeInternal:         http.StatusInternalServerError,
		CodeUpstreamTimeout:  http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := New(code, "").HTTPStatus(); got != want {
			t.Errorf("%s: HTTPStatus() = %d, want %d", code, got, want)
		}
	}
	// 表に無いコードは 500 に倒す。
	if got := New(Code("SOMETHING_NEW"), "").HTTPStatus(); got != http.StatusInternalServerError {
		t.Errorf("未登録コード: HTTPStatus() = %d, want 500", got)
	}
}

func TestDefaultMessage(t *testing.T) {
	if got := New(CodePlanNotFound, "").Message; got != "プランが見つかりません" {
		t.Errorf("Message = %q", got)
	}
	if got := New(CodePlanNotFound, "独自の文言").Message; got != "独自の文言" {
		t.Errorf("明示したメッセージが上書きされました: %q", got)
	}
}

func TestWrapKeepsDomainCodeAndHidesCause(t *testing.T) {
	cause := context.DeadlineExceeded
	err := Upstream(cause, CodeUpstreamTimeout, "rakuten_travel")

	if err.Code != CodeUpstreamTimeout {
		t.Errorf("Code = %q", err.Code)
	}
	// ユーザー向けメッセージに内部の生エラーが漏れていないこと。
	if err.Message != defaultMessage[CodeUpstreamTimeout] {
		t.Errorf("Message = %q", err.Message)
	}
	// 原因は errors.Is で辿れること。
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("原因エラーを辿れません")
	}
	// 二重ラップしてもコードは最初のものを保つ。
	again := Wrap(err, CodeInternal, "service/plan")
	if again.Code != CodeUpstreamTimeout {
		t.Errorf("再ラップでコードが上書きされました: %q", again.Code)
	}
	if again.Op != "upstream:rakuten_travel" {
		t.Errorf("Op = %q", again.Op)
	}
}

func TestIsAndHasCode(t *testing.T) {
	err := NotFound("repository/plan")
	if !errors.Is(err, New(CodePlanNotFound, "")) {
		t.Error("同一コードのエラーを errors.Is で判定できません")
	}
	if errors.Is(err, New(CodePlanExpired, "")) {
		t.Error("別コードを一致と判定しました")
	}
	if !HasCode(Wrap(err, CodeInternal, "service"), CodePlanNotFound) {
		t.Error("ラップ後にコードを辿れません")
	}
}

func TestFromNormalizesUnknownError(t *testing.T) {
	raw := errors.New("pq: connection refused on 10.0.1.5:5432")
	e := From(raw)
	if e.Code != CodeInternal {
		t.Errorf("Code = %q, want %q", e.Code, CodeInternal)
	}
	// DB のホスト名などがユーザー向けメッセージに漏れないこと。
	if e.Message != defaultMessage[CodeInternal] {
		t.Errorf("生のエラー文が露出しています: %q", e.Message)
	}
	if !errors.Is(e, raw) {
		t.Error("原因を辿れません")
	}
	if From(nil) != nil {
		t.Error("nil は nil のままであるべきです")
	}
	if got := HTTPStatusOf(raw); got != http.StatusInternalServerError {
		t.Errorf("HTTPStatusOf() = %d", got)
	}
}

func TestRetryableAndRepairable(t *testing.T) {
	if !New(CodeUpstreamTimeout, "").Retryable() {
		t.Error("上流タイムアウトは再試行可能であるべきです")
	}
	if New(CodeValidationFailed, "").Retryable() {
		t.Error("入力不正を再試行可能と判定しました")
	}
	// LLM の幻覚は組み直しで回復しうる（validator が 1 回だけ再生成する）。
	if !New(CodeUnknownCandidate, "").Repairable() {
		t.Error("未知の候補 ID は組み直し可能であるべきです")
	}
	if New(CodeUpstreamTimeout, "").Repairable() {
		t.Error("上流タイムアウトを組み直し対象と判定しました")
	}
}

func TestValidationDetails(t *testing.T) {
	err := Validation(FieldError{Field: "event.endsAt", Reason: "過去の日時は指定できません"})
	if err.HTTPStatus() != http.StatusBadRequest {
		t.Errorf("HTTPStatus() = %d", err.HTTPStatus())
	}
	more := err.WithDetails(FieldError{Field: "party.adults", Reason: "1〜20 人で指定してください"})
	if len(more.Details) != 2 {
		t.Errorf("details = %d 件, want 2", len(more.Details))
	}
	// WithDetails は元のエラーを書き換えない。
	if len(err.Details) != 1 {
		t.Errorf("元のエラーが変更されました: %d 件", len(err.Details))
	}
}
