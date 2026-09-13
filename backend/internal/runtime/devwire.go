package runtime

import (
	"context"
	"sync"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
	planservice "github.com/taka/lifestyle-mapper/backend/internal/service/plan"
)

// InMemoryPlanRepository is a simple in-memory repository for dev/testing.
type InMemoryPlanRepository struct {
	mu sync.RWMutex
	m  map[model.PlanID]*model.Plan
}

func NewInMemoryPlanRepository() *InMemoryPlanRepository {
	return &InMemoryPlanRepository{m: make(map[model.PlanID]*model.Plan)}
}

func (r *InMemoryPlanRepository) Save(ctx context.Context, p *model.Plan) error {
	if p == nil {
		return apperror.Internal(nil, "repo.Save")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[p.ID] = p
	return nil
}

func (r *InMemoryPlanRepository) Find(ctx context.Context, id model.PlanID) (*model.Plan, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.m[id]
	if !ok {
		return nil, nil
	}
	return p, nil
}

// Broadcaster publishes PlanEvent to subscribers.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[model.PlanID][]chan model.PlanEvent
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[model.PlanID][]chan model.PlanEvent)}
}

func (b *Broadcaster) Publish(ev model.PlanEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[ev.PlanID] {
		select {
		case ch <- ev:
		default:
			// don't block on slow subscriber
		}
	}
}

// Subscribe implements a subscriber pattern used by StreamController.
func (b *Broadcaster) Subscribe(ctx context.Context, id model.PlanID) (<-chan model.PlanEvent, error) {
	ch := make(chan model.PlanEvent, 16)
	b.mu.Lock()
	b.subs[id] = append(b.subs[id], ch)
	b.mu.Unlock()

	out := make(chan model.PlanEvent)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				// remove subscription
				b.mu.Lock()
				arr := b.subs[id]
				for i, c := range arr {
					if c == ch {
						arr = append(arr[:i], arr[i+1:]...)
						break
					}
				}
				b.subs[id] = arr
				b.mu.Unlock()
				return
			case e, ok := <-ch:
				if !ok {
					return
				}
				out <- e
				if e.Terminal() {
					return
				}
			}
		}
	}()
	return out, nil
}

// ClickRecorder is a noop recorder for local development.
type ClickRecorder struct{}

func NewClickRecorder() *ClickRecorder { return &ClickRecorder{} }

func (c *ClickRecorder) Record(ctx context.Context, e model.ClickEvent) error {
	// Keep local: could log or persist later.
	return nil
}

// PlanOrchestrator wires Service + Repo + Broadcaster to provide controller-level
// Create/Get behavior for local dev.
type PlanOrchestrator struct {
	svc  *planservice.Service
	repo planservice.Repository
	b    *Broadcaster
}

func NewPlanOrchestrator(svc *planservice.Service, repo planservice.Repository, b *Broadcaster) *PlanOrchestrator {
	return &PlanOrchestrator{svc: svc, repo: repo, b: b}
}

func (o *PlanOrchestrator) Create(ctx context.Context, cond *model.SearchCondition) (model.PlanID, model.PlanStatus, error) {
	id := model.NewPlanID()
	p := &model.Plan{ID: id, Status: model.StatusQueued, GeneratedAt: time.Now(), ExpiresAt: time.Now().Add(5 * time.Minute)}
	_ = o.repo.Save(ctx, p)

	o.b.Publish(model.NewStatusEvent(id, model.StatusQueued, time.Now()))

	go func() {
		o.b.Publish(model.NewStatusEvent(id, model.StatusCollecting, time.Now()))
		res, err := o.svc.Generate(context.Background(), cond)
		if err != nil {
			o.b.Publish(model.NewErrorEvent(id, err, time.Now()))
			return
		}
		for _, s := range res.Sources {
			o.b.Publish(model.NewSourceEvent(res.Plan.ID, s, time.Now()))
		}
		o.b.Publish(model.NewPlanEvent(res.Plan, time.Now()))
	}()
	return id, model.StatusQueued, nil
}

func (o *PlanOrchestrator) Get(ctx context.Context, id model.PlanID) (*model.Plan, error) {
	return o.repo.Find(ctx, id)
}
