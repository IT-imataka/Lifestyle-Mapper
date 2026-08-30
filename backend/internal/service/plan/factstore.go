// Package plan はプラン生成パイプライン（収集 → 正規化 → 採番 → 作文 → 再水和 → 検証）を実装する。
package plan

import (
	"fmt"
	"sync"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// maxSeqPerCategory は CandidateID の連番が 3 桁であることに由来する上限。
// 実際には scorer が options.maxCandidatesPerCategory（最大 20）まで絞るため到達しない。
const maxSeqPerCategory = 999

// FactStore は candidateId と検証済みの事実の唯一の写像を持つ。
//
// この型が設計の要になっている理由:
//   - LLM に見せてよいのは candidateId だけで、店名・価格・URL の正解はここにしか無い。
//   - Hydrator はここに存在しない ID を含む LLM 出力を棄却する。
//     **ハルシネーションはこの照合で物理的に遮断される**。
//   - Collector が 3 つの API を並行に叩くため、書き込みは並行に来る。内部で同期する。
//
// 寿命はプラン 1 件ぶん。リクエストごとに作り、生成が終われば捨てる。
type FactStore struct {
	mu sync.RWMutex

	venue model.Waypoint

	seq   map[model.CandidateCategory]int
	facts map[model.CandidateID]model.Fact
	// order は採番順（＝ scorer が付けたスコア順）。LLM への提示順を安定させる。
	order []model.CandidateID
	// dedupe は提供元 ID による重複登録の抑止キー。
	// ジャンルごとに searchNearby を複数回投げると同じ店が返るため必要。
	dedupe map[string]model.CandidateID

	routes map[model.RouteKey]*model.RouteFact

	// frozen が真なら以降の書き込みを拒否する。
	// LLM に提示した候補集合と、Hydrator が照合する集合が食い違わないことを保証する。
	frozen bool
}

func NewFactStore(venue model.Waypoint) *FactStore {
	return &FactStore{
		venue:  venue,
		seq:    make(map[model.CandidateCategory]int),
		facts:  make(map[model.CandidateID]model.Fact),
		dedupe: make(map[string]model.CandidateID),
		routes: make(map[model.RouteKey]*model.RouteFact),
	}
}

// Venue は経路の起点となる会場を返す。
func (s *FactStore) Venue() model.Waypoint { return s.venue }

// ── 登録（採番） ──────────────────────────────────────

// AddPlace は飲食店・スポットを登録して candidateId を採番する。
// 同じ providerPlaceId が既にあれば採番せず既存の ID を返す（重複提示はトークンの無駄）。
func (s *FactStore) AddPlace(f *model.PlaceFact) (model.CandidateID, error) {
	if f == nil {
		return "", apperror.Internal(fmt.Errorf("place fact が nil です"), "factstore.AddPlace")
	}
	if f.Cat != model.CategoryDining && f.Cat != model.CategorySpot {
		return "", apperror.Internal(
			fmt.Errorf("place fact の種別が不正です: %q", f.Cat), "factstore.AddPlace")
	}
	return s.add(f.Cat, "places:"+f.ProviderPlaceID, func(id model.CandidateID) model.Fact {
		f.ID = id
		return f
	})
}

// AddHotel は宿泊候補を登録して candidateId を採番する。
func (s *FactStore) AddHotel(f *model.HotelFact) (model.CandidateID, error) {
	if f == nil {
		return "", apperror.Internal(fmt.Errorf("hotel fact が nil です"), "factstore.AddHotel")
	}
	return s.add(model.CategoryLodging, "rakuten:"+f.ProviderHotelID, func(id model.CandidateID) model.Fact {
		f.ID = id
		return f
	})
}

func (s *FactStore) add(cat model.CandidateCategory, dedupeKey string, bind func(model.CandidateID) model.Fact) (model.CandidateID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.frozen {
		return "", apperror.Internal(
			fmt.Errorf("凍結後に候補を追加しようとしました: %s", dedupeKey), "factstore.add")
	}
	if id, ok := s.dedupe[dedupeKey]; ok {
		return id, nil
	}

	next := s.seq[cat] + 1
	if next > maxSeqPerCategory {
		return "", apperror.Internal(
			fmt.Errorf("候補数が上限（%d 件）を超えました: %s", maxSeqPerCategory, cat), "factstore.add")
	}
	id, err := model.NewCandidateID(cat, next)
	if err != nil {
		return "", apperror.Internal(err, "factstore.add")
	}

	s.seq[cat] = next
	s.facts[id] = bind(id)
	s.order = append(s.order, id)
	s.dedupe[dedupeKey] = id
	return id, nil
}

// Freeze は候補集合を確定する。以降 Add* は失敗する。
// Composer が LLM を呼ぶ直前に必ず呼ぶこと。
func (s *FactStore) Freeze() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = true
}

func (s *FactStore) Frozen() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.frozen
}

// ── 照合（Hydrator の防壁） ───────────────────────────

