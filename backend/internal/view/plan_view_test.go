package view

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

var jst = time.FixedZone("JST", 9*60*60)

func at(hour, min int) time.Time { return time.Date(2026, 9, 12, hour, min, 0, 0, jst) }

func ptrF(v float64) *float64 { return &v }
func ptrI(v int) *int         { return &v }

func samplePlan() *model.Plan {
	closesAt := time.Date(2026, 9, 13, 2, 0, 0, 0, jst)
	dining := &model.PlaceFact{
		ID:                        "cand_dining_003",
		Cat:                       model.CategoryDining,
		ProviderPlaceID:           "ChIJxxxxxxxxxxxx",
		Name:                      "居酒屋 ○○ 水道橋店",
		Genres:                    []string{"izakaya"},
		Rating:                    ptrF(4.1),
		UserRatingCount:           ptrI(832),
		PriceLevel:                ptrI(2),
		EstimatedCostPerPersonJPY: ptrI(4000),
		Address:                   "東京都文京区後楽1-1-1",
		Loc:                       model.Location{Lat: 35.7021, Lng: 139.7538},
		PhoneNumber:               "+81-3-1234-5678",
		Hours: model.OpeningHours{
			Known:   true,
			Periods: []model.OpeningPeriod{{Open: at(17, 0), Close: closesAt}},
		},
	}
	checkIn := model.ClockTime(15 * 60)
	deadline := model.ClockTime(26 * 60)
	checkOut := model.ClockTime(10 * 60)
	hotelPlan := model.HotelPlanFact{
		ProviderPlanID:  "9876543",
		PlanName:        "【素泊まり】直前割・チェックイン26時までOK",
		RoomName:        "セミダブル 禁煙",
		TotalPriceJPY:   16200,
		VacancyStatus:   model.VacancyFewLeft,
		RemainingRooms:  ptrI(2),
		CheckInTime:     &checkIn,
		CheckInDeadline: &deadline,
		CheckOutTime:    &checkOut,
		Amenities:       []string{"free_wifi", "large_bath"},
	}
	hotel := &model.HotelFact{
		ID: "cand_hotel_001", ProviderHotelID: "123456", Name: "○○ホテル 後楽園",
		ReviewAverage: ptrF(4.2), ReviewCount: ptrI(1204),
		Loc: model.Location{Lat: 35.7075, Lng: 139.7519}, Plans: []model.HotelPlanFact{hotelPlan},
	}
	route := &model.RouteFact{
		Key:            model.NewRouteKey(model.RouteOriginVenue, "cand_dining_003", model.TravelWalk),
		From:           model.Waypoint{Name: "東京ドーム", Location: model.Location{Lat: 35.7056, Lng: 139.7519}},
		To:             model.Waypoint{Name: "居酒屋 ○○ 水道橋店", Location: dining.Loc},
		DistanceMeters: 650,
		Duration:       8 * time.Minute,
		MapsURL:        "https://www.google.com/maps/dir/?api=1",
	}

	return &model.Plan{
		ID:          model.PlanID("pln_01HX8Z9K3M4N5P6Q7R8S9T0V1W"),
		Status:      model.StatusCompleted,
		GeneratedAt: at(21, 4),
		ExpiresAt:   at(21, 19),
		ShareURL:    "https://lifestyle-mapper.app/plan/pln_01HX8Z9K3M4N5P6Q7R8S9T0V1W",
		Narrative: &model.PlanNarrative{
			Title: "終演の余韻を、水道橋の深夜に持ち帰る", Summary: "退場の混雑を30分やり過ごしてから。",
			ClosingNote: "歩く距離を最小にしました。", Vibe: "relaxed_late_night",
		},
		Summary: model.PlanSummary{
			StartAt: at(21, 0), EndAt: time.Date(2026, 9, 13, 10, 0, 0, 0, jst),
			TotalWalk: 14 * time.Minute, TotalWalkMeters: 1080,
			Cost:        model.CostBreakdown{DiningJPY: 8000, LodgingJPY: 16200},
			Feasibility: model.FeasibilityOK,
		},
		Timeline: model.Timeline{
			model.NewBufferSegment(1, at(21, 0), 30*time.Minute, model.BufferExitCongestion, "東京ドーム"),
			model.NewMoveSegment(2, at(21, 30), route),
			// リンクが 1 件も付かない飲食セグメント（電話も place id も無い店）。
			model.NewDiningSegment(3, at(21, 38), 90*time.Minute, dining, nil),
			model.NewLodgingSegment(4, at(23, 20), time.Date(2026, 9, 13, 10, 0, 0, 0, jst),
				model.LodgingChoice{Hotel: hotel, Plan: &hotelPlan},
				[]model.Link{{Kind: model.LinkAffiliate, Provider: model.LinkProviderRakutenTravel,
					Label: "楽天トラベルで予約", URL: "https://hb.afl.rakuten.co.jp/hgc/xxxx", TrackingID: "trk_b7"}}),
		},
		Alternatives: model.Alternatives{
			Dining: []model.DiningAlternative{{Place: dining, ExcludedReason: "23時閉店のため"}},
		},
		Warnings: []model.Warning{
			model.NewWarning(model.WarnVacancyLow, model.SeverityInfo, "seg_4", "残室は2室です。"),
		},
	}
}

