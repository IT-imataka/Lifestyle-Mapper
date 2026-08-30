package rakutentravel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/time/rate"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
)

// maxResponseBody は応答本文の上限。30 件 × 複数プランでも数百 KB に収まる。
const maxResponseBody = 8 << 20

// Client は空室検索を叩く。**プロセス内で共有すること**。
// リクエストごとに作ると Limiter も作り直され、レート制限の意味が消える。
type Client struct {
	http          *http.Client
	limiter       *rate.Limiter
	applicationID string
	affiliateID   string
}

// Options は Client の構成。
type Options struct {
	ApplicationID string
	// AffiliateID が空だとアフィリエイト URL にならず、収益がゼロになる。
	// 本番での欠落は config が起動時に弾く。
	AffiliateID string
	Timeout     time.Duration
	// QPS は 1 秒あたりの許容リクエスト数。楽天は概ね 1req/sec。
	QPS        float64
	HTTPClient *http.Client
}

func New(opt Options) *Client {
	if opt.Timeout <= 0 {
		opt.Timeout = 5 * time.Second
	}
	if opt.QPS <= 0 {
		opt.QPS = 1
	}
	hc := opt.HTTPClient
	if hc == nil {
		hc = &http.Client{Transport: http.DefaultTransport}
	}
	c := *hc
	c.Timeout = opt.Timeout

	return &Client{
		http: &c,
		// バースト 1。連続で投げても必ず 1 秒間隔に均される。
		// バーストを許すと最初の数件で制限に当たり、以降が全滅する。
		limiter:       rate.NewLimiter(rate.Limit(opt.QPS), 1),
		applicationID: opt.ApplicationID,
		affiliateID:   opt.AffiliateID,
	}
}

// SearchVacant は空室のあるホテルを返す。
//
// 0 件は (nil, nil)。**満室と障害を同じ形で返さない**のがこの関数の要点で、
// collector は前者なら宿泊なしのプランを組み、後者なら partial として警告を出す。
func (c *Client) SearchVacant(ctx context.Context, q VacantQuery) ([]Hotel, error) {
	if !q.Center.Valid() {
		return nil, apperror.Internal(
			fmt.Errorf("検索の中心座標が不正です: %+v", q.Center), "rakutentravel.SearchVacant")
	}
	if q.CheckIn.IsZero() || q.CheckOut.IsZero() || !q.CheckOut.After(q.CheckIn) {
		return nil, apperror.Internal(
			fmt.Errorf("宿泊日程が不正です: %v → %v", q.CheckIn, q.CheckOut), "rakutentravel.SearchVacant")
	}

	// **並行 fan-out の中でここだけ直列化される**。待たされるぶんは
	// PLAN_COLLECT_TIMEOUT に食い込むので、待ち時間も ctx の管理下に置く。
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, apperror.Upstream(err, apperror.CodeUpstreamTimeout, Provider)
	}

	raw, status, err := c.get(ctx, q)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		err := c.statusError(status, raw)
		if IsNoVacancy(err) {
			// 満室は業務上の 0 件。呼び出し側に障害として伝えない。
			return nil, nil
		}
		return nil, err
	}

	var res vacantResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, apperror.Upstream(
			fmt.Errorf("応答 JSON を解釈できません: %w", err), apperror.CodeUpstreamFailed, Provider)
	}
	return flatten(res), nil
}

func (c *Client) get(ctx context.Context, q VacantQuery) ([]byte, int, error) {
	params := url.Values{}
	for k, v := range q.values(c.applicationID, c.affiliateID) {
		params.Set(k, v)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return nil, 0, apperror.Internal(err, "rakutentravel.get")
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, c.transportError(ctx, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxResponseBody))
		_ = res.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBody))
	if err != nil {
		return nil, res.StatusCode, c.transportError(ctx, err)
	}
	return body, res.StatusCode, nil
}

func (c *Client) transportError(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
		return apperror.Upstream(err, apperror.CodeUpstreamTimeout, Provider)
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return apperror.Upstream(err, apperror.CodeUpstreamTimeout, Provider)
	}
	return apperror.Upstream(err, apperror.CodeUpstreamFailed, Provider)
}

// errNoVacancy は「空室が無い」ことを示す番兵。SearchVacant はこれを
// エラーとして外に出さず 0 件に翻訳する。
var errNoVacancy = errors.New("空室がありません")

// statusError は楽天のエラー本文を分類する。
//
// **404 + not_found は 0 件**であって障害ではない。ここを取り違えると、
// 満室の夜に「サーバエラー」を出すことになる。
func (c *Client) statusError(status int, raw []byte) error {
	var e errorResponse
	_ = json.Unmarshal(raw, &e)

	if e.Error == errNotFound {
		return errNoVacancy
	}

	code := apperror.CodeUpstreamFailed
	switch status {
	case http.StatusTooManyRequests:
		code = apperror.CodeUpstreamRateLimited
	case http.StatusGatewayTimeout, http.StatusRequestTimeout:
		code = apperror.CodeUpstreamTimeout
	}
	detail := e.Error
	if e.ErrorDescription != "" {
		detail += ": " + e.ErrorDescription
	}
	if detail == "" {
		detail = truncate(raw, 200)
	}
	return apperror.Upstream(fmt.Errorf("HTTP %d: %s", status, detail), code, Provider)
}

func truncate(raw []byte, limit int) string {
	if len(raw) > limit {
		return string(raw[:limit]) + "…"
	}
	return string(raw)
}

// flatten は「キーが 1 つだけのオブジェクトの配列」という応答の形を、
// 素直な構造体に均す。この形の面倒さを service 側に見せない。
func flatten(res vacantResponse) []Hotel {
	out := make([]Hotel, 0, len(res.Hotels))
	for _, entry := range res.Hotels {
		var h Hotel
		var found bool
		for _, el := range entry.Hotel {
			switch {
			case el.HotelBasicInfo != nil:
				h.Basic = *el.HotelBasicInfo
				found = true
			case el.HotelRatingInfo != nil:
				h.Rating = el.HotelRatingInfo
			case el.HotelDetailInfo != nil:
				h.Detail = el.HotelDetailInfo
			case len(el.RoomInfo) > 0:
				if room, ok := flattenRoom(el.RoomInfo); ok {
					h.Rooms = append(h.Rooms, room)
				}
			}
		}
		// 基本情報が無ければ施設として成立しない。名前も座標も無い箱を candidate にしない。
		if !found || h.Basic.HotelNo == 0 {
			continue
		}
		out = append(out, h)
	}
	return out
}

func flattenRoom(elements []roomElement) (Room, bool) {
	var r Room
	var found bool
	for _, el := range elements {
		if el.RoomBasicInfo != nil {
			r.Basic = *el.RoomBasicInfo
			found = true
		}
		if el.DailyCharge != nil {
			r.Charges = append(r.Charges, *el.DailyCharge)
		}
	}
	return r, found
}

// IsNoVacancy は「空室が無い」ことを示す内部エラーかを判定する。
// テストと、SearchVacant 自身の翻訳だけが使う。
func IsNoVacancy(err error) bool { return errors.Is(err, errNoVacancy) }
