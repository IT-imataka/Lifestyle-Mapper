// Package googleapi は Places / Routes に共通する HTTP 往復だけを担う。
//
// 設計書は googleplaces / googleroutes の 2 パッケージしか挙げていないが、
// 両者の転送層（API キーヘッダ・FieldMask ヘッダ・エラー本文の形・
// 失敗の分類）は文字どおり同一で、写すと「片方だけ直す」事故が必ず起きる。
// **提供元ごとに違うのは URL と本文の形だけ**なので、そこから上だけを
// 各パッケージに残し、下をここに寄せている。
//
// ドメイン知識はここに置かない。この層が知っているのは
// 「Google に JSON を送ると JSON かエラー本文が返る」ことだけ。
package googleapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
)

// maxResponseBody は応答本文の上限。
// searchNearby 20 件・RouteMatrix 20 要素なら数十 KB に収まる。
// 壊れた応答で無限にメモリを食うより、切って失敗扱いにするほうが安全。
const maxResponseBody = 4 << 20

// Client は API キー付きの JSON POST を行う。ゼロ値は使えない。New で作る。
type Client struct {
	http     *http.Client
	apiKey   string
	provider string
}

// New は転送層を作る。provider はエラーの発生箇所に載る識別子（例: "google_places"）。
// timeout が 0 以下なら 5 秒。
func New(apiKey, provider string, timeout time.Duration, hc *http.Client) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if hc == nil {
		// 既定のトランスポートは Keep-Alive を持つ。1 リクエストで会場周辺に
		// 複数回投げるため、接続を使い回せないと TLS ハンドシェイクだけで数百 ms 損する。
		hc = &http.Client{Transport: http.DefaultTransport}
	}
	c := *hc
	c.Timeout = timeout
	return &Client{http: &c, apiKey: apiKey, provider: provider}
}

// PostJSON は body を送り、応答 JSON を out に載せる。
//
// fieldMask が空でなければ X-Goog-FieldMask を付ける。Places / Routes は
// **FieldMask 未指定だと 400 になる**（課金額が青天井にならないための仕様）。
func (c *Client) PostJSON(ctx context.Context, url, fieldMask string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return apperror.Internal(err, "googleapi.PostJSON")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return apperror.Internal(err, "googleapi.PostJSON")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", c.apiKey)
	if fieldMask != "" {
		req.Header.Set("X-Goog-FieldMask", fieldMask)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return c.transportError(ctx, err)
	}
	defer func() {
		// 本文を読み切ってから閉じないと接続が再利用されず、Keep-Alive が無意味になる。
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxResponseBody))
		_ = res.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBody))
	if err != nil {
		return c.transportError(ctx, err)
	}
	if res.StatusCode != http.StatusOK {
		return c.statusError(res.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// 200 なのに読めない＝提供元の仕様変更か中間装置の介入。再試行では直らない。
		return apperror.Upstream(
			fmt.Errorf("応答 JSON を解釈できません: %w", err), apperror.CodeUpstreamFailed, c.provider)
	}
	return nil
}

// transportError は接続レベルの失敗を分類する。
// 期限切れとキャンセルを UPSTREAM_TIMEOUT に寄せるのは、collector が
// 「待ち切れなかったので部分失敗として切り捨てる」判断に使うため。
func (c *Client) transportError(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
		return apperror.Upstream(err, apperror.CodeUpstreamTimeout, c.provider)
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return apperror.Upstream(err, apperror.CodeUpstreamTimeout, c.provider)
	}
	return apperror.Upstream(err, apperror.CodeUpstreamFailed, c.provider)
}

// statusError は HTTP ステータスとエラー本文を分類する。
//
// 429 を独立させるのは、レート制限だけは**候補数を減らして出直せば回復する**
// 性質の失敗で、鍵の誤りや仕様違反とは対処がまったく違うため。
func (c *Client) statusError(status int, raw []byte) error {
	code := apperror.CodeUpstreamFailed
	switch status {
	case http.StatusTooManyRequests:
		code = apperror.CodeUpstreamRateLimited
	case http.StatusGatewayTimeout, http.StatusRequestTimeout:
		code = apperror.CodeUpstreamTimeout
	}
	return apperror.Upstream(
		fmt.Errorf("HTTP %d: %s", status, describe(raw)), code, c.provider)
}

// describe はエラー本文から原因を 1 行に落とす。
// 本文は API キーを含まないが、そのまま垂れ流すとログが読めなくなるので要約する。
func describe(raw []byte) string {
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err == nil && e.Error.Message != "" {
		if e.Error.Status != "" {
			return e.Error.Status + ": " + e.Error.Message
		}
		return e.Error.Message
	}
	const limit = 200
	if len(raw) > limit {
		return string(raw[:limit]) + "…"
	}
	return string(raw)
}
