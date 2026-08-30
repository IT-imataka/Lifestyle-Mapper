package view

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// 設計書 2-A のリクエスト例をそのまま入れて、ドメインに移せることを確かめる。
const fullRequestJSON = `{
  "event": {
    "eventId": null,
    "name": "○○ TOUR 2026 東京公演",
    "venue": {
      "name": "東京ドーム",
      "address": "東京都文京区後楽1-3-61",
      "location": { "lat": 35.7056, "lng": 139.7519 },
      "googlePlaceId": "ChIJK1kFhcSMGGARFwQMs73Kejo"
    },
    "endsAt": "2026-09-12T21:00:00+09:00",
    "exitBufferMinutes": 30
  },
  "party": { "adults": 2, "children": 0, "relationship": "friends" },
  "situation": {
    "intent": "stay",
    "mood": ["celebrate", "relaxed"],
    "notes": "初めての遠征で土地勘がありません。荷物が多めです。",
    "physicalLimit": { "maxWalkMinutes": 12 }
  },
  "dining": {
    "enabled": true,
    "genres": ["izakaya", "ramen"],
    "budgetPerPersonJpy": { "min": 2000, "max": 5000 },
    "requirements": ["open_late", "reservable", "non_smoking"],
    "desiredStayMinutes": 90
  },
  "lodging": {
    "enabled": true,
    "checkInDate": "2026-09-12",
    "checkOutDate": "2026-09-13",
    "rooms": 1,
    "budgetPerNightJpy": { "min": 0, "max": 18000 },
    "requirements": ["no_smoking", "late_checkin_ok", "with_bath"]
  },
  "returnTrip": { "destination": { "type": "station", "name": "新宿" }, "checkLastTrain": true },
  "options": {
    "transportModes": ["WALK", "TRANSIT"],
    "maxCandidatesPerCategory": 8,
    "llmNarrative": true
  },
  "context": { "locale": "ja-JP", "currency": "JPY", "timezone": "Asia/Tokyo" }
}`

func decode(t *testing.T, body string) *SearchRequest {
	t.Helper()
	var req SearchRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("デコードに失敗: %v", err)
	}
	return &req
}

func TestToModelFullRequest(t *testing.T) {
	cond, aerr := decode(t, fullRequestJSON).ToModel()
	if aerr != nil {
		t.Fatalf("変換に失敗: %v", aerr)
	}
	cond.ApplyDefaults()

	// 起点の時刻はオフセットごと保つ。UTC に丸めると 9 時間ずれたプランになる。
	if got := cond.Event.EndsAt.Format(time.RFC3339); got != "2026-09-12T21:00:00+09:00" {
		t.Errorf("endsAt = %q", got)
	}
	if cond.Event.ExitBuffer != 30*time.Minute {
		t.Errorf("exitBuffer = %v", cond.Event.ExitBuffer)
	}
	if cond.Event.Venue.Location.Lat != 35.7056 || cond.Event.Venue.GooglePlaceID == "" {
		t.Errorf("venue = %+v", cond.Event.Venue)
	}
	if cond.Situation.MaxWalk != 12*time.Minute {
		t.Errorf("maxWalk = %v", cond.Situation.MaxWalk)
	}
	if len(cond.Situation.Moods) != 2 || cond.Situation.Moods[0] != model.MoodCelebrate {
		t.Errorf("moods = %v", cond.Situation.Moods)
	}
	if cond.Dining.DesiredStay != 90*time.Minute {
		t.Errorf("desiredStay = %v", cond.Dining.DesiredStay)
	}
	if cond.Dining.BudgetPerPerson.Min != 2000 || cond.Dining.BudgetPerPerson.Max == nil || *cond.Dining.BudgetPerPerson.Max != 5000 {
		t.Errorf("dining budget = %+v", cond.Dining.BudgetPerPerson)
	}
	if cond.Lodging.Nights() != 1 {
		t.Errorf("宿泊数 = %d", cond.Lodging.Nights())
	}
	// 日付はリクエストのタイムゾーンで解釈する。UTC で読むと前日になる。
	if zone, _ := cond.Lodging.CheckIn.Zone(); zone != "JST" {
		t.Errorf("checkInDate のゾーン = %q（%v）", zone, cond.Lodging.CheckIn)
	}
	if cond.ReturnTrip.Destination.Type != model.DestinationStation {
		t.Errorf("destination = %+v", cond.ReturnTrip.Destination)
	}
	if !cond.Options.AllowsMode(model.TravelTransit) {
		t.Errorf("transportModes = %v", cond.Options.TransportModes)
	}
	if err := cond.Validate(time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("設計書の例が検証に落ちました: %v", err)
	}
}

