// Package service orchestrates the backend drivers, the job manager and the
// agent wiring behind one backend-agnostic API that both the REST and the MCP
// surfaces expose. One process runs one or several serving backends at once:
// every object the service returns says which backend it belongs to, every
// request may name one, and an unqualified model reference is resolved to the
// one backend that holds it.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/identity"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/wiring"
)

// ErrWiringDisabled is returned for wire operations when no Kubernetes access
// was configured.
var ErrWiringDisabled = fmt.Errorf("%w: agent wiring is disabled (no Kubernetes access)", backend.ErrUnsupported)

// Config tunes the service.
type Config struct {
	// AutoWire creates a ModelConfig when a pull completes or a model is
	// loaded, unless the request opts out.
	AutoWire bool
	// DefaultKeepAlive is passed to Load when the request has none.
	DefaultKeepAlive string
	// ReconcileInterval is how often Run re-checks served models for missing
	// ModelConfigs on ServeLifecycle backends (0 disables).
	ReconcileInterval time.Duration
	// CallerOnly means every Kubernetes call is made with the caller's token
	// (downstream OAuth) and the ServiceAccount holds no permissions: Run
	// neither adopts running downloads nor reconciles wiring, since both run
	// without a caller.
	CallerOnly bool
}

// BackendResponse is one backend's identity plus effective capabilities.
type BackendResponse struct {
	backend.Info
	Capabilities backend.Capabilities `json:"capabilities"`
	// Wiring describes where ModelConfigs are created, when wiring is enabled.
	Wiring *WiringInfo `json:"wiring,omitempty"`
	// Backends names every configured backend in order, the first being the
	// default backend. Only on the descriptor GET /api/v1/backend answers, so
	// a client of that route learns that there is more to read.
	Backends []backend.Name `json:"backends,omitempty"`
}

// WiringInfo describes the agent-wiring target.
type WiringInfo struct {
	Namespace  string `json:"namespace"`
	APIVersion string `json:"apiVersion,omitempty"`
	AutoWire   bool   `json:"autoWire"`
}

// ModelView is a downloaded model enriched with loaded state and wiring.
type ModelView struct {
	backend.Model
	Loaded      bool                   `json:"loaded"`
	Running     *backend.LoadedModel   `json:"running,omitempty"`
	ModelConfig *wiring.ModelConfigRef `json:"modelConfig,omitempty"`
}

// PullOptions describe an import request.
type PullOptions struct {
	// Backend names the driver to pull on; empty is the default backend (a
	// pull names a model that does not exist yet, so nothing else can pick).
	Backend string
	Model   string
	// Wire nil means the AutoWire default.
	Wire *bool
	// Preset / Node are kserve concerns (cache directory and target node).
	Preset string
	Node   string
}

// LoadOptions describe a load / serve request.
type LoadOptions struct {
	// Backend names the driver; empty resolves the model across backends.
	Backend   string
	Model     string
	KeepAlive string
	Preset    string
	Node      string
}

// Errors are the per-backend failures of an aggregate read, keyed by backend
// name: the items of the other backends were read.
type Errors map[backend.Name]string

// Service is the orchestration layer.
type Service struct {
	backends []backend.Backend
	byName   map[backend.Name]backend.Backend
	jobs     *jobs.Manager
	wirer    wiring.Wirer
	wiring   *WiringInfo
	cfg      Config
	log      *slog.Logger
}

// New builds a Service over the configured backends, in the operator's order:
// the first is the default backend. wirer may be nil (wiring disabled).
func New(backends []backend.Backend, jm *jobs.Manager, wirer wiring.Wirer, info *WiringInfo, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if len(backends) == 0 {
		panic("service.New: at least one backend is required")
	}
	if info != nil {
		info.AutoWire = cfg.AutoWire
	}
	s := &Service{backends: backends, byName: make(map[backend.Name]backend.Backend, len(backends)), jobs: jm, wirer: wirer, wiring: info, cfg: cfg, log: log}
	for _, b := range backends {
		if _, dup := s.byName[b.Name()]; dup {
			panic(fmt.Sprintf("service.New: backend %s configured twice", b.Name()))
		}
		s.byName[b.Name()] = b
	}
	return s
}

// Names lists the configured backends in order.
func (s *Service) Names() []backend.Name {
	names := make([]backend.Name, 0, len(s.backends))
	for _, b := range s.backends {
		names = append(names, b.Name())
	}
	return names
}

// Default is the first configured backend.
func (s *Service) Default() backend.Backend { return s.backends[0] }

