package plan

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

func newStore() *FactStore {
	return NewFactStore(model.Waypoint{
		Name:     "東京ドーム",
		Location: model.Location{Lat: 35.7056, Lng: 139.7519},
	})
}

func place(providerID, name string) *model.PlaceFact {
	return &model.PlaceFact{Cat: model.CategoryDining, ProviderPlaceID: providerID, Name: name}
}

func TestAddAssignsSequentialIDs(t *testing.T) {
	s := newStore()

	for i, want := range []string{"cand_dining_001", "cand_dining_002", "cand_dining_003"} {
		got, err := s.AddPlace(place(string(rune('a'+i)), "店"))
		if err != nil {
			t.Fatalf("採番に失敗: %v", err)
		}
		if string(got) != want {
			t.Errorf("%d 件目の ID = %q, want %q", i+1, got, want)
		}
	}

	// カテゴリごとに独立した連番になる。
	hotelID, err := s.AddHotel(&model.HotelFact{ProviderHotelID: "123456", Name: "○○ホテル"})
	if err != nil {
		t.Fatalf("採番に失敗: %v", err)
	}
	if string(hotelID) != "cand_lodging_001" {
		t.Errorf("宿泊の ID = %q, want cand_lodging_001", hotelID)
	}

	if s.Count(model.CategoryDining) != 3 || s.Count(model.CategoryLodging) != 1 || s.Len() != 4 {
		t.Errorf("件数が合いません: dining=%d lodging=%d len=%d",
			s.Count(model.CategoryDining), s.Count(model.CategoryLodging), s.Len())
	}
}

func TestAddIsIdempotentPerProviderID(t *testing.T) {
	// ジャンルごとに searchNearby を複数回投げると同じ店が返る。
	// 二重に採番すると LLM に同じ店を 2 通りの ID で見せてしまう。
	s := newStore()
	first, _ := s.AddPlace(place("ChIJsame", "居酒屋 ○○"))
	second, err := s.AddPlace(place("ChIJsame", "居酒屋 ○○"))
	if err != nil {
		t.Fatalf("再登録でエラー: %v", err)
	}
	if first != second {
		t.Errorf("同じ店に別 ID を振りました: %q / %q", first, second)
	}
	if s.Len() != 1 {
		t.Errorf("重複が登録されています: %d 件", s.Len())
	}
}

func TestAddBindsIDToFact(t *testing.T) {
	s := newStore()
	f := place("ChIJxxx", "居酒屋 ○○")
	id, _ := s.AddPlace(f)
	if f.ID != id {
		t.Errorf("採番した ID が fact に書き戻されていません: %q != %q", f.ID, id)
	}
	got, err := s.Place(id)
	if err != nil {
		t.Fatalf("解決に失敗: %v", err)
	}
	if got != f {
		t.Error("登録した fact と別の実体が返りました")
	}
}

func TestResolveRejectsUnknownCandidate(t *testing.T) {
	// LLM が実在しない ID を書いた場合。ここで止まらないと幻覚がそのままユーザーに出る。
	s := newStore()
	s.AddPlace(place("ChIJxxx", "居酒屋 ○○"))

	_, err := s.Resolve("cand_dining_999")
	if err == nil {
		t.Fatal("未知の候補 ID を通しました")
	}
	e := apperror.From(err)
	if e.Code != apperror.CodeUnknownCandidate {
		t.Errorf("Code = %q, want %q", e.Code, apperror.CodeUnknownCandidate)
	}
	// 組み直しで回復しうる、と validator が判断できること。
	if !e.Repairable() {
		t.Error("UNKNOWN_CANDIDATE は組み直し可能であるべきです")
	}
	// 内部の ID がユーザー向けメッセージに出ていないこと。
	if strings.Contains(e.Message, "cand_dining_999") {
		t.Errorf("内部情報がユーザー向けメッセージに漏れています: %q", e.Message)
	}
	if s.Has("cand_dining_999") {
		t.Error("Has が偽陽性を返しました")
	}
}

func TestResolveRejectsCategoryMismatch(t *testing.T) {
	// LLM が dining のステップに宿泊の ID を書いた場合。
	s := newStore()
	hotelID, _ := s.AddHotel(&model.HotelFact{ProviderHotelID: "123456"})

	_, err := s.Place(hotelID)
	if err == nil {
		t.Fatal("種別の食い違いを通しました")
	}
	if got := apperror.From(err).Code; got != apperror.CodeLLMInvalidOutput {
		t.Errorf("Code = %q, want %q", got, apperror.CodeLLMInvalidOutput)
	}

	placeID, _ := s.AddPlace(place("ChIJxxx", "居酒屋"))
	if _, err := s.Hotel(placeID); err == nil {
		t.Error("逆方向の食い違いを通しました")
	}
}

