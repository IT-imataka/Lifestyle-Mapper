package plan

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/llm"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// Hydrator は LLM の下書き（参照 ID + 文章）に FactStore の事実を差し戻す。
//
// **ここが幻覚の遮断点**。LLM が書いた candidateId は必ず FactStore で照合され、
// 存在しなければ棄却される。逆に言えば、店名・住所・価格・URL・営業時間が
// プランに載る経路はここ 1 箇所しかない。
//
// 時刻は付けない。絶対時刻の確定は timecalc の責務で、ここは「何を・どの順で・
// どれだけ滞在するか」までを確定させる。
type Hydrator struct {
	store *FactStore
	links LinkBuilder
}

func NewHydrator(store *FactStore, links LinkBuilder) *Hydrator {
	if links == nil {
		links = DefaultLinkBuilder{}
	}
	return &Hydrator{store: store, links: links}
}

// Hydrated は時刻確定前のプラン。timecalc がこれを受けてタイムラインにする。
type Hydrated struct {
	Steps        []ResolvedStep
	Narrative    *model.PlanNarrative
	Alternatives model.Alternatives
	Warnings     []model.Warning
}

// ResolvedStep は事実が差し戻された 1 ステップ。時刻はまだ持たない。
type ResolvedStep struct {
	Kind model.SegmentType
	// Stay は滞在時間。move は 0（実移動時間は Route.Duration を使う）、
	// lodging も 0（チェックアウトは翌朝の実時刻から決まる）。
	Stay      time.Duration
	Narrative *model.SegmentNarrative

	Buffer  *model.BufferDetail
	Route   *model.RouteFact
	Place   *model.PlaceFact
	Lodging *model.LodgingChoice

	Links []model.Link
}