// 省略された bool は既定値、明示された false はその通りに解釈する。
// ゼロ値では区別できないためポインタで受けている箇所の回帰テスト。
func TestToModelBoolDefaults(t *testing.T) {
	minimal := `{"event":{"venue":{"name":"東京ドーム","location":{"lat":35.7,"lng":139.75}},
	  "endsAt":"2026-09-12T21:00:00+09:00"},"situation":{"intent":"stay"}}`
	cond, aerr := decode(t, minimal).ToModel()
	if aerr != nil {
		t.Fatalf("変換に失敗: %v", aerr)
	}
	if !cond.Dining.Enabled || !cond.Lodging.Enabled {
		t.Error("省略時は飲食・宿泊とも有効であるべきです")
	}
	if !cond.Options.LLMNarrative {
		t.Error("省略時は llmNarrative=true であるべきです")
	}
	if !cond.ReturnTrip.CheckLastTrain {
		t.Error("省略時は checkLastTrain=true であるべきです")
	}

	explicit := `{"event":{"venue":{"name":"東京ドーム","location":{"lat":35.7,"lng":139.75}},
	  "endsAt":"2026-09-12T21:00:00+09:00"},"situation":{"intent":"return"},
	  "lodging":{"enabled":false},"options":{"llmNarrative":false},"returnTrip":{"checkLastTrain":false}}`
	cond, aerr = decode(t, explicit).ToModel()
	if aerr != nil {
		t.Fatalf("変換に失敗: %v", aerr)
	}
	if cond.Lodging.Enabled {
		t.Error("明示的な false が既定値に上書きされました: lodging.enabled")
	}
	if cond.Options.LLMNarrative {
		t.Error("明示的な false が既定値に上書きされました: options.llmNarrative")
	}
	if cond.ReturnTrip.CheckLastTrain {
		t.Error("明示的な false が既定値に上書きされました: returnTrip.checkLastTrain")
	}
}

// 書式として読めない値は VALIDATION_FAILED になり、openapi のパスで項目を指す。
func TestToModelReportsUnparsableFields(t *testing.T) {
	body := `{"event":{"venue":{"name":"東京ドーム","location":{"lat":35.7,"lng":139.75}},
	  "endsAt":"2026-09-12 21:00:00"},"situation":{"intent":"stay"},
	  "lodging":{"checkInDate":"2026/09/12"}}`
	cond, aerr := decode(t, body).ToModel()
	if cond != nil || aerr == nil {
		t.Fatalf("不正な書式が通ってしまいました: %+v", cond)
	}
	if aerr.Code != apperror.CodeValidationFailed {
		t.Errorf("Code = %q", aerr.Code)
	}
	fields := map[string]bool{}
	for _, d := range aerr.Details {
		fields[d.Field] = true
	}
	for _, want := range []string{"event.endsAt", "lodging.checkInDate"} {
		if !fields[want] {
			t.Errorf("%s の指摘がありません: %+v", want, aerr.Details)
		}
	}
}

// オフセット無しの時刻を黙って受けると、9 時間ずれたプランを組んでしまう。
func TestToModelRejectsTimeWithoutOffset(t *testing.T) {
	body := `{"event":{"venue":{"name":"東京ドーム","location":{"lat":35.7,"lng":139.75}},
	  "endsAt":"2026-09-12T21:00:00"},"situation":{"intent":"stay"}}`
	if _, aerr := decode(t, body).ToModel(); aerr == nil {
		t.Fatal("オフセット無しの endsAt が通ってしまいました")
	}
}

func TestClickRequestToModel(t *testing.T) {
	now := time.Date(2026, 9, 12, 23, 0, 0, 0, time.UTC)
	seg := "seg_5"
	at := "2026-09-12T23:10:00+09:00"
	e := ClickRequest{
		PlanID:     "pln_01HX8Z9K3M4N5P6Q7R8S9T0V1W",
		SegmentID:  &seg,
		TrackingID: "trk_b7",
		Provider:   "rakuten_travel",
		ClickedAt:  &at,
	}.ToModel(now, "req_1")

	if !e.Valid() {
		t.Fatalf("有効なクリックが弾かれました: %+v", e)
	}
	if e.ClickedAt.Format(time.RFC3339) != at {
		t.Errorf("clickedAt = %v", e.ClickedAt)
	}
	if e.Provider != model.LinkProviderRakutenTravel || e.SegmentID != seg || e.RequestID != "req_1" {
		t.Errorf("変換結果 = %+v", e)
	}

	// 読めない clickedAt はサーバ時刻で補う。計測のために落とさない。
	broken := "きのう"
	e = ClickRequest{PlanID: "pln_01HX8Z9K3M4N5P6Q7R8S9T0V1W", TrackingID: "trk_b7", ClickedAt: &broken}.ToModel(now, "")
	if !e.ClickedAt.Equal(now) {
		t.Errorf("clickedAt = %v, want %v", e.ClickedAt, now)
	}

	// planId が無いクリックは記録に値しない（ただし応答は 204 のまま）。
	if (ClickRequest{TrackingID: "trk_b7"}).ToModel(now, "").Valid() {
		t.Error("planId 無しのクリックが有効と判定されました")
	}
}
