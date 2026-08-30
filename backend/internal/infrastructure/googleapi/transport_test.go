package googleapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
)

type echoBody struct {
	Message string `json:"message"`
}

// captured は 1 リクエストぶんの受信内容。
type captured struct {
	method    string
	apiKey    string
	fieldMask string
	mask      bool
	mediaType string
	body      []byte
}

// newServer は応答を固定したサーバと、受け取った 1 件目の内容を返す。
func newServer(t *testing.T, status int, response string) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.apiKey = r.Header.Get("X-Goog-Api-Key")
		got.fieldMask = r.Header.Get("X-Goog-FieldMask")
		_, got.mask = r.Header["X-Goog-Fieldmask"]
		got.mediaType = r.Header.Get("Content-Type")
		got.body, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestPostJSONSendsAuthAndFieldMask(t *testing.T) {
	// Places / Routes は FieldMask 未指定だと 400 になる。
	// ここを落とすと全リクエストが失敗する。
	srv, got := newServer(t, http.StatusOK, `{"message":"ok"}`)
	c := New("test-key", "google_places", time.Second, nil)

	var out echoBody
	if err := c.PostJSON(context.Background(), srv.URL, "places.id,places.location",
		map[string]string{"q": "居酒屋"}, &out); err != nil {
		t.Fatal(err)
	}

	if got.method != http.MethodPost {
		t.Errorf("メソッド = %q, want POST", got.method)
	}
	if got.apiKey != "test-key" {
		t.Errorf("API キーヘッダ = %q", got.apiKey)
	}
	if got.fieldMask != "places.id,places.location" {
		t.Errorf("FieldMask = %q", got.fieldMask)
	}
	if !strings.HasPrefix(got.mediaType, "application/json") {
		t.Errorf("Content-Type = %q", got.mediaType)
	}
	if string(got.body) != `{"q":"居酒屋"}` {
		t.Errorf("本文 = %s", got.body)
	}
	if out.Message != "ok" {
		t.Errorf("応答 = %+v", out)
	}
}

func TestPostJSONOmitsEmptyFieldMask(t *testing.T) {
	// FieldMask を持たない API に空ヘッダを送ると弾かれる。付けないこと自体が仕様。
	srv, got := newServer(t, http.StatusOK, `{"message":"ok"}`)
	c := New("test-key", "google_routes", time.Second, nil)

	var out echoBody
	if err := c.PostJSON(context.Background(), srv.URL, "", map[string]string{}, &out); err != nil {
		t.Fatal(err)
	}
	if got.mask {
		t.Error("空の FieldMask ヘッダを送っています")
	}
}

func TestPostJSONClassifiesStatusCodes(t *testing.T) {
	// レート制限だけは候補数を減らして出直せば回復する性質の失敗で、
	// 鍵の誤りや仕様違反とは対処がまったく違う。
	tests := []struct {
		name   string
		status int
		want   apperror.Code
	}{
		{"400 は回復不能な失敗", http.StatusBadRequest, apperror.CodeUpstreamFailed},
		{"403 も失敗", http.StatusForbidden, apperror.CodeUpstreamFailed},
		{"429 はレート制限", http.StatusTooManyRequests, apperror.CodeUpstreamRateLimited},
		{"504 はタイムアウト", http.StatusGatewayTimeout, apperror.CodeUpstreamTimeout},
		{"408 もタイムアウト", http.StatusRequestTimeout, apperror.CodeUpstreamTimeout},
		{"500 は失敗", http.StatusInternalServerError, apperror.CodeUpstreamFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newServer(t, tt.status, `{"error":{"status":"X","message":"y"}}`)
			c := New("k", "google_places", time.Second, nil)

			var out echoBody
			err := c.PostJSON(context.Background(), srv.URL, "m", map[string]string{}, &out)
			if err == nil {
				t.Fatal("エラーになりません")
			}
			var e *apperror.Error
			if !errors.As(err, &e) || e.Code != tt.want {
				t.Fatalf("コード = %v, want %v", err, tt.want)
			}
			// 発生箇所にプロバイダ名が残る。障害時にどの提供元かを即座に判別するため。
			if e.Op != "upstream:google_places" {
				t.Errorf("発生箇所 = %q", e.Op)
			}
		})
	}
}

func TestPostJSONSummarizesErrorBody(t *testing.T) {
	srv, _ := newServer(t, http.StatusBadRequest,
		`{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"FieldMask is required"}}`)
	c := New("k", "google_places", time.Second, nil)

	var out echoBody
	err := c.PostJSON(context.Background(), srv.URL, "m", map[string]string{}, &out)
	if err == nil {
		t.Fatal("エラーになりません")
	}
	// 原因はログ専用だが、そこに至れないと FieldMask の誤りが調べられない。
	if !strings.Contains(err.Error(), "INVALID_ARGUMENT: FieldMask is required") {
		t.Errorf("原因が残っていません: %v", err)
	}
	// ユーザー向けメッセージには内部情報を出さない。
	var e *apperror.Error
	errors.As(err, &e)
	if strings.Contains(e.Message, "FieldMask") {
		t.Errorf("ユーザー向けメッセージに内部情報が漏れています: %q", e.Message)
	}
}