// named returns the backend called name; "" is the default backend.
func (s *Service) named(name string) (backend.Backend, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return s.backends[0], nil
	}
	if b, ok := s.byName[backend.Name(name)]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("%w: unknown backend %q (configured: %s)", backend.ErrInvalid, name, joinNames(s.Names()))
}

// targets are the backends a read addresses: the named one, or all of them.
func (s *Service) targets(name string) ([]backend.Backend, error) {
	if strings.TrimSpace(name) == "" {
		return s.backends, nil
	}
	b, err := s.named(name)
	if err != nil {
		return nil, err
	}
	return []backend.Backend{b}, nil
}

// targetsWith narrows targets to the backends offering a capability; none
// left (or the named one lacking it) is ErrUnsupported for op.
func (s *Service) targetsWith(name string, has func(backend.Capabilities) bool, op string) ([]backend.Backend, error) {
	targets, err := s.targets(name)
	if err != nil {
		return nil, err
	}
	out := make([]backend.Backend, 0, len(targets))
	for _, b := range targets {
		if has(b.Capabilities()) {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", backend.ErrUnsupported, op)
	}
	return out, nil
}

func joinNames(names []backend.Name) string {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, string(n))
	}
	return strings.Join(parts, ", ")
}

// capabilities returns a backend's effective flags: the driver's plus Wire.
func (s *Service) capabilities(b backend.Backend) backend.Capabilities {
	caps := b.Capabilities()
	caps.Wire = s.wirer != nil
	return caps
}

func (s *Service) describe(ctx context.Context, b backend.Backend) BackendResponse {
	resp := BackendResponse{Info: b.Info(ctx), Capabilities: s.capabilities(b)}
	// Load applies cfg.DefaultKeepAlive before the driver sees the request, so
	// that is the default a client should report — not the driver's fallback.
	if resp.Loading.KeepAliveDefault != "" && s.cfg.DefaultKeepAlive != "" {
		resp.Loading.KeepAliveDefault = s.cfg.DefaultKeepAlive
	}
	if s.wirer != nil {
		resp.Wiring = s.wiring
	}
	return resp
}

// Backends describes every configured backend, in order.
func (s *Service) Backends(ctx context.Context) []BackendResponse {
	out := make([]BackendResponse, 0, len(s.backends))
	for _, b := range s.backends {
		out = append(out, s.describe(ctx, b))
	}
	return out
}

// Backend describes one backend — the named one, else the default — and names
// every configured backend next to it.
func (s *Service) Backend(ctx context.Context, name string) (BackendResponse, error) {
	b, err := s.named(name)
	if err != nil {
		return BackendResponse{}, err
	}
	resp := s.describe(ctx, b)
	resp.Backends = s.Names()
	return resp, nil
}