// Hydrate は下書きを解決する。
//
// 返すエラーはいずれも Repairable なので、validator は「LLM に 1 回だけ組み直させる」
// 判断ができる。候補に経路が無い・満室しか無いといった理由でも、組み直せば LLM が
// 別の候補を選び直せる見込みがあるため。
func (h *Hydrator) Hydrate(draft *llm.PlanDraft, cond *model.SearchCondition) (*Hydrated, error) {
	if draft == nil {
		return nil, apperror.Internal(fmt.Errorf("下書きが nil です"), "hydrator.Hydrate")
	}
	// 構造の検査は composer 側でも通しているが、ここは単独で正しさを保証する。
	if err := draft.Validate(model.LLMAnthropic); err != nil {
		return nil, err
	}

	ordered := draft.StepsInOrder()
	out := &Hydrated{
		Steps:     make([]ResolvedStep, 0, len(ordered)),
		Narrative: draft.Narrative(),
	}

	// 現在地。move の起点と buffer の場所名に使う。
	here := locationRef{key: model.RouteOriginVenue, name: h.store.Venue().Name}
	usedBuffer := false

	for i, s := range ordered {
		step := ResolvedStep{Kind: s.Kind, Narrative: s.Narrative()}

		switch s.Kind {
		case model.SegmentBuffer:
			// 終演直後の 1 つ目だけが規制退場・物販待ちの待機。以降は単なる余白。
			kind := model.BufferSpareTime
			if !usedBuffer && here.key == model.RouteOriginVenue {
				kind = model.BufferExitCongestion
			}
			usedBuffer = true
			step.Buffer = &model.BufferDetail{Kind: kind, AtPlaceName: here.name}
			step.Stay = time.Duration(s.StayMinutes) * time.Minute

		case model.SegmentMove:
			// 行き先は「次に現れる、場所を持つステップ」。move → buffer → dining のように
			// 待機を挟む組み方もあるため、直後の 1 件だけを見ては足りない。
			dest, ok := nextPlacedStep(ordered, i)
			if !ok {
				return nil, repairable(apperror.CodeTimelineInvalid,
					fmt.Errorf("order=%d: move の行き先となるステップがありません", s.Order))
			}
			to, err := h.locationOf(dest.Candidate())
			if err != nil {
				return nil, err
			}
			route, err := h.resolveRoute(here, to, cond)
			if err != nil {
				return nil, err
			}
			step.Route = route
			// 移動時間は Routes API の実測値。LLM の提案値は使わない。
			step.Stay = 0
			// 移動した先が現在地になる。move の直後に待機が来ても場所名を取り違えない。
			here = to

		case model.SegmentDining:
			place, err := h.store.Place(s.Candidate())
			if err != nil {
				return nil, err
			}
			step.Place = place
			step.Stay = time.Duration(s.StayMinutes) * time.Minute
			if step.Stay <= 0 {
				step.Stay = cond.Dining.DesiredStay
			}
			step.Links = h.links.DiningLinks(place)
			here = locationRef{key: string(place.ID), name: place.Name}

		case model.SegmentLodging:
			hotel, err := h.store.Hotel(s.Candidate())
			if err != nil {
				return nil, err
			}
			roomPlan := hotel.CheapestPlan()
			if roomPlan == nil {
				// 収集時点から満室に変わった場合。別の宿を選び直せば回復しうる。
				return nil, repairable(apperror.CodeTimelineInvalid,
					fmt.Errorf("%s: 予約可能なプランがありません", hotel.ID))
			}
			step.Lodging = &model.LodgingChoice{Hotel: hotel, Plan: roomPlan}
			step.Stay = 0 // チェックアウトは翌朝の実時刻から決まる
			step.Links = h.links.LodgingLinks(hotel, roomPlan)
			if w, ok := vacancyWarning(hotel, roomPlan); ok {
				out.Warnings = append(out.Warnings, w)
			}
			here = locationRef{key: string(hotel.ID), name: hotel.Name}

		default:
			return nil, repairable(apperror.CodeLLMInvalidOutput,
				fmt.Errorf("order=%d: 未知のステップ種別です: %q", s.Order, s.Kind))
		}

		// 収益の出口は必ずこの形を通る。壊れたリンクを世に出さない。
		for _, l := range step.Links {
			if err := l.Validate(); err != nil {
				return nil, apperror.Internal(err, "hydrator.Hydrate")
			}
		}
		out.Steps = append(out.Steps, step)
	}

	out.Alternatives = h.alternatives(draft)
	return out, nil
}

// locationRef は「今どこにいるか」。key は経路の鍵に、name は表示に使う。
type locationRef struct {
	key  string
	name string
}

// nextPlacedStep は i より後で最初に場所を持つステップを返す。
func nextPlacedStep(steps []llm.DraftStep, i int) (llm.DraftStep, bool) {
	for _, s := range steps[i+1:] {
		if s.Candidate() != "" {
			return s, true
		}
	}
	return llm.DraftStep{}, false
}

// resolveRoute は現在地から目的地への実測経路を FactStore から引く。
//
// 経路を「作らない」のがこの関数の要点。直線距離からの推定で埋めると、
// 事実と推定が混ざったまま UI に出てしまう。無ければ無いとして失敗させる。
func (h *Hydrator) resolveRoute(from, to locationRef, cond *model.SearchCondition) (*model.RouteFact, error) {
	var candidates []*model.RouteFact
	for _, mode := range cond.Options.TransportModes {
		if r, ok := h.store.Route(model.NewRouteKey(from.key, to.key, mode)); ok {
			candidates = append(candidates, r)
		}
	}
	best := pickRoute(candidates, cond.Situation.MaxWalk)
	if best == nil {
		return nil, repairable(apperror.CodeTimelineInvalid,
			fmt.Errorf("%s → %s(%s) の経路が取得できていません", from.name, to.name, to.key))
	}
	return best, nil
}