// 応答 JSON を素の map に戻し、openapi が required にしている項目が
// 実際に出ているかを見る。構造体の型検査では「消えている」ことに気づけない。
func marshalPlan(t *testing.T, p *model.Plan) map[string]any {
	t.Helper()
	b, err := json.Marshal(NewPlan(p))
	if err != nil {
		t.Fatalf("直列化に失敗: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("復元に失敗: %v", err)
	}
	return out
}

func TestPlanJSONHasRequiredFields(t *testing.T) {
	got := marshalPlan(t, samplePlan())

	for _, key := range []string{"planId", "status", "generatedAt", "narrative", "summary", "timeline", "alternatives", "warnings"} {
		if _, ok := got[key]; !ok {
			t.Errorf("必須項目がありません: %s", key)
		}
	}
	if got["planId"] != "pln_01HX8Z9K3M4N5P6Q7R8S9T0V1W" {
		t.Errorf("planId = %v", got["planId"])
	}
	// 時刻はオフセット付きで出す。
	if got["generatedAt"] != "2026-09-12T21:04:00+09:00" {
		t.Errorf("generatedAt = %v", got["generatedAt"])
	}
	summary := got["summary"].(map[string]any)
	// total は内訳からの導出値。食い違わせない。
	cost := summary["estimatedCostJpy"].(map[string]any)
	if cost["total"] != float64(24200) {
		t.Errorf("total = %v", cost["total"])
	}
	if summary["totalWalkMinutes"] != float64(14) {
		t.Errorf("totalWalkMinutes = %v", summary["totalWalkMinutes"])
	}
}

// 判別可能ユニオンの不変条件: type に対応する詳細だけが出て、他は出ない。
func TestSegmentUnionShape(t *testing.T) {
	got := marshalPlan(t, samplePlan())
	timeline := got["timeline"].([]any)
	if len(timeline) != 4 {
		t.Fatalf("timeline の件数 = %d", len(timeline))
	}

	details := []string{"buffer", "move", "dining", "lodging"}
	for _, raw := range timeline {
		seg := raw.(map[string]any)
		typ := seg["type"].(string)
		for _, d := range details {
			_, present := seg[d]
			if d == typ && !present {
				t.Errorf("type=%s に %s がありません", typ, d)
			}
			if d != typ && present {
				t.Errorf("type=%s に %s が混ざっています", typ, d)
			}
		}
		// narrative は LLM 由来。無くても項目自体は null で出す（フロントの型が固定される）。
		if _, ok := seg["narrative"]; !ok {
			t.Errorf("%s: narrative の項目がありません", typ)
		}
		// links は dining / lodging でのみ必須。buffer / move には概念ごと無い。
		_, hasLinks := seg["links"]
		wantLinks := typ == "dining" || typ == "lodging"
		if hasLinks != wantLinks {
			t.Errorf("type=%s: links の有無 = %v, want %v", typ, hasLinks, wantLinks)
		}
	}

	dining := timeline[2].(map[string]any)
	// リンクが 0 件でも null ではなく空配列。フロントに length を数えさせる。
	if links, ok := dining["links"].([]any); !ok || len(links) != 0 {
		t.Errorf("dining.links = %#v", dining["links"])
	}
	// 営業判定の基準は到着予定時刻。21:38 着なら 2:00 まで開いている。
	status := dining["dining"].(map[string]any)["openingStatus"].(map[string]any)
	if status["openNow"] != true || status["closesAt"] != "2026-09-13T02:00:00+09:00" {
		t.Errorf("openingStatus = %#v", status)
	}
	lodging := timeline[3].(map[string]any)["lodging"].(map[string]any)
	plan := lodging["plan"].(map[string]any)
	// 24 時超えの表記は提供元仕様のまま返す。
	if plan["checkInDeadline"] != "26:00" {
		t.Errorf("checkInDeadline = %v", plan["checkInDeadline"])
	}
}

// LLM を止めた日でも timeline だけで機能が成立することが narrative 分離の目的。
func TestPlanWithoutNarrative(t *testing.T) {
	p := samplePlan()
	p.Narrative = nil
	for i := range p.Timeline {
		p.Timeline[i].Narrative = nil
	}
	got := marshalPlan(t, p)
	if v, ok := got["narrative"]; !ok || v != nil {
		t.Errorf("narrative = %#v, want null", v)
	}
	if len(got["timeline"].([]any)) != 4 {
		t.Error("narrative が無いとタイムラインが欠けます")
	}
}

// 空でも配列で返す。null と空配列をフロントに区別させない。
func TestEmptyCollectionsAreArrays(t *testing.T) {
	got := marshalPlan(t, &model.Plan{ID: model.PlanID("pln_1"), Status: model.StatusQueued})
	for _, key := range []string{"timeline", "warnings"} {
		if v, ok := got[key].([]any); !ok || v == nil {
			t.Errorf("%s = %#v, want []", key, got[key])
		}
	}
	alt := got["alternatives"].(map[string]any)
	for _, key := range []string{"dining", "lodging"} {
		if v, ok := alt[key].([]any); !ok || v == nil {
			t.Errorf("alternatives.%s = %#v, want []", key, alt[key])
		}
	}

	meta := NewMeta("req_1", nil, nil)
	b, _ := json.Marshal(meta)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if v, ok := m["sources"].([]any); !ok || v == nil {
		t.Errorf("meta.sources = %#v, want []", m["sources"])
	}
	// 使わなかった LLM は null。空オブジェクトにすると「呼んだが空」と読めてしまう。
	if v, ok := m["llm"]; !ok || v != nil {
		t.Errorf("meta.llm = %#v, want null", v)
	}
}

func TestMetaCarriesSourcesAndLLM(t *testing.T) {
	sources := []model.SourceStatus{
		{Provider: model.ProviderGooglePlaces, State: model.SourceOK, Latency: 412 * time.Millisecond, ResultCount: 24},
		{Provider: model.ProviderRakutenTravel, State: model.SourceDegraded, Latency: 3 * time.Second,
			Err: apperror.New(apperror.CodeUpstreamTimeout, "")},
	}
	llm := &model.LLMMeta{
		Provider: model.LLMAnthropic, Model: "claude-opus-5", SchemaVersion: "plan_output.v1",
		PromptVersion: "plan_v1", Latency: 4820 * time.Millisecond, InputTokens: 3204,
		OutputTokens: 1180, RepairAttempts: 1,
	}
	meta := NewMeta("req_1", sources, llm)

	if len(meta.Sources) != 2 || meta.Sources[0].LatencyMs != 412 {
		t.Fatalf("sources = %+v", meta.Sources)
	}
	// 部分失敗は隠さない。上流の失敗理由まで返す。
	if meta.Sources[1].Error == nil || meta.Sources[1].Error.Code != string(apperror.CodeUpstreamTimeout) {
		t.Errorf("degraded な source に error がありません: %+v", meta.Sources[1])
	}
	if meta.LLM == nil || meta.LLM.RepairAttempts != 1 || meta.LLM.LatencyMs != 4820 {
		t.Errorf("llm = %+v", meta.LLM)
	}
}

func TestNewErrorMapsStatusAndHidesCause(t *testing.T) {
	status, env := NewError(apperror.Validation(
		apperror.FieldError{Field: "event.endsAt", Reason: "過去の日時は指定できません"}), "req_1")

	if status != http.StatusBadRequest {
		t.Errorf("status = %d", status)
	}
	if env.Error.Code != string(apperror.CodeValidationFailed) || env.Error.RequestID != "req_1" {
		t.Errorf("error = %+v", env.Error)
	}
	if len(env.Error.Details) != 1 || env.Error.Details[0].Field != "event.endsAt" {
		t.Errorf("details = %+v", env.Error.Details)
	}

	// ドメインエラーでない error は内部エラーに丸め、生のメッセージを漏らさない。
	status, env = NewError(errors.New("pq: connection refused to 10.0.1.5"), "req_2")
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d", status)
	}
	if env.Error.Message == "pq: connection refused to 10.0.1.5" {
		t.Error("内部のエラーメッセージがそのまま外に出ています")
	}
}

