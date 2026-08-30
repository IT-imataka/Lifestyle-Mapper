package controller

import (
	"context"
	"net/http"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
	"github.com/taka/lifestyle-mapper/backend/internal/view"
)

// PlanCreator はプラン生成を **開始** する。
//
// 3 つの外部 API と LLM で合計 8〜15 秒かかるため、同期で待たせると確実に離脱する。
// 実装は即座に planId を採番して返し、生成そのものは裏で進める。
// 進捗は GET /v1/plans/{planId}/events (SSE) が配る。
type PlanCreator interface {
	Create(ctx context.Context, cond *model.SearchCondition) (model.PlanID, model.PlanStatus, error)
}

// PlanFinder は生成済み・生成中のプランを引く。
// 期限切れ（410）や不在（404）の判定は service 側の責務。
type PlanFinder interface {
	Get(ctx context.Context, id model.PlanID) (*model.Plan, error)
}

// PlanController は POST /v1/plans と GET /v1/plans/{planId}。
type PlanController struct {
	creator PlanCreator
	finder  PlanFinder
	now     func() time.Time
}

func NewPlanController(creator PlanCreator, finder PlanFinder, now func() time.Time) *PlanController {
	if now == nil {
		now = time.Now
	}
	return &PlanController{creator: creator, finder: finder, now: now}
}

// Create は検索条件を受け取り、202 と planId を返す。
func (c *PlanController) Create(w http.ResponseWriter, r *http.Request) {
	var req view.SearchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}

	// 表現 → ドメイン（書式の解釈）→ 既定値 → 検証、の順。
	// 値の妥当性は model が持つ規則が唯一の正で、ここには書かない。
	cond, err := req.ToModel()
	if err != nil {
		writeError(w, r, err)
		return
	}
	cond.ApplyDefaults()
	if err := cond.Validate(c.now()); err != nil {
		writeError(w, r, err)
		return
	}

	id, status, cerr := c.creator.Create(r.Context(), cond)
	if cerr != nil {
		writeError(w, r, cerr)
		return
	}

	// 受理した時点では外部 API をまだ呼んでいないので sources は空。
	writeJSON(w, http.StatusAccepted, view.Envelope[view.PlanCreated]{
		Data: view.NewPlanCreated(id, status),
		Meta: view.NewMeta(requestID(r), nil, nil),
	})
}

// Get はプランを返す。生成中でも 200 を返し、進捗は data.status が示す。
func (c *PlanController) Get(w http.ResponseWriter, r *http.Request) {
	id := model.PlanID(r.PathValue("planId"))

	p, err := c.finder.Get(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// meta.sources / meta.llm は 1 回の生成に紐づく観測情報で、保存済みプランには付かない。
	// 取得経路で空になるのは仕様（openapi でも required は requestId だけ）。
	writeJSON(w, http.StatusOK, view.NewPlanEnvelope(p, view.NewMeta(requestID(r), nil, nil)))
}