// pickRoute は取得済みの経路から採用する 1 本を選ぶ。
//
// 徒歩で上限時間内に着くなら徒歩を選ぶ。乗り換えの手間より歩くほうが速く、
// 遠征では「乗り換えなし」自体が価値になる。それ以外は最速の手段を採る。
//
// **どの手段を採るかの規則はここ 1 箇所**。プロンプトに載せる経路と実際に組む経路が
// 食い違うと、LLM が見た前提と結果がずれる。
func pickRoute(candidates []*model.RouteFact, maxWalk time.Duration) *model.RouteFact {
	var best *model.RouteFact
	for _, r := range candidates {
		if r.Key.Mode == model.TravelWalk && r.Duration <= maxWalk {
			return r
		}
		if best == nil || r.Duration < best.Duration {
			best = r
		}
	}
	return best
}

// locationOf は候補 ID を現在地として扱える形に解決する。
// ここでも FactStore を通すので、実在しない ID は移動の解決前に弾かれる。
func (h *Hydrator) locationOf(id model.CandidateID) (locationRef, error) {
	f, err := h.store.Resolve(id)
	if err != nil {
		return locationRef{}, err
	}
	return locationRef{key: string(id), name: f.DisplayName()}, nil
}

// alternatives は採用しなかった候補を差し替え用に並べる。
//
// LLM が理由を書いた候補（unusedCandidateNotes）は理由付きで、それ以外も
// 理由なしで載せる。「なぜ他の店ではないのか」が UI の説得力になる一方、
// 理由が無いからといって差し替えの選択肢から外す必要はない。
func (h *Hydrator) alternatives(draft *llm.PlanDraft) model.Alternatives {
	reasons := make(map[model.CandidateID]string, len(draft.UnusedCandidateNotes))
	for _, n := range draft.UnusedCandidateNotes {
		reasons[model.CandidateID(strings.TrimSpace(n.CandidateID))] = strings.TrimSpace(n.Reason)
	}
	used := make(map[model.CandidateID]bool)
	for _, id := range draft.ReferencedCandidates() {
		used[id] = true
	}
	return h.alternativesExcept(used, reasons)
}

// alternativesExcept は採用済みを除いた候補を並べる。
// 理由は LLM がある場合のみ付くので、ルールベースの素組みからも同じ経路で呼べる。
func (h *Hydrator) alternativesExcept(used map[model.CandidateID]bool, reasons map[model.CandidateID]string) model.Alternatives {
	alt := model.Alternatives{
		Dining:  []model.DiningAlternative{},
		Lodging: []model.LodgingAlternative{},
	}
	for _, p := range h.store.Places(model.CategoryDining) {
		if used[p.ID] {
			continue
		}
		alt.Dining = append(alt.Dining, model.DiningAlternative{
			Place:          p,
			ExcludedReason: reasons[p.ID],
			Links:          h.links.DiningLinks(p),
		})
	}
	for _, hotel := range h.store.Hotels() {
		if used[hotel.ID] {
			continue
		}
		roomPlan := hotel.CheapestPlan()
		if roomPlan == nil {
			continue // 満室の宿は差し替え候補にならない
		}
		alt.Lodging = append(alt.Lodging, model.LodgingAlternative{
			Choice:         model.LodgingChoice{Hotel: hotel, Plan: roomPlan},
			ExcludedReason: reasons[hotel.ID],
			Links:          h.links.LodgingLinks(hotel, roomPlan),
		})
	}
	return alt
}

// vacancyWarning は残室が少ない宿に注意書きを付ける。
// 「クリックしたら満室だった」が最も痛い離脱なので、先に伝える。
func vacancyWarning(hotel *model.HotelFact, p *model.HotelPlanFact) (model.Warning, bool) {
	switch {
	case p.RemainingRooms != nil && *p.RemainingRooms <= 3:
		return model.NewWarning(model.WarnVacancyLow, model.SeverityInfo, "",
			fmt.Sprintf("%s の残室は%d室です。お早めのご予約をおすすめします。", hotel.Name, *p.RemainingRooms)), true
	case p.VacancyStatus == model.VacancyFewLeft:
		return model.NewWarning(model.WarnVacancyLow, model.SeverityInfo, "",
			fmt.Sprintf("%s は残りわずかです。お早めのご予約をおすすめします。", hotel.Name)), true
	}
	return model.Warning{}, false
}