func TestFreezeBlocksLateWrites(t *testing.T) {
	// LLM に提示した集合と Hydrator が照合する集合は一致していなければならない。
	s := newStore()
	s.AddPlace(place("ChIJxxx", "居酒屋"))
	s.Freeze()

	if !s.Frozen() {
		t.Error("Frozen() が偽です")
	}
	if _, err := s.AddPlace(place("ChIJyyy", "ラーメン")); err == nil {
		t.Error("凍結後の追加を許してしまいました")
	}
	// 読み出しは凍結後も通る。
	if _, err := s.Resolve("cand_dining_001"); err != nil {
		t.Errorf("凍結後に解決できません: %v", err)
	}
}

func TestCandidatesKeepScoreOrder(t *testing.T) {
	// 採番順 = scorer が付けたスコア順。LLM への提示順が毎回変わると再現性が無くなる。
	s := newStore()
	names := []string{"1位", "2位", "3位"}
	for i, n := range names {
		s.AddPlace(place(string(rune('a'+i)), n))
	}
	s.AddHotel(&model.HotelFact{ProviderHotelID: "h1", Name: "ホテル"})

	got := s.Places(model.CategoryDining)
	if len(got) != 3 {
		t.Fatalf("件数 = %d, want 3", len(got))
	}
	for i, p := range got {
		if p.Name != names[i] {
			t.Errorf("%d 番目 = %q, want %q", i, p.Name, names[i])
		}
	}
	if len(s.Hotels()) != 1 {
		t.Errorf("宿泊候補 = %d 件, want 1", len(s.Hotels()))
	}
	// カテゴリ違いが混ざらないこと。
	if len(s.Places(model.CategorySpot)) != 0 {
		t.Error("spot に dining が混ざっています")
	}
}

func TestRoutes(t *testing.T) {
	s := newStore()
	id, _ := s.AddPlace(place("ChIJxxx", "居酒屋"))

	r := &model.RouteFact{
		Key:            model.NewRouteKey(model.RouteOriginVenue, string(id), model.TravelWalk),
		DistanceMeters: 650,
		Duration:       8 * time.Minute,
	}
	if err := s.PutRoute(r); err != nil {
		t.Fatalf("経路の保管に失敗: %v", err)
	}

	got, ok := s.RouteFromVenue(id, model.TravelWalk)
	if !ok || got.DistanceMeters != 650 {
		t.Errorf("会場からの経路を引けません: %+v ok=%v", got, ok)
	}
	// 移動手段が違えば別の経路。
	if _, ok := s.RouteFromVenue(id, model.TravelTransit); ok {
		t.Error("TRANSIT の経路が無いのに引けてしまいました")
	}
	// RouteMatrix の粗い値を computeRoutes の詳細値で上書きできる。
	r2 := *r
	r2.Duration = 9 * time.Minute
	r2.EncodedPolyline = "yv{xEuvfsY"
	s.PutRoute(&r2)
	if got, _ := s.RouteFromVenue(id, model.TravelWalk); got.Duration != 9*time.Minute {
		t.Errorf("上書きされていません: %v", got.Duration)
	}
	if s.RouteCount() != 1 {
		t.Errorf("経路数 = %d, want 1", s.RouteCount())
	}
	if err := s.PutRoute(&model.RouteFact{}); err == nil {
		t.Error("鍵の無い経路を通しました")
	}
}

func TestConcurrentAdd(t *testing.T) {
	// collector が 3 つの API を並行に叩くため、書き込みは並行に来る。
	s := newStore()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				s.AddPlace(place(string(rune(i)), "店"))
			} else {
				s.AddHotel(&model.HotelFact{ProviderHotelID: string(rune(i))})
			}
		}(i)
	}
	wg.Wait()

	if s.Len() != 50 {
		t.Errorf("登録数 = %d, want 50", s.Len())
	}
	// 並行採番でも ID が飛んだり重複したりしないこと。
	seen := map[model.CandidateID]bool{}
	for _, f := range append(s.Candidates(model.CategoryDining), s.Candidates(model.CategoryLodging)...) {
		id := f.CandidateID()
		if !id.Valid() {
			t.Errorf("不正な ID: %q", id)
		}
		if seen[id] {
			t.Errorf("ID が重複: %q", id)
		}
		seen[id] = true
	}
}