func TestPostJSONFallsBackToRawBodyWhenUnparsable(t *testing.T) {
	// 中間装置が HTML を返すことがある。読めない本文でも原因の手掛かりを残す。
	srv, _ := newServer(t, http.StatusBadGateway, "<html>502 Bad Gateway</html>")
	c := New("k", "google_routes", time.Second, nil)

	var out echoBody
	err := c.PostJSON(context.Background(), srv.URL, "m", map[string]string{}, &out)
	if err == nil || !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Errorf("生の本文が残っていません: %v", err)
	}
}

func TestDescribeTruncatesLongBody(t *testing.T) {
	long := strings.Repeat("あ", 300)
	got := describe([]byte(long))
	if !strings.HasSuffix(got, "…") {
		t.Errorf("長い本文が切られていません: %q", got[:30])
	}
	if len(got) > 210 {
		t.Errorf("要約が長すぎます: %d バイト", len(got))
	}
}

func TestPostJSONRejectsUnreadableSuccessBody(t *testing.T) {
	// 200 なのに読めない＝提供元の仕様変更か中間装置の介入。再試行では直らない。
	srv, _ := newServer(t, http.StatusOK, `{"message":`)
	c := New("k", "google_places", time.Second, nil)

	var out echoBody
	err := c.PostJSON(context.Background(), srv.URL, "m", map[string]string{}, &out)
	var e *apperror.Error
	if !errors.As(err, &e) || e.Code != apperror.CodeUpstreamFailed {
		t.Fatalf("コード = %v, want UPSTREAM_FAILED", err)
	}
	if !strings.Contains(err.Error(), "応答 JSON を解釈できません") {
		t.Errorf("原因が残っていません: %v", err)
	}
}

// newBlockingServer は応答を返さないサーバを立てる。
//
// ハンドラを止めたまま Close を呼ぶと、接続が閉じるまでテストが止まる。
// 解除用のチャネルを噛ませて、Close の前に必ずハンドラを返させる。
func newBlockingServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	return srv, sync.OnceFunc(func() {
		close(done)
		srv.Close()
	})
}

func TestPostJSONTreatsTimeoutAsUpstreamTimeout(t *testing.T) {
	// collector が「待ち切れなかったので部分失敗として切り捨てる」判断に使う。
	srv, release := newBlockingServer(t)
	defer release()

	c := New("k", "google_places", 50*time.Millisecond, nil)
	var out echoBody
	err := c.PostJSON(context.Background(), srv.URL, "m", map[string]string{}, &out)

	var e *apperror.Error
	if !errors.As(err, &e) || e.Code != apperror.CodeUpstreamTimeout {
		t.Fatalf("コード = %v, want UPSTREAM_TIMEOUT", err)
	}
}

func TestPostJSONTreatsCancellationAsUpstreamTimeout(t *testing.T) {
	srv, release := newBlockingServer(t)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	c := New("k", "google_places", 5*time.Second, nil)
	var out echoBody
	err := c.PostJSON(ctx, srv.URL, "m", map[string]string{}, &out)

	var e *apperror.Error
	if !errors.As(err, &e) || e.Code != apperror.CodeUpstreamTimeout {
		t.Fatalf("コード = %v, want UPSTREAM_TIMEOUT", err)
	}
	// 原因まで辿れる。ログで「打ち切ったのか落ちたのか」を区別するため。
	if !errors.Is(err, context.Canceled) {
		t.Error("原因のキャンセルまで辿れません")
	}
}

func TestPostJSONRejectsUnmarshalableBody(t *testing.T) {
	// 送れない本文はこちら側の不具合。上流のせいにしない。
	c := New("k", "google_places", time.Second, nil)
	err := c.PostJSON(context.Background(), "http://example.invalid", "m",
		map[string]any{"bad": make(chan int)}, &echoBody{})

	var e *apperror.Error
	if !errors.As(err, &e) || e.Code != apperror.CodeInternal {
		t.Fatalf("コード = %v, want INTERNAL_ERROR", err)
	}
}

func TestNewAppliesDefaultTimeout(t *testing.T) {
	c := New("k", "p", 0, nil)
	if c.http.Timeout != 5*time.Second {
		t.Errorf("既定のタイムアウト = %v, want 5s", c.http.Timeout)
	}
	// 渡された http.Client は複製する。呼び出し側の設定を書き換えない。
	shared := &http.Client{Timeout: time.Minute}
	_ = New("k", "p", time.Second, shared)
	if shared.Timeout != time.Minute {
		t.Errorf("共有された http.Client を書き換えています: %v", shared.Timeout)
	}
}

func TestPostJSONDecodesTopLevelArray(t *testing.T) {
	// computeRouteMatrix はオブジェクトに包まれない配列を返す。
	srv, _ := newServer(t, http.StatusOK, `[{"message":"a"},{"message":"b"}]`)
	c := New("k", "google_routes", time.Second, nil)

	var out []echoBody
	if err := c.PostJSON(context.Background(), srv.URL, "m", map[string]string{}, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Message != "b" {
		b, _ := json.Marshal(out)
		t.Errorf("応答 = %s", b)
	}
}
