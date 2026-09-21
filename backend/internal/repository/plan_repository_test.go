package repository

import (
	"context"
	"testing"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

func TestPlanRepositorySaveAndFind(t *testing.T) {
	id := model.NewPlanID()
	p := &model.Plan{
		ID:           id,
		Status:       model.StatusQueued,
		GeneratedAt:  time.Now(),
		ExpiresAt:    time.Now().Add(15 * time.Minute),
		ShareURL:     "https://example.com/plan/" + id.String(),
		Narrative:    &model.PlanNarrative{Title: "夜のプラン", Summary: "食事を楽しむ", ClosingNote: "ゆっくり", Vibe: "relaxed_late_night"},
		Summary:      model.PlanSummary{StartAt: time.Now(), EndAt: time.Now().Add(3 * time.Hour), Feasibility: model.FeasibilityOK},
		Timeline:     model.Timeline{{ID: "seg_1", Type: model.SegmentBuffer, StartAt: time.Now(), EndAt: time.Now().Add(30 * time.Minute), Buffer: &model.BufferDetail{Kind: model.BufferExitCongestion, AtPlaceName: "会場"}}},
		Alternatives: model.Alternatives{},
		Warnings:     []model.Warning{{Code: model.WarnProviderDegraded, Severity: model.SeverityInfo, Message: "たまに遅い"}},
	}

	if !p.ID.Valid() {
		t.Fatalf("plan ID is invalid: %s", p.ID)
	}

	blob, err := marshalPlan(p)
	if err != nil {
		t.Fatalf("marshalPlan returned error: %v", err)
	}
	if len(blob) == 0 {
		t.Fatal("marshalPlan returned empty payload")
	}

	var got model.Plan
	if err := unmarshalPlan(blob, &got); err != nil {
		t.Fatalf("unmarshalPlan returned error: %v", err)
	}
	if got.ID != p.ID {
		t.Fatalf("decoded ID = %q, want %q", got.ID, p.ID)
	}
	if got.Narrative == nil || got.Narrative.Title != p.Narrative.Title {
		t.Fatalf("decoded narrative mismatch: %#v", got.Narrative)
	}
	if got.Status != p.Status {
		t.Fatalf("decoded status = %q, want %q", got.Status, p.Status)
	}
	_ = context.Background()
}
