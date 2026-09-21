package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// PlanRepository persists plans in PostgreSQL.
type PlanRepository struct {
	db *sql.DB
}

func NewPlanRepository(db *sql.DB) *PlanRepository {
	return &PlanRepository{db: db}
}

func (r *PlanRepository) Save(ctx context.Context, p *model.Plan) error {
	if r.db == nil {
		return apperror.Internal(fmt.Errorf("DB connection is nil"), "repository.PlanRepository.Save")
	}
	if p == nil {
		return apperror.Internal(fmt.Errorf("plan is nil"), "repository.PlanRepository.Save")
	}

	payload, err := marshalPlan(p)
	if err != nil {
		return apperror.Wrap(err, apperror.CodeInternal, "repository.PlanRepository.Save")
	}

	_, err = r.db.ExecContext(ctx, `
		INSERT INTO plans (id, status, generated_at, expires_at, share_url, payload)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			generated_at = EXCLUDED.generated_at,
			expires_at = EXCLUDED.expires_at,
			share_url = EXCLUDED.share_url,
			payload = EXCLUDED.payload,
			updated_at = NOW()
	`, p.ID.String(), string(p.Status), p.GeneratedAt, p.ExpiresAt, p.ShareURL, payload)
	if err != nil {
		return apperror.Wrap(err, apperror.CodeInternal, "repository.PlanRepository.Save")
	}
	return nil
}

func (r *PlanRepository) Find(ctx context.Context, id model.PlanID) (*model.Plan, error) {
	if r.db == nil {
		return nil, apperror.Internal(fmt.Errorf("DB connection is nil"), "repository.PlanRepository.Find")
	}

	var (
		storedID    string
		status      string
		generatedAt time.Time
		expiresAt   time.Time
		shareURL    string
		payload     []byte
	)

	err := r.db.QueryRowContext(ctx, `
		SELECT id, status, generated_at, expires_at, share_url, payload
		FROM plans WHERE id = $1
	`, id.String()).Scan(&storedID, &status, &generatedAt, &expiresAt, &shareURL, &payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperror.Wrap(err, apperror.CodeInternal, "repository.PlanRepository.Find")
	}

	p := &model.Plan{ID: model.PlanID(storedID), Status: model.PlanStatus(status), GeneratedAt: generatedAt, ExpiresAt: expiresAt, ShareURL: shareURL}
	if err := unmarshalPlan(payload, p); err != nil {
		return nil, apperror.Wrap(err, apperror.CodeInternal, "repository.PlanRepository.Find")
	}
	return p, nil
}

func marshalPlan(p *model.Plan) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("plan is nil")
	}
	return json.Marshal(struct {
		ID           string               `json:"id"`
		Status       string               `json:"status"`
		GeneratedAt  time.Time            `json:"generatedAt"`
		ExpiresAt    time.Time            `json:"expiresAt"`
		ShareURL     string               `json:"shareUrl"`
		Narrative    *model.PlanNarrative `json:"narrative,omitempty"`
		Summary      model.PlanSummary    `json:"summary"`
		Timeline     model.Timeline       `json:"timeline"`
		Alternatives model.Alternatives   `json:"alternatives"`
		Warnings     []model.Warning      `json:"warnings"`
	}{
		ID:           p.ID.String(),
		Status:       string(p.Status),
		GeneratedAt:  p.GeneratedAt,
		ExpiresAt:    p.ExpiresAt,
		ShareURL:     p.ShareURL,
		Narrative:    p.Narrative,
		Summary:      p.Summary,
		Timeline:     p.Timeline,
		Alternatives: p.Alternatives,
		Warnings:     p.Warnings,
	})
}

func unmarshalPlan(data []byte, p *model.Plan) error {
	if len(data) == 0 {
		return fmt.Errorf("empty payload")
	}
	var raw struct {
		ID           string               `json:"id"`
		Status       string               `json:"status"`
		GeneratedAt  time.Time            `json:"generatedAt"`
		ExpiresAt    time.Time            `json:"expiresAt"`
		ShareURL     string               `json:"shareUrl"`
		Narrative    *model.PlanNarrative `json:"narrative,omitempty"`
		Summary      model.PlanSummary    `json:"summary"`
		Timeline     model.Timeline       `json:"timeline"`
		Alternatives model.Alternatives   `json:"alternatives"`
		Warnings     []model.Warning      `json:"warnings"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("plan is nil")
	}
	if raw.ID != "" {
		p.ID = model.PlanID(raw.ID)
	}
	p.Status = model.PlanStatus(raw.Status)
	p.GeneratedAt = raw.GeneratedAt
	p.ExpiresAt = raw.ExpiresAt
	p.ShareURL = raw.ShareURL
	p.Narrative = raw.Narrative
	p.Summary = raw.Summary
	p.Timeline = raw.Timeline
	p.Alternatives = raw.Alternatives
	p.Warnings = raw.Warnings
	return nil
}
