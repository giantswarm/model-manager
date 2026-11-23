// Package jobs tracks long-running backend operations (model pulls) as
// in-memory jobs with progress, so the API can return a job id immediately
// and clients poll for state.
package jobs

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
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
	// backends where serving takes time (kserve LLMInferenceServices).
	TypeLoad Type = "load"
)

// ErrNotFound means no job has that id.
var (
	ErrNotFound         = errors.New("job not found")
	ErrShuttingDown     = errors.New("job manager is shutting down")
	ErrIdentityRequired = errors.New("complete caller identity required")
)

// Job is a snapshot of one operation. Values returned by the manager are
// copies; mutate nothing.
type Job struct {
	// Scope binds application coordination to the original backend instance.
	Scope string `json:"-"`
	ID    string `json:"id"`
	Type  Type   `json:"type"`
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
	// Preset is the serving preset a pull or a load is for (kserve): the one
	// the request named, else the single preset the backend resolved for the
	// model.
	Preset string `json:"preset,omitempty"`
	// Resource is the serving object a load follows (kserve: the
	// InferenceService or LLMInferenceService name), so a reader can refer
	// to it; empty when the backend listed none.
	Resource string `json:"resource,omitempty"`
	// Object identifies the Kubernetes resource followed by this task.
	Object *backend.JobObject `json:"object,omitempty"`
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
	job       Job
	owner     principal
	key       operationKey
	authorize func(context.Context, Job) error
	cancel    context.CancelFunc
	done      chan struct{} // closed once the job reached a terminal phase
}

// Manager owns the job table.
type Manager struct {
	mu             sync.Mutex
	jobs           map[string]*entry
	retention      time.Duration
	maxJobs        int
	newID          func() string
	lifetime       context.Context
	stopping       bool
	allowAnonymous bool
}

// Option configures a Manager.
type Option func(*Manager)

// WithRetention sets how long finished jobs stay listed (default 24h).
func WithRetention(d time.Duration) Option { return func(m *Manager) { m.retention = d } }

// WithMaxJobs caps the number of finished jobs kept (default 200).
func WithMaxJobs(n int) Option { return func(m *Manager) { m.maxJobs = n } }

// WithLifetime cancels every job when its application owner stops.
func WithLifetime(ctx context.Context) Option { return func(m *Manager) { m.lifetime = ctx } }

// WithAnonymousAccess enables the explicit single-user development mode.
func WithAnonymousAccess(allowed bool) Option { return func(m *Manager) { m.allowAnonymous = allowed } }