func repairable(code apperror.Code, cause error) *apperror.Error {
	return apperror.Wrap(cause, code, "hydrator")
}

// ── リンク生成 ────────────────────────────────────────

// LinkBuilder はセグメントに添えるリンクを組み立てる。
//
// アフィリエイト URL の組み立ては提供元ごとに作法が違うため、実装は
// infrastructure 側（楽天・ホットペッパー）から差し込む。Hydrator は
// 「リンクは必ずここを通る」という一点だけを保証する。
type LinkBuilder interface {
	DiningLinks(*model.PlaceFact) []model.Link
	LodgingLinks(*model.HotelFact, *model.HotelPlanFact) []model.Link
}

// DefaultLinkBuilder はアフィリエイトを伴わない最小構成。
// 電話と地図だけを返すので、ASP 未接続でもプランは成立する。
type DefaultLinkBuilder struct{}

var telSanitizer = regexp.MustCompile(`[^0-9+]`)

func (DefaultLinkBuilder) DiningLinks(p *model.PlaceFact) []model.Link {
	links := make([]model.Link, 0, 2)
	if tel := telSanitizer.ReplaceAllString(p.PhoneNumber, ""); tel != "" {
		links = append(links, model.Link{
			Kind: model.LinkTel, Provider: model.LinkProviderDirect,
			Label: "電話で予約", URL: "tel:" + tel,
		})
	}
	if u := mapsURL(p.ProviderPlaceID, p.Loc); u != "" {
		links = append(links, model.Link{
			Kind: model.LinkMap, Provider: model.LinkProviderGoogleMaps,
			Label: "地図で見る", URL: u,
		})
	}
	return links
}

// LodgingLinks は予約リンクと地図リンクを返す。
//
// 予約 URL は **提供元が返した文字列をそのまま使う**。affiliateId 付きで
// 検索していればアフィリエイト URL になっており、自前で組み立て直すと
// ID の 1 文字違いで収益が黙ってゼロになる（設計書 0 章の事故そのもの）。
// URL を持たないプランは予約リンクを出さない。出せない導線を偽装しない。
func (DefaultLinkBuilder) LodgingLinks(h *model.HotelFact, p *model.HotelPlanFact) []model.Link {
	links := make([]model.Link, 0, 2)
	if u := reserveURL(h, p); u != "" {
		links = append(links, model.Link{
			Kind: model.LinkAffiliate, Provider: model.LinkProviderRakutenTravel,
			Label: "楽天トラベルで予約", URL: u, TrackingID: model.NewTrackingID(),
		})
	}
	if u := mapsURL("", h.Loc); u != "" {
		links = append(links, model.Link{
			Kind: model.LinkMap, Provider: model.LinkProviderGoogleMaps,
			Label: "地図で見る", URL: u,
		})
	}
	if len(links) == 0 {
		return nil
	}
	return links
}

// reserveURL はプラン個別の予約ページを優先し、無ければ施設ページに落とす。
// プラン直リンクのほうが離脱が少ないが、施設ページでも予約は成立する。
func reserveURL(h *model.HotelFact, p *model.HotelPlanFact) string {
	if p != nil && p.ReserveURL != "" {
		return p.ReserveURL
	}
	if h != nil {
		return h.InformationURL
	}
	return ""
}

// mapsURL は Google Maps の場所リンクを組み立てる。
// placeId があればそちらが正確なので併記する。
func mapsURL(placeID string, loc model.Location) string {
	if loc.IsZero() {
		return ""
	}
	u := fmt.Sprintf("https://www.google.com/maps/search/?api=1&query=%.6f%%2C%.6f", loc.Lat, loc.Lng)
	if placeID != "" {
		u += "&query_place_id=" + placeID
	}
	return u
}