// Resolve は candidateId から事実を引く。
//
// **LLM の出力を信じてよいかを決める唯一の関門**。存在しない ID には
// UNKNOWN_CANDIDATE を返す。このコードは Repairable なので、
// validator が 1 回だけ LLM に組み直させる判断ができる。
func (s *FactStore) Resolve(id model.CandidateID) (model.Fact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	f, ok := s.facts[id]
	if !ok {
		return nil, apperror.Errorf(apperror.CodeUnknownCandidate,
			"提示していない候補が指定されました").
			WithOp("factstore.Resolve").
			WithCause(fmt.Errorf("未知の candidateId: %q", id))
	}
	return f, nil
}

// Place は飲食店・スポットとして解決する。種別が違えば LLM_INVALID_OUTPUT を返す。
func (s *FactStore) Place(id model.CandidateID) (*model.PlaceFact, error) {
	f, err := s.Resolve(id)
	if err != nil {
		return nil, err
	}
	p, ok := f.(*model.PlaceFact)
	if !ok {
		return nil, categoryMismatch(id, model.CategoryDining, f.Category())
	}
	return p, nil
}

// Hotel は宿泊候補として解決する。
func (s *FactStore) Hotel(id model.CandidateID) (*model.HotelFact, error) {
	f, err := s.Resolve(id)
	if err != nil {
		return nil, err
	}
	h, ok := f.(*model.HotelFact)
	if !ok {
		return nil, categoryMismatch(id, model.CategoryLodging, f.Category())
	}
	return h, nil
}

func categoryMismatch(id model.CandidateID, want, got model.CandidateCategory) error {
	return apperror.Errorf(apperror.CodeLLMInvalidOutput,
		"候補の種別が合いません").
		WithOp("factstore.resolve").
		WithCause(fmt.Errorf("%s: %s を期待しましたが %s でした", id, want, got))
}

// Has は ID の存在だけを確かめる。
func (s *FactStore) Has(id model.CandidateID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.facts[id]
	return ok
}

// ── 取り出し（プロンプト構築・代替候補） ──────────────

// Candidates は指定カテゴリの候補を採番順に返す。
// LLM への提示順とフロントの AlternativePicker の並び順はこれに従う。
func (s *FactStore) Candidates(cat model.CandidateCategory) []model.Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]model.Fact, 0, len(s.order))
	for _, id := range s.order {
		if f := s.facts[id]; f != nil && f.Category() == cat {
			out = append(out, f)
		}
	}
	return out
}

// Places は飲食店・スポットを採番順に返す。
func (s *FactStore) Places(cat model.CandidateCategory) []*model.PlaceFact {
	out := make([]*model.PlaceFact, 0)
	for _, f := range s.Candidates(cat) {
		if p, ok := f.(*model.PlaceFact); ok {
			out = append(out, p)
		}
	}
	return out
}

// Hotels は宿泊候補を採番順に返す。
func (s *FactStore) Hotels() []*model.HotelFact {
	out := make([]*model.HotelFact, 0)
	for _, f := range s.Candidates(model.CategoryLodging) {
		if h, ok := f.(*model.HotelFact); ok {
			out = append(out, h)
		}
	}
	return out
}

// Count はカテゴリごとの候補数を返す。
func (s *FactStore) Count(cat model.CandidateCategory) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.seq[cat]
}

// Len は全候補数を返す。0 なら LLM を呼ぶ意味がない。
func (s *FactStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.facts)
}

// ── 経路 ──────────────────────────────────────────────

// PutRoute は実測経路を保管する。同じ鍵の再登録は上書きする
// （RouteMatrix の粗い値を computeRoutes の詳細値で置き換えるため）。
func (s *FactStore) PutRoute(r *model.RouteFact) error {
	if r == nil {
		return apperror.Internal(fmt.Errorf("route fact が nil です"), "factstore.PutRoute")
	}
	if r.Key.From == "" || r.Key.To == "" || !r.Key.Mode.Valid() {
		return apperror.Internal(fmt.Errorf("経路の鍵が不正です: %+v", r.Key), "factstore.PutRoute")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[r.Key] = r
	return nil
}

// Route は鍵で経路を引く。
func (s *FactStore) Route(key model.RouteKey) (*model.RouteFact, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.routes[key]
	return r, ok
}

// RouteFromVenue は会場から候補までの経路を引く。scorer の徒歩圏判定で使う。
func (s *FactStore) RouteFromVenue(to model.CandidateID, mode model.TravelMode) (*model.RouteFact, bool) {
	return s.Route(model.NewRouteKey(model.RouteOriginVenue, string(to), mode))
}

// RouteBetween は候補どうしの経路を引く。timecalc が移動時間を積むときに使う。
func (s *FactStore) RouteBetween(from, to model.CandidateID, mode model.TravelMode) (*model.RouteFact, bool) {
	return s.Route(model.NewRouteKey(string(from), string(to), mode))
}

// RouteCount は保管済みの経路数を返す。
func (s *FactStore) RouteCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.routes)
}