// NewManager builds a Manager.
func NewManager(opts ...Option) *Manager {
	m := &Manager{
		jobs:      map[string]*entry{},
		retention: 24 * time.Hour,
		maxJobs:   200,
		newID:     randomID,
		lifetime:  context.Background(),
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
	// outlives the HTTP request). Nil requires application-owned work or
	// explicit anonymous mode.
	Context context.Context
	// Node and Preset are what the request named (or what a re-adopted
	// download Job carries); the backend refines them through Progress.
	Node   string
	Preset string
	// Resource is the serving object a load job follows.
	Resource string
	Object   *backend.JobObject
	// Parameters distinguishes effective operation settings beyond placement.
	Parameters string
	// Scope separates backend instances and target namespaces.
	Scope string
	// ApplicationOwned identifies work started by the application reconciler.
	ApplicationOwned bool
	// Authorize checks current resource access before returning or cancelling a task.
	Authorize func(context.Context, Job) error
}

// Start deduplicates active tasks only within one principal, grant, backend
// instance and complete operation parameters.
func (m *Manager) Start(req StartRequest, fn RunFunc) (job Job, created bool, err error) {
	parent := req.Context
	if parent == nil {
		parent = context.Background()
	}
	if err := parent.Err(); err != nil {
		return Job{}, false, err
	}
	owner, err := callerPrincipal(parent)
	if err != nil {
		return Job{}, false, err
	}
	if owner == (principal{}) && !m.allowAnonymous && !req.ApplicationOwned {
		return Job{}, false, ErrIdentityRequired
	}
	key := operationKey{Owner: owner, Type: req.Type, Backend: req.Backend, Model: req.Model, Node: req.Node, Preset: req.Preset, Resource: req.Resource, Wire: req.Wire, Parameters: req.Parameters, Grant: grantKey(parent), Scope: req.Scope}
	if req.Object != nil {
		key.Object = *req.Object
	}
	initial := Job{Type: req.Type, Backend: req.Backend, Model: req.Model, Node: req.Node, Preset: req.Preset, Resource: req.Resource, Wire: req.Wire, Object: req.Object}
	if req.Authorize != nil {
		if err := req.Authorize(parent, initial); err != nil {
			return Job{}, false, err
		}
	}
	m.mu.Lock()
	if m.stopping || m.lifetime.Err() != nil {
		m.mu.Unlock()
		return Job{}, false, ErrShuttingDown
	}
	m.pruneLocked()
	for _, e := range m.jobs {
		if e.key == key && !e.job.Done() {
			existing, authorize := snapshot(e.job), e.authorize
			m.mu.Unlock()
			if authorize != nil {
				if err := authorize(parent, existing); err != nil {
					return Job{}, false, err
				}
			}
			return existing, false, nil
		}
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stopLifetime := context.AfterFunc(m.lifetime, cancel)
	e := &entry{
		owner: owner, key: key, authorize: req.Authorize,
		job: Job{
			Scope:       req.Scope,
			ID:          m.newID(),
			Type:        req.Type,
			Backend:     req.Backend,
			Model:       req.Model,
			Phase:       PhasePending,
			CreatedAt:   time.Now(),
			Wire:        req.Wire,
			Node:        req.Node,
			Preset:      req.Preset,
			Resource:    req.Resource,
			Object:      cloneObject(req.Object),
			RequestedBy: identity.Caller(parent),
		},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	m.jobs[e.job.ID] = e
	createdJob := snapshot(e.job)
	m.mu.Unlock()
	go func() { defer stopLifetime(); defer cancel(); m.run(ctx, e.job.ID, e.done, fn) }()
	return createdJob, true, nil
}

func (m *Manager) run(ctx context.Context, id string, done chan struct{}, fn RunFunc) {
	defer close(done)
	m.update(id, func(j *Job) {
		t := time.Now()
		j.Phase = PhaseRunning
		j.StartedAt = &t
	})
	report := func(p backend.Progress) {
		m.update(id, func(j *Job) {
			if p.Object != nil && j.Object == nil {
				j.Object = cloneObject(p.Object)
				j.Resource = p.Object.Name
			}
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
	var result any
	err := ctx.Err()
	if err == nil {
		result, err = fn(ctx, report)
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	m.update(id, func(j *Job) {
		t := time.Now()
		j.FinishedAt = &t
		switch {
		case err != nil && errors.Is(err, context.Canceled) && ctx.Err() != nil:
			j.Phase = PhaseCancelled
			j.Error = "cancelled"
		case err != nil:
			j.Phase = PhaseFailed
			j.Error = backend.PublicErrorMessage(err)
			// The public message omits upstream detail; operators need the cause.
			slog.Warn("job failed", "job", id, "type", j.Type, "backend", j.Backend, "model", j.Model, "error", err)
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
	return snapshot(e.job), nil
}

// List returns all jobs, newest first.
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	out := make([]Job, 0, len(m.jobs))
	for _, e := range m.jobs {
		out = append(out, snapshot(e.job))
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

// Await blocks until the job reached a terminal phase, or ctx ends.
func (m *Manager) Await(ctx context.Context, id string) (Job, error) {
	m.mu.Lock()
	e, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return Job{}, ErrNotFound
	}
	select {
	case <-e.done:
	case <-ctx.Done():
		return Job{}, ctx.Err()
	}
	return m.Get(id)
}

// Shutdown stops admission, cancels active jobs and waits within ctx's deadline.
// Jobs that ignore cancellation cannot extend the caller's shutdown budget.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.stopping = true
	done := make([]<-chan struct{}, 0, len(m.jobs))
	for _, entry := range m.jobs {
		entry.cancel()
		done = append(done, entry.done)
	}
	m.mu.Unlock()
	for _, finished := range done {
		select {
		case <-finished:
			continue
		default:
		}
		select {
		case <-finished:
		case <-ctx.Done():
			select {
			case <-finished:
				continue
			default:
				return ctx.Err()
			}
		}
	}
	return nil
}

// pruneLocked drops finished jobs beyond the retention window or count cap.
func (m *Manager) pruneLocked() {
	cutoff := time.Now().Add(-m.retention)
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
	return rand.Text()
}