func TestNewPlanCreated(t *testing.T) {
	got := NewPlanCreated(model.PlanID("pln_01HX8Z9K3M4N5P6Q7R8S9T0V1W"), model.StatusQueued)
	if got.Status != "queued" {
		t.Errorf("status = %q", got.Status)
	}
	// 202 の時点で購読先を教える。フロントが URL を組み立てずに済む。
	if got.StreamURL != "/v1/plans/pln_01HX8Z9K3M4N5P6Q7R8S9T0V1W/events" {
		t.Errorf("streamUrl = %q", got.StreamURL)
	}
}

func TestNewStreamEvent(t *testing.T) {
	id := model.PlanID("pln_01HX8Z9K3M4N5P6Q7R8S9T0V1W")
	now := at(21, 0)

	ev, ok := NewStreamEvent(model.NewStatusEvent(id, model.StatusCollecting, now), "req_1")
	if !ok || ev.Name != "status" || ev.Data.(StatusPayload).Status != "collecting" {
		t.Fatalf("status イベント = %+v (ok=%v)", ev, ok)
	}
	if ev.Terminal() {
		t.Error("status でストリームを閉じてはいけません")
	}

	src := model.SourceStatus{Provider: model.ProviderGooglePlaces, State: model.SourceOK, ResultCount: 24}
	ev, ok = NewStreamEvent(model.NewSourceEvent(id, src, now), "req_1")
	if !ok || ev.Name != "sources" || ev.Data.(SourceStatus).ResultCount != 24 {
		t.Fatalf("sources イベント = %+v (ok=%v)", ev, ok)
	}

	// plan イベントの data は GET と同一形状。フロントは型を 1 つしか持たない。
	ev, ok = NewStreamEvent(model.NewPlanEvent(samplePlan(), now), "req_1")
	if !ok || ev.Name != "plan" || !ev.Terminal() {
		t.Fatalf("plan イベント = %+v (ok=%v)", ev, ok)
	}
	if _, isPlan := ev.Data.(Plan); !isPlan {
		t.Errorf("plan イベントの data 型 = %T", ev.Data)
	}

	ev, ok = NewStreamEvent(model.NewErrorEvent(id, apperror.NotFound("test"), now), "req_1")
	if !ok || ev.Name != "error" || !ev.Terminal() {
		t.Fatalf("error イベント = %+v (ok=%v)", ev, ok)
	}
	if body := ev.Data.(ErrorBody); body.Code != string(apperror.CodePlanNotFound) || body.RequestID != "req_1" {
		t.Errorf("error の data = %+v", body)
	}

	// 中身の無いイベントは配信しない。
	if _, ok := NewStreamEvent(model.PlanEvent{Type: model.EventPlan}, ""); ok {
		t.Error("Plan が nil のイベントを配信しようとしました")
	}
}
