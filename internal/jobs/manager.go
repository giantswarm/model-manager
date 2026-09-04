// Package jobs tracks long-running backend operations (model pulls) as
// in-memory jobs with progress, so the API can return a job id immediately
// and clients poll for state.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/identity"
)

// Phase is the job lifecycle state.
type Phase string

const (
	PhasePending   Phase = "pending"
	PhaseRunning   Phase = "running"
	PhaseSucceeded Phase = "succeeded"
	PhaseFailed    Phase = "failed"
	PhaseCancelled Phase = "cancelled"
)

// Type is the kind of operation a job runs.
type Type string

const (
	// TypePull is a model import.
	TypePull Type = "pull"
	// TypeLoad tracks a served model from start to ready (and its wiring) on
	// backends where serving takes time (kserve InferenceServices).
	TypeLoad Type = "load"
)

// ErrNotFound means no job has that id.
var ErrNotFound = errors.New("job not found")

// Job is a snapshot of one operation. Values returned by the manager are
// copies; mutate nothing.
type Job struct {
	ID   string `json:"id"`
	Type Type   `json:"type"`
	// Backend is the driver the job runs on; with several backends the same
	// reference pulled on two of them is two jobs.
	Backend backend.Name `json:"backend,omitempty"`
	Model   string       `json:"model"`
	Phase   Phase        `json:"phase"`
	// Status is the backend's last progress message (e.g. "pulling manifest").
	Status         string     `json:"status,omitempty"`
	BytesCompleted int64      `json:"bytesCompleted"`
	BytesTotal     int64      `json:"bytesTotal"`
	Percent        float64    `json:"percent"`
	Error          string     `json:"error,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	// Wire records whether the job wires the model into kagent when done.
	Wire bool `json:"wire"`
	// Node is the node whose cache receives a pull (kserve): the one the
	// request named, else the one the backend picked after its fit check.
	// Empty on backends without placement (ollama) and on a shared cache.
	Node string `json:"node,omitempty"`
	// Preset is the serving preset a pull is for (kserve): the one the request
	// named, else the single preset the backend resolved for the model.
	Preset string `json:"preset,omitempty"`
	// RequestedBy is the caller who started the job (email, else the IdP
	// subject) when the request was authenticated; empty for jobs the service
	// starts on its own (adopted downloads).
	RequestedBy string `json:"requestedBy,omitempty"`
	// Result is operation-specific output (pull: the ModelConfig reference).
	Result any `json:"result,omitempty"`
}

// Done reports whether the job reached a terminal phase.
func (j Job) Done() bool {
	return j.Phase == PhaseSucceeded || j.Phase == PhaseFailed || j.Phase == PhaseCancelled
}

// RunFunc performs the operation, reporting progress, and returns a result.
type RunFunc func(ctx context.Context, report func(backend.Progress)) (any, error)

type entry struct {
	job    Job
	cancel context.CancelFunc
}

// Manager owns the job table.
type Manager struct {
	mu        sync.Mutex
	jobs      map[string]*entry
	retention time.Duration
	maxJobs   int
	now       func() time.Time
	newID     func() string
	wg        sync.WaitGroup
}

// Option configures a Manager.
type Option func(*Manager)

// WithRetention sets how long finished jobs stay listed (default 24h).
func WithRetention(d time.Duration) Option { return func(m *Manager) { m.retention = d } }

// WithMaxJobs caps the number of finished jobs kept (default 200).
func WithMaxJobs(n int) Option { return func(m *Manager) { m.maxJobs = n } }

// WithClock replaces the clock (tests).
func WithClock(now func() time.Time) Option { return func(m *Manager) { m.now = now } }

// NewManager builds a Manager.
func NewManager(opts ...Option) *Manager {
	m := &Manager{
		jobs:      map[string]*entry{},
		retention: 24 * time.Hour,
		maxJobs:   200,
		now:       time.Now,
		newID:     randomID,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// StartRequest describes the job to start.
type StartRequest struct {
	Type Type
	// Backend is the driver the job runs on (part of the dedupe key).
	Backend backend.Name
	Model   string
	Wire    bool
	// Context is the request that starts the job. Its values — the caller's
	// identity and, with downstream OAuth, the caller's token — are inherited
	// by the job's own context so the job's Kubernetes writes are made as the
	// caller for as long as the token lives; its cancellation is not (the job
	// outlives the HTTP request). Nil starts the job without a caller.
	Context context.Context
	// Node and Preset are what the request named (or what a re-adopted
	// download Job carries); the backend refines them through Progress.
	Node   string
	Preset string
}

// Start begins a job in the background and returns its initial snapshot. If a
// job of the same type for the same model on the same backend is still
// pending/running, that job is returned instead and created is false.
func (m *Manager) Start(req StartRequest, fn RunFunc) (job Job, created bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	for _, e := range m.jobs {
		if e.job.Type == req.Type && e.job.Backend == req.Backend && e.job.Model == req.Model && !e.job.Done() {
			return e.job, false
		}
	}
	parent := req.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	e := &entry{
		job: Job{
			ID:          m.newID(),
			Type:        req.Type,
			Backend:     req.Backend,
			Model:       req.Model,
			Phase:       PhasePending,
			CreatedAt:   m.now(),
			Wire:        req.Wire,
			Node:        req.Node,
			Preset:      req.Preset,
			RequestedBy: identity.Caller(parent),
		},
		cancel: cancel,
	}
	m.jobs[e.job.ID] = e
	m.wg.Add(1)
	go m.run(ctx, e.job.ID, fn)
	return e.job, true
}

func (m *Manager) run(ctx context.Context, id string, fn RunFunc) {
	defer m.wg.Done()
	m.update(id, func(j *Job) {
		t := m.now()
		j.Phase = PhaseRunning
		j.StartedAt = &t
	})
	report := func(p backend.Progress) {
		m.update(id, func(j *Job) {
			if p.Status != "" {
				j.Status = p.Status
			}
			if p.BytesTotal > 0 {
				j.BytesTotal = p.BytesTotal
			}
			if p.BytesCompleted > j.BytesCompleted {
				j.BytesCompleted = p.BytesCompleted
			}
			if p.Node != "" {
				j.Node = p.Node
			}
			if p.Preset != "" {
				j.Preset = p.Preset
			}
			if j.BytesTotal > 0 {
				j.Percent = float64(j.BytesCompleted) / float64(j.BytesTotal) * 100
				if j.Percent > 100 {
					j.Percent = 100
				}
			}
		})
	}
	result, err := fn(ctx, report)
	m.update(id, func(j *Job) {
		t := m.now()
		j.FinishedAt = &t
		switch {
		case err != nil && errors.Is(err, context.Canceled) && ctx.Err() != nil:
			j.Phase = PhaseCancelled
			j.Error = "cancelled"
		case err != nil:
			j.Phase = PhaseFailed
			j.Error = err.Error()
		default:
			j.Phase = PhaseSucceeded
			j.Result = result
			if j.BytesTotal > 0 {
				j.BytesCompleted = j.BytesTotal
			}
			j.Percent = 100
		}
	})
}

func (m *Manager) update(id string, fn func(*Job)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.jobs[id]; ok {
		fn(&e.job)
	}
}

// Get returns a job snapshot.
func (m *Manager) Get(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return e.job, nil
}

// List returns all jobs, newest first.
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	out := make([]Job, 0, len(m.jobs))
	for _, e := range m.jobs {
		out = append(out, e.job)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].CreatedAt.Equal(out[k].CreatedAt) {
			return out[i].ID < out[k].ID
		}
		return out[i].CreatedAt.After(out[k].CreatedAt)
	})
	return out
}

// Cancel stops a pending/running job. Cancelling a finished job is a no-op.
func (m *Manager) Cancel(id string) (Job, error) {
	m.mu.Lock()
	e, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return Job{}, ErrNotFound
	}
	cancel := e.cancel
	job := e.job
	m.mu.Unlock()
	if !job.Done() {
		cancel()
	}
	return m.Get(id)
}

// Wait blocks until all running jobs finished (shutdown, tests).
func (m *Manager) Wait() { m.wg.Wait() }

// pruneLocked drops finished jobs beyond the retention window or count cap.
func (m *Manager) pruneLocked() {
	cutoff := m.now().Add(-m.retention)
	finished := make([]*entry, 0)
	for id, e := range m.jobs {
		if !e.job.Done() || e.job.FinishedAt == nil {
			continue
		}
		if e.job.FinishedAt.Before(cutoff) {
			delete(m.jobs, id)
			continue
		}
		finished = append(finished, e)
	}
	if len(finished) <= m.maxJobs {
		return
	}
	sort.Slice(finished, func(i, k int) bool { return finished[i].job.FinishedAt.Before(*finished[k].job.FinishedAt) })
	for _, e := range finished[:len(finished)-m.maxJobs] {
		delete(m.jobs, e.job.ID)
	}
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))[:16]
	}
	return hex.EncodeToString(b)
}