// Ready reports whether every backend answers.
func (s *Service) Ready(ctx context.Context) error {
	var problems []string
	for _, b := range s.backends {
		info := b.Info(ctx)
		if !info.Healthy {
			problems = append(problems, fmt.Sprintf("backend %s not healthy: %s", info.Backend, info.Message))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// serveLifecycle reports whether a backend's endpoints exist only while a
// model is loaded (wire on ready, unwire on unload, never wire after a pull).
func serveLifecycle(b backend.Backend) (backend.ServeLifecycle, bool) {
	sl, ok := b.(backend.ServeLifecycle)
	return sl, ok
}

// aggregateError is the failure of an aggregate read on every backend; it
// unwraps to the first failure so the API maps it as that backend would.
type aggregateError struct {
	msg   string
	first error
}

func (e *aggregateError) Error() string { return e.msg }
func (e *aggregateError) Unwrap() error { return e.first }

// aggregate reads from every target and collects per-backend failures. A
// single target's failure is returned as is; all targets failing is an
// aggregateError; otherwise the items read plus the failures of the rest.
func aggregate[T any](targets []backend.Backend, fetch func(backend.Backend) ([]T, error)) ([]T, Errors, error) {
	out := []T{}
	errs := Errors{}
	var first error
	var msgs []string
	for _, b := range targets {
		items, err := fetch(b)
		if err != nil {
			if first == nil {
				first = err
			}
			errs[b.Name()] = err.Error()
			msgs = append(msgs, fmt.Sprintf("%s: %v", b.Name(), err))
			continue
		}
		out = append(out, items...)
	}
	if len(targets) > 0 && len(errs) == len(targets) {
		if len(targets) == 1 {
			return nil, nil, first
		}
		return nil, nil, &aggregateError{msg: "every backend failed: " + strings.Join(msgs, "; "), first: first}
	}
	if len(errs) == 0 {
		errs = nil
	}
	return out, errs, nil
}

// resolve finds the backend holding ref: the named one, or — unqualified —
// the only one that has it. Several holding it is ErrConflict (name the
// backend); none is ErrNotFound unless a backend failed to answer, which is
// then the error.
func (s *Service) resolve(ctx context.Context, name, ref string) (backend.Backend, *backend.Model, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil, fmt.Errorf("%w: model reference is required", backend.ErrInvalid)
	}
	if strings.TrimSpace(name) != "" || len(s.backends) == 1 {
		b, err := s.named(name)
		if err != nil {
			return nil, nil, err
		}
		m, err := b.GetModel(ctx, ref)
		if err != nil {
			return nil, nil, err
		}
		return b, m, nil
	}
	type hit struct {
		b backend.Backend
		m *backend.Model
	}
	var hits []hit
	var failure error
	for _, b := range s.backends {
		m, err := b.GetModel(ctx, ref)
		switch {
		case err == nil:
			hits = append(hits, hit{b, m})
		case errors.Is(err, backend.ErrNotFound):
		case errors.Is(err, backend.ErrInvalid):
			return nil, nil, err
		default:
			if failure == nil {
				failure = fmt.Errorf("%s: %w", b.Name(), err)
			}
		}
	}
	switch len(hits) {
	case 1:
		return hits[0].b, hits[0].m, nil
	case 0:
		if failure != nil {
			return nil, nil, failure
		}
		return nil, nil, fmt.Errorf("%w: %s is on none of the backends (%s)", backend.ErrNotFound, ref, joinNames(s.Names()))
	default:
		names := make([]backend.Name, 0, len(hits))
		for _, h := range hits {
			names = append(names, h.b.Name())
		}
		return nil, nil, fmt.Errorf("%w: %s exists on %s; name the backend", backend.ErrConflict, ref, joinNames(names))
	}
}

// ListModels returns the downloaded models of the named backend, or of every
// backend, enriched with loaded state and ModelConfig references. Enrichment
// failures are logged, not fatal; a backend that fails to list is reported in
// Errors while the others' models are returned.
func (s *Service) ListModels(ctx context.Context, name string) ([]ModelView, Errors, error) {
	targets, err := s.targets(name)
	if err != nil {
		return nil, nil, err
	}
	return aggregate(targets, func(b backend.Backend) ([]ModelView, error) {
		models, err := b.ListModels(ctx)
		if err != nil {
			return nil, err
		}
		loaded := s.loadedIndex(ctx, b)
		wired := s.wiredIndex(ctx, b)
		out := make([]ModelView, 0, len(models))
		for _, m := range models {
			out = append(out, s.view(b, m, loaded, wired))
		}
		return out, nil
	})
}

// GetModel returns one enriched model, resolved to its backend.
func (s *Service) GetModel(ctx context.Context, name, ref string) (*ModelView, error) {
	b, m, err := s.resolve(ctx, name, ref)
	if err != nil {
		return nil, err
	}
	v := s.view(b, *m, s.loadedIndex(ctx, b), s.wiredIndex(ctx, b))
	return &v, nil
}

// ListLoaded returns the loaded/running models of the named backend, or of
// every backend that lists them.
func (s *Service) ListLoaded(ctx context.Context, name string) ([]backend.LoadedModel, Errors, error) {
	targets, err := s.targetsWith(name, func(c backend.Capabilities) bool { return c.LoadedModels }, "listing loaded models")
	if err != nil {
		return nil, nil, err
	}
	return aggregate(targets, func(b backend.Backend) ([]backend.LoadedModel, error) {
		loaded, err := b.ListLoaded(ctx)
		if err != nil {
			return nil, err
		}
		for i := range loaded {
			loaded[i].Backend = b.Name()
		}
		return loaded, nil
	})
}

// Pull starts (or joins) an import job on the named backend, else the default
// backend.
func (s *Service) Pull(ctx context.Context, opts PullOptions) (jobs.Job, bool, error) {
	b, err := s.named(opts.Backend)
	if err != nil {
		return jobs.Job{}, false, err
	}
	if !b.Capabilities().Pull {
		return jobs.Job{}, false, fmt.Errorf("%w: pull on %s", backend.ErrUnsupported, b.Name())
	}
	ref := strings.TrimSpace(opts.Model)
	if ref == "" {
		return jobs.Job{}, false, fmt.Errorf("%w: model reference is required", backend.ErrInvalid)
	}
	_, servesOnLoad := serveLifecycle(b)
	doWire := s.cfg.AutoWire && s.wirer != nil && !servesOnLoad
	if opts.Wire != nil {
		doWire = *opts.Wire
	}
	if doWire && s.wirer == nil {
		return jobs.Job{}, false, ErrWiringDisabled
	}
	if doWire && servesOnLoad {
		return jobs.Job{}, false, fmt.Errorf("%w: models on %s are wired when loaded (served), not when pulled", backend.ErrInvalid, b.Name())
	}
	req := backend.PullRequest{Ref: ref, Preset: strings.TrimSpace(opts.Preset), Node: strings.TrimSpace(opts.Node)}
	job, created := s.startPull(ctx, b, req, doWire)
	if created {
		s.log.Info("pull started", "backend", b.Name(), "model", ref, "job", job.ID, "wire", doWire, "preset", req.Preset, "node", req.Node, identity.LogAttr(ctx))
	}
	return job, created, nil
}

func (s *Service) startPull(ctx context.Context, b backend.Backend, req backend.PullRequest, doWire bool) (jobs.Job, bool) {
	return s.jobs.Start(jobs.StartRequest{Type: jobs.TypePull, Backend: b.Name(), Model: req.Ref, Wire: doWire, Node: req.Node, Preset: req.Preset, Context: ctx},
		func(jobCtx context.Context, report func(backend.Progress)) (any, error) {
			if err := b.Pull(jobCtx, req, report); err != nil {
				return nil, err
			}
			if !doWire {
				return nil, nil
			}
			mcRef, err := s.wireModel(jobCtx, b, req.Ref)
			if err != nil {
				return nil, fmt.Errorf("pulled %s but wiring failed: %w", req.Ref, err)
			}
			return mcRef, nil
		})
}

// Load loads/serves a model. With AutoWire the ModelConfig is ensured right
// away, or — on ServeLifecycle backends — by a `load` job once the served
// model is ready.
func (s *Service) Load(ctx context.Context, opts LoadOptions) (*ModelView, error) {
	if strings.TrimSpace(opts.Backend) != "" {
		b, err := s.named(opts.Backend)
		if err != nil {
			return nil, err
		}
		if !b.Capabilities().Load {
			return nil, fmt.Errorf("%w: load on %s", backend.ErrUnsupported, b.Name())
		}
	}
	name := strings.TrimSpace(opts.Model)
	if name == "" && opts.Preset != "" {
		name = strings.TrimSpace(opts.Preset)
	}
	b, m, err := s.resolve(ctx, opts.Backend, name)
	if err != nil {
		return nil, err
	}
	if !b.Capabilities().Load {
		return nil, fmt.Errorf("%w: load on %s", backend.ErrUnsupported, b.Name())
	}
	keepAlive := opts.KeepAlive
	if keepAlive == "" {
		keepAlive = s.cfg.DefaultKeepAlive
	}
	req := backend.LoadRequest{Name: m.Name, KeepAlive: keepAlive, Preset: strings.TrimSpace(opts.Preset), Node: strings.TrimSpace(opts.Node)}
	if req.Preset == "" && m.Preset != "" {
		req.Preset = m.Preset
	}
	if err := b.Load(ctx, req); err != nil {
		return nil, err
	}
	s.log.Info("model loaded", "backend", b.Name(), "model", m.Name, "keepAlive", keepAlive, "preset", req.Preset, "node", req.Node, identity.LogAttr(ctx))
	if s.cfg.AutoWire && s.wirer != nil {
		if sl, ok := serveLifecycle(b); ok {
			s.startLoadJob(ctx, b, sl, m.Name)
		} else if _, err := s.wireModel(ctx, b, m.Name); err != nil {
			s.log.Warn("auto-wire after load failed", "backend", b.Name(), "model", m.Name, "error", err)
		}
	}
	return s.GetModel(ctx, string(b.Name()), m.Name)
}

// startLoadJob follows a served model to readiness and wires it.
func (s *Service) startLoadJob(ctx context.Context, b backend.Backend, sl backend.ServeLifecycle, model string) {
	job, created := s.jobs.Start(jobs.StartRequest{Type: jobs.TypeLoad, Backend: b.Name(), Model: model, Wire: true, Context: ctx},
		func(jobCtx context.Context, report func(backend.Progress)) (any, error) {
			report(backend.Progress{Status: "waiting for the served model to become ready"})
			if err := sl.WaitReady(jobCtx, model); err != nil {
				return nil, err
			}
			report(backend.Progress{Status: "ready; wiring into kagent"})
			ref, err := s.wireModel(jobCtx, b, model)
			if err != nil {
				return nil, fmt.Errorf("%s is ready but wiring failed: %w", model, err)
			}
			report(backend.Progress{Status: "wired"})
			return ref, nil
		})
	if created {
		s.log.Info("load job started", "backend", b.Name(), "model", model, "job", job.ID, identity.LogAttr(ctx))
	}
}

// Unload evicts a model and reports the backend it was on. On ServeLifecycle
// backends the ModelConfig goes with the endpoint.
func (s *Service) Unload(ctx context.Context, name, ref string) (backend.Name, error) {
	b, m, err := s.resolve(ctx, name, ref)
	if err != nil {
		return "", err
	}
	if !b.Capabilities().Unload {
		return b.Name(), fmt.Errorf("%w: unload on %s", backend.ErrUnsupported, b.Name())
	}
	if err := b.Unload(ctx, m.Name); err != nil {
		return b.Name(), err
	}
	s.log.Info("model unloaded", "backend", b.Name(), "model", m.Name, identity.LogAttr(ctx))
	if _, ok := serveLifecycle(b); ok && s.wirer != nil {
		if err := s.wirer.Remove(ctx, b.Name(), m.Name); err != nil {
			s.log.Warn("unwire after unload failed", "backend", b.Name(), "model", m.Name, "error", err)
		} else {
			s.log.Info("model unwired", "backend", b.Name(), "model", m.Name, identity.LogAttr(ctx))
		}
	}
	return b.Name(), nil
}

// Delete removes a downloaded model, unwiring it first when requested, and
// reports the backend it was on.
func (s *Service) Delete(ctx context.Context, name, ref string, unwire bool) (backend.Name, error) {
	b, m, err := s.resolve(ctx, name, ref)
	if err != nil {
		return "", err
	}
	if !b.Capabilities().Delete {
		return b.Name(), fmt.Errorf("%w: delete on %s", backend.ErrUnsupported, b.Name())
	}
	if unwire && s.wirer != nil {
		if err := s.wirer.Remove(ctx, b.Name(), m.Name); err != nil {
			return b.Name(), fmt.Errorf("unwire %s: %w", m.Name, err)
		}
	}
	if err := b.Delete(ctx, m.Name); err != nil {
		return b.Name(), err
	}
	s.log.Info("model deleted", "backend", b.Name(), "model", m.Name, "unwired", unwire && s.wirer != nil, identity.LogAttr(ctx))
	return b.Name(), nil
}

// Wire creates the ModelConfig for an existing model.
func (s *Service) Wire(ctx context.Context, name, ref string) (*wiring.ModelConfigRef, error) {
	if s.wirer == nil {
		return nil, ErrWiringDisabled
	}
	b, m, err := s.resolve(ctx, name, ref)
	if err != nil {
		return nil, err
	}
	return s.wireModel(ctx, b, m.Name)
}

// Unwire removes the ModelConfig for a model (which need not exist anymore)
// and reports the backend it belonged to. Unqualified, the managed
// ModelConfigs are consulted first — the model may be gone from its backend.
func (s *Service) Unwire(ctx context.Context, name, ref string) (backend.Name, error) {
	if s.wirer == nil {
		return "", ErrWiringDisabled
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("%w: model name is required", backend.ErrInvalid)
	}
	var b backend.Backend
	if strings.TrimSpace(name) != "" || len(s.backends) == 1 {
		var err error
		if b, err = s.named(name); err != nil {
			return "", err
		}
	} else {
		owners, err := s.wiredBackends(ctx, ref)
		if err != nil {
			return "", err
		}
		switch len(owners) {
		case 1:
			b = s.byName[owners[0]]
		case 0:
		default:
			return "", fmt.Errorf("%w: %s is wired on %s; name the backend", backend.ErrConflict, ref, joinNames(owners))
		}
		if b == nil {
			resolved, _, err := s.resolve(ctx, "", ref)
			switch {
			case err == nil:
				b = resolved
			case errors.Is(err, backend.ErrNotFound):
				// Nothing wired, nothing downloaded: absent counts as success.
				return "", nil
			default:
				return "", err
			}
		}
	}
	if m, err := b.GetModel(ctx, ref); err == nil {
		ref = m.Name
	}
	if err := s.wirer.Remove(ctx, b.Name(), ref); err != nil {
		return b.Name(), err
	}
	s.log.Info("model unwired", "backend", b.Name(), "model", ref, identity.LogAttr(ctx))
	return b.Name(), nil
}

// wiredBackends lists the configured backends holding a managed ModelConfig
// for ref (a label-less ModelConfig counts for the default backend).
func (s *Service) wiredBackends(ctx context.Context, ref string) ([]backend.Name, error) {
	refs, err := s.wirer.List(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[backend.Name]bool{}
	var out []backend.Name
	for _, r := range refs {
		if r.Model != ref {
			continue
		}
		owner := r.Backend
		if _, ok := s.byName[owner]; !ok {
			owner = s.backends[0].Name()
		}
		if !seen[owner] {
			seen[owner] = true
			out = append(out, owner)
		}
	}
	return out, nil
}

// Presets lists the serving presets of the named backend, or of every backend
// offering them (capability presets).
func (s *Service) Presets(ctx context.Context, name string) ([]backend.Preset, Errors, error) {
	targets, err := s.targetsWith(name, func(c backend.Capabilities) bool { return c.Presets }, "presets")
	if err != nil {
		return nil, nil, err
	}
	return aggregate(targets, func(b backend.Backend) ([]backend.Preset, error) {
		pl, ok := b.(backend.PresetLister)
		if !ok {
			return nil, fmt.Errorf("%w: presets on %s", backend.ErrUnsupported, b.Name())
		}
		presets, err := pl.ListPresets(ctx)
		if err != nil {
			return nil, err
		}
		for i := range presets {
			presets[i].Backend = b.Name()
		}
		return presets, nil
	})
}

// Search proxies the model hub of the named backend, or of every backend
// offering one (capability search).
func (s *Service) Search(ctx context.Context, name, query string, limit int) ([]backend.SearchResult, Errors, error) {
	targets, err := s.targetsWith(name, func(c backend.Capabilities) bool { return c.Search }, "search")
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil, fmt.Errorf("%w: query is required", backend.ErrInvalid)
	}
	return aggregate(targets, func(b backend.Backend) ([]backend.SearchResult, error) {
		sr, ok := b.(backend.Searcher)
		if !ok {
			return nil, fmt.Errorf("%w: search on %s", backend.ErrUnsupported, b.Name())
		}
		hits, err := sr.Search(ctx, query, limit)
		if err != nil {
			return nil, err
		}
		if hits == nil {
			hits = []backend.SearchResult{}
		}
		return hits, nil
	})
}

// FitCheck sizes a model against node budgets (capability fitCheck) on the
// named backend, else on the one backend that offers fit checks.
func (s *Service) FitCheck(ctx context.Context, name string, req backend.FitRequest) (*backend.FitResult, error) {
	targets, err := s.targetsWith(name, func(c backend.Capabilities) bool { return c.FitCheck }, "fit check")
	if err != nil {
		return nil, err
	}
	if len(targets) > 1 {
		return nil, fmt.Errorf("%w: several backends offer fit checks (%s); name the backend", backend.ErrConflict, joinNames(namesOf(targets)))
	}
	b := targets[0]
	fc, ok := b.(backend.FitChecker)
	if !ok {
		return nil, fmt.Errorf("%w: fit check on %s", backend.ErrUnsupported, b.Name())
	}
	if strings.TrimSpace(req.Model) == "" && strings.TrimSpace(req.Preset) == "" {
		return nil, fmt.Errorf("%w: model or preset is required", backend.ErrInvalid)
	}
	res, err := fc.FitCheck(ctx, req)
	if err != nil {
		return nil, err
	}
	res.Backend = b.Name()
	return res, nil
}

// Nodes lists node budgets and caches (capability nodeInventory) of the named
// backend, or of every backend reporting nodes.
func (s *Service) Nodes(ctx context.Context, name string) ([]backend.NodeInfo, Errors, error) {
	targets, err := s.targetsWith(name, func(c backend.Capabilities) bool { return c.NodeInventory }, "node inventory")
	if err != nil {
		return nil, nil, err
	}
	return aggregate(targets, func(b backend.Backend) ([]backend.NodeInfo, error) {
		nl, ok := b.(backend.NodeLister)
		if !ok {
			return nil, fmt.Errorf("%w: node inventory on %s", backend.ErrUnsupported, b.Name())
		}
		nodes, err := nl.ListNodes(ctx)
		if err != nil {
			return nil, err
		}
		for i := range nodes {
			nodes[i].Backend = b.Name()
		}
		return nodes, nil
	})
}

func namesOf(bs []backend.Backend) []backend.Name {
	out := make([]backend.Name, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Name())
	}
	return out
}

// Jobs lists jobs, newest first, of the named backend or of all.
func (s *Service) Jobs(name string) ([]jobs.Job, error) {
	all := s.jobs.List()
	if strings.TrimSpace(name) == "" {
		return all, nil
	}
	b, err := s.named(name)
	if err != nil {
		return nil, err
	}
	out := make([]jobs.Job, 0, len(all))
	for _, j := range all {
		if j.Backend == b.Name() {
			out = append(out, j)
		}
	}
	return out, nil
}

// Job returns one job.
func (s *Service) Job(id string) (jobs.Job, error) { return s.jobs.Get(id) }

// CancelJob cancels a running job.
func (s *Service) CancelJob(id string) (jobs.Job, error) { return s.jobs.Cancel(id) }

// Run performs the background duties until ctx ends: adopting pulls that
// survived a restart (PullAdopter backends) and, on ServeLifecycle backends,
// wiring served models that became ready without a load job watching them
// (model-manager restarted, or the InferenceService was created by someone
// else with the preset label).
func (s *Service) Run(ctx context.Context) {
	if s.cfg.CallerOnly {
		s.log.Info("caller-only mode: no download adoption and no wiring reconciler (every Kubernetes call carries the caller's token; the ServiceAccount holds no permissions)")
		return
	}
	for _, b := range s.backends {
		s.adoptPulls(ctx, b)
	}
	var lifecycle []backend.Backend
	for _, b := range s.backends {
		if _, ok := serveLifecycle(b); ok {
			lifecycle = append(lifecycle, b)
		}
	}
	if len(lifecycle) == 0 || s.cfg.ReconcileInterval <= 0 || s.wirer == nil || !s.cfg.AutoWire {
		return
	}
	ticker := time.NewTicker(s.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		for _, b := range lifecycle {
			s.reconcileWiring(ctx, b)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) adoptPulls(ctx context.Context, b backend.Backend) {
	pa, ok := b.(backend.PullAdopter)
	if !ok {
		return
	}
	pulls, err := pa.RunningPulls(ctx)
	if err != nil {
		s.log.Warn("listing running pulls for adoption failed", "backend", b.Name(), "error", err)
		return
	}
	for _, req := range pulls {
		job, created := s.startPull(ctx, b, req, false)
		if created {
			s.log.Info("adopted running pull", "backend", b.Name(), "model", req.Ref, "job", job.ID)
		}
	}
}

// reconcileWiring ensures a ModelConfig for every ready served model of b that
// has none. It never touches ModelConfigs of models that are not served:
// unload removes those explicitly.
func (s *Service) reconcileWiring(ctx context.Context, b backend.Backend) {
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	loaded, err := b.ListLoaded(rctx)
	if err != nil {
		s.log.Warn("reconcile: listing served models failed", "backend", b.Name(), "error", err)
		return
	}
	wired := s.wiredIndex(rctx, b)
	for _, l := range loaded {
		if l.Status != "Ready" {
			continue
		}
		if wired.has(l.Name) {
			continue
		}
		if _, ok := wired.byEndpoint[endpointKey(l.Endpoint, l.Resource)]; ok {
			continue // wired by someone else (the portal)
		}
		if s.hasActiveJob(jobs.TypeLoad, b.Name(), l.Name) {
			continue
		}
		if _, err := s.wireModel(rctx, b, l.Name); err != nil {
			s.log.Warn("reconcile: wiring served model failed", "backend", b.Name(), "model", l.Name, "error", err)
		}
	}
}

func (s *Service) hasActiveJob(t jobs.Type, b backend.Name, model string) bool {
	for _, j := range s.jobs.List() {
		if j.Type == t && j.Backend == b && j.Model == model && !j.Done() {
			return true
		}
	}
	return false
}

func (s *Service) wireModel(ctx context.Context, b backend.Backend, model string) (*wiring.ModelConfigRef, error) {
	// Resolve the canonical name so "smollm2:135m" and "smollm2:135m" pulled
	// as "smollm2" end up in one ModelConfig.
	if m, err := b.GetModel(ctx, model); err == nil {
		model = m.Name
	}
	ep := b.AgentEndpoint(model)
	ep.Backend = b.Name()
	// On serve-lifecycle backends the endpoint identifies the served model:
	// a ModelConfig someone else created for it (the portal's serve flow)
	// counts as wired — never a duplicate, never touched.
	if _, ok := serveLifecycle(b); ok {
		if existing := s.foreignForEndpoint(ctx, ep); existing != nil {
			s.log.Info("model already wired by another owner", "backend", b.Name(), "model", model, "modelConfig", existing.Namespace+"/"+existing.Name)
			return existing, nil
		}
	}
	ref, err := s.wirer.Ensure(ctx, model, ep)
	if err != nil {
		return nil, err
	}
	s.log.Info("model wired", "backend", b.Name(), "model", model, "modelConfig", ref.Namespace+"/"+ref.Name)
	return ref, nil
}

// foreignForEndpoint finds a ModelConfig not created by model-manager that
// already points at the endpoint (same host, same served model name).
func (s *Service) foreignForEndpoint(ctx context.Context, ep backend.AgentEndpoint) *wiring.ModelConfigRef {
	all, err := s.wirer.ListAll(ctx)
	if err != nil {
		s.log.Warn("listing ModelConfigs for dedupe failed", "error", err)
		return nil
	}
	target := ep.BaseURL
	if target == "" {
		target = ep.Host
	}
	for i := range all {
		r := all[i]
		if !r.Managed && sameEndpoint(r.Endpoint, target) && r.ProviderModel == ep.Model {
			return &r
		}
	}
	return nil
}

// sameEndpoint compares two provider endpoints by host name (scheme, port
// and path such as /v1 do not matter: one predictor Service, one model).
func sameEndpoint(a, b string) bool {
	return endpointHost(a) != "" && endpointHost(a) == endpointHost(b)
}

func endpointHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return strings.ToLower(raw)
	}
	return strings.ToLower(u.Hostname())
}

// endpointKey indexes ModelConfigs by served endpoint and provider model.
func endpointKey(endpoint, providerModel string) string {
	return endpointHost(endpoint) + "|" + providerModel
}

func (s *Service) loadedIndex(ctx context.Context, b backend.Backend) map[string]backend.LoadedModel {
	idx := map[string]backend.LoadedModel{}
	if !b.Capabilities().LoadedModels {
		return idx
	}
	loaded, err := b.ListLoaded(ctx)
	if err != nil {
		s.log.Warn("listing loaded models failed", "backend", b.Name(), "error", err)
		return idx
	}
	for _, l := range loaded {
		l.Backend = b.Name()
		idx[l.Name] = l
		if l.Resource != "" {
			idx[l.Resource] = l
		}
	}
	return idx
}

// wiredView is what the model views join against: model-manager's own
// ModelConfigs of one backend by model reference and, on serve-lifecycle
// backends, every ModelConfig by served endpoint so a portal-wired model
// shows its config.
type wiredView struct {
	byModel    map[string]wiring.ModelConfigRef
	byEndpoint map[string]wiring.ModelConfigRef
}

func (w wiredView) has(model string) bool {
	_, ok := w.byModel[model]
	return ok
}

func (s *Service) wiredIndex(ctx context.Context, b backend.Backend) wiredView {
	out := wiredView{}
	if s.wirer == nil {
		return out
	}
	refs, err := s.wirer.List(ctx)
	if err != nil {
		s.log.Warn("listing ModelConfigs failed", "error", err)
		return out
	}
	out.byModel = make(map[string]wiring.ModelConfigRef, len(refs))
	for _, r := range refs {
		// A ModelConfig without the backend label (written before the label
		// existed) counts for every backend.
		if r.Backend != "" && r.Backend != b.Name() {
			continue
		}
		out.byModel[r.Model] = r
	}
	if _, ok := serveLifecycle(b); !ok {
		return out
	}
	all, err := s.wirer.ListAll(ctx)
	if err != nil {
		s.log.Warn("listing all ModelConfigs failed", "error", err)
		return out
	}
	out.byEndpoint = map[string]wiring.ModelConfigRef{}
	for _, r := range all {
		if r.Endpoint != "" && r.ProviderModel != "" {
			out.byEndpoint[endpointKey(r.Endpoint, r.ProviderModel)] = r
		}
	}
	return out
}

func (s *Service) view(b backend.Backend, m backend.Model, loaded map[string]backend.LoadedModel, wired wiredView) ModelView {
	m.Backend = b.Name()
	v := ModelView{Model: m}
	l, ok := loaded[m.Name]
	if !ok && m.Path != "" {
		l, ok = loaded[m.Path]
	}
	if ok {
		v.Loaded = true
		lc := l
		lc.Backend = b.Name()
		v.Running = &lc
	}
	if mc, ok := wired.byModel[m.Name]; ok {
		mcc := mc
		v.ModelConfig = &mcc
	} else if v.Running != nil && wired.byEndpoint != nil {
		if mc, ok := wired.byEndpoint[endpointKey(v.Running.Endpoint, v.Running.Resource)]; ok {
			mcc := mc
			v.ModelConfig = &mcc
		}
	}
	return v
}

// IsNotFound reports whether err is a not-found from the backend or jobs.
func IsNotFound(err error) bool {
	return errors.Is(err, backend.ErrNotFound) || errors.Is(err, jobs.ErrNotFound)
}
