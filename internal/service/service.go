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
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// Source says where the backend came from: static (--backends), person
	// or cluster-manager (a registered document).
	Source       string               `json:"source"`
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
	// Fit is the verdict a load judged the model by (backend.Server); only
	// a load's answer carries it.
	Fit *backend.FitResult `json:"fit,omitempty"`
	// Wiring is what a load did about the ModelConfig on a serve-lifecycle
	// backend — created in the same call, before the model is ready; only a
	// load's answer carries it.
	Wiring *WiringResult `json:"wiring,omitempty"`
}

// LoadedView is a loaded / served model as the loaded list answers it: the
// backend's entry plus, on a serve-lifecycle backend, its kagent ModelConfig
// when one exists and — when this read wired a model that had none — what
// the read did.
type LoadedView struct {
	backend.LoadedModel
	ModelConfig *wiring.ModelConfigRef `json:"modelConfig,omitempty"`
	Wiring      *WiringResult          `json:"wiring,omitempty"`
}

// WiringResult says what a call did about a served model's ModelConfig, so a
// wiring the caller did not ask for by name is never silent: Wired with the
// ModelConfig and the occasion as Reason, or not wired and why.
type WiringResult struct {
	Wired  bool   `json:"wired"`
	Reason string `json:"reason"`
	// ModelConfig is the ModelConfig the model is wired to — model-manager's
	// own, or the one someone else already pointed at the model.
	ModelConfig *wiring.ModelConfigRef `json:"modelConfig,omitempty"`
	Error       string                 `json:"error,omitempty"`
}

// The occasions a WiringResult names.
const (
	// WiredOnLoad: load created (or refreshed) the ModelConfig in the same
	// call, before the model is ready.
	WiredOnLoad = "wired on load"
	// WiredOnRead: a read found a served model model-manager manages without
	// a ModelConfig and wired it as the caller.
	WiredOnRead = "wired on read"
	// AlreadyWired: a ModelConfig someone else created already points at the
	// served model; it is reported, never duplicated.
	AlreadyWired = "already wired"
	// WiringFailed: the ModelConfig could not be written; Error says why.
	WiringFailed = "wiring failed"
)

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
	mu sync.RWMutex
	// backends holds the static backends in the operator's order, then the
	// registered ones sorted by name; byName indexes them; sources records
	// where each came from (static, person, cluster-manager).
	backends []backend.Backend
	byName   map[backend.Name]backend.Backend
	sources  map[backend.Name]string
	problems map[string]string // invalid backend documents by ConfigMap name
	static   int               // how many of backends are static

	jobs   *jobs.Manager
	wirer  wiring.Wirer
	wiring *WiringInfo
	cfg    Config
	log    *slog.Logger
}

// New builds a Service over the static backends, in the operator's order:
// the first is the default backend. The list may be empty — backends are
// then registered at runtime (Register) and every backend-scoped call
// answers backend.ErrNoBackend until one is. wirer may be nil (wiring
// disabled).
func New(backends []backend.Backend, jm *jobs.Manager, wirer wiring.Wirer, info *WiringInfo, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if info != nil {
		info.AutoWire = cfg.AutoWire
	}
	s := &Service{byName: make(map[backend.Name]backend.Backend, len(backends)), sources: map[backend.Name]string{}, problems: map[string]string{}, jobs: jm, wirer: wirer, wiring: info, cfg: cfg, log: log}
	for _, b := range backends {
		if _, dup := s.byName[b.Name()]; dup {
			panic(fmt.Sprintf("service.New: backend %s configured twice", b.Name()))
		}
		s.backends = append(s.backends, b)
		s.byName[b.Name()] = b
		s.sources[b.Name()] = backend.SourceStatic
	}
	s.static = len(s.backends)
	return s
}

// ErrStaticBackend: a document names a kind --backends already configures.
var ErrStaticBackend = errors.New("configured statically by --backends; remove it from the chart values to register it at runtime")

// Register adds a backend registered at runtime (source person or
// cluster-manager); a registered backend of the same name is replaced, a
// static one is refused with ErrStaticBackend.
func (s *Service) Register(b backend.Backend, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registerLocked(b, source)
}

// RegisterDocument registers the backend a document builds and clears the
// document's report in the same step, so no reader sees the backend
// registered while its ConfigMap is still listed as invalid — the window in
// which list_backends answered both for a document that had just been fixed
// (giantswarm/model-manager#101). A refused registration leaves the report
// untouched; the caller records the refusal.
func (s *Service) RegisterDocument(b backend.Backend, source, configMap string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.registerLocked(b, source); err != nil {
		return err
	}
	delete(s.problems, configMap)
	return nil
}

func (s *Service) registerLocked(b backend.Backend, source string) error {
	name := b.Name()
	if s.sources[name] == backend.SourceStatic {
		return fmt.Errorf("%w: backend %s is %w", backend.ErrConflict, name, ErrStaticBackend)
	}
	s.byName[name] = b
	s.sources[name] = source
	s.rebuildLocked()
	return nil
}

// Deregister drops a registered backend; a static or unknown name is a no-op
// returning false.
func (s *Service) Deregister(name backend.Name) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src, ok := s.sources[name]; !ok || src == backend.SourceStatic {
		return false
	}
	delete(s.byName, name)
	delete(s.sources, name)
	s.rebuildLocked()
	return true
}

// rebuildLocked recomputes the ordered list: static first (operator order,
// kept in place), then the registered backends sorted by name.
func (s *Service) rebuildLocked() {
	static := s.backends[:s.static]
	registered := make([]backend.Backend, 0, len(s.byName)-s.static)
	for name, b := range s.byName {
		if s.sources[name] != backend.SourceStatic {
			registered = append(registered, b)
		}
	}
	sort.Slice(registered, func(i, j int) bool { return registered[i].Name() < registered[j].Name() })
	s.backends = append(static[:len(static):len(static)], registered...)
}

// ReportDocument records (or, with an empty problem, clears) an invalid
// backend document by ConfigMap name; list_backends reports them.
func (s *Service) ReportDocument(configMap, problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if problem == "" {
		delete(s.problems, configMap)
		return
	}
	s.problems[configMap] = problem
}

// InvalidDocument is a backend document that failed the read-time schema.
type InvalidDocument struct {
	ConfigMap string `json:"configMap"`
	Error     string `json:"error"`
}

// InvalidDocuments lists the documents that were reported, by ConfigMap name.
func (s *Service) InvalidDocuments() []InvalidDocument {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]InvalidDocument, 0, len(s.problems))
	for cm, p := range s.problems {
		out = append(out, InvalidDocument{ConfigMap: cm, Error: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConfigMap < out[j].ConfigMap })
	return out
}

// all is a snapshot of the backends in order.
func (s *Service) all() []backend.Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.backends
}

// lookup returns the backend called name, if any.
func (s *Service) lookup(name backend.Name) (backend.Backend, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.byName[name]
	return b, ok
}

// Has reports whether a backend called name is configured, and its source.
func (s *Service) Has(name backend.Name) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src, ok := s.sources[name]
	return src, ok
}

// Source says where a backend came from: static, person or cluster-manager.
func (s *Service) Source(name backend.Name) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sources[name]
}

// Wiring describes where ModelConfigs are created, nil when agent wiring is
// disabled.
func (s *Service) Wiring() *WiringInfo { return s.wiring }

// Names lists the configured backends in order.
func (s *Service) Names() []backend.Name {
	all := s.all()
	names := make([]backend.Name, 0, len(all))
	for _, b := range all {
		names = append(names, b.Name())
	}
	return names
}

// Default is the first configured backend, nil when none is.
func (s *Service) Default() backend.Backend {
	all := s.all()
	if len(all) == 0 {
		return nil
	}
	return all[0]
}

// named returns the backend called name; "" is the default backend.
func (s *Service) named(name string) (backend.Backend, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		if b := s.Default(); b != nil {
			return b, nil
		}
		return nil, backend.ErrNoBackend
	}
	if b, ok := s.lookup(backend.Name(name)); ok {
		return b, nil
	}
	if len(s.all()) == 0 {
		return nil, backend.ErrNoBackend
	}
	return nil, fmt.Errorf("%w: unknown backend %q (configured: %s)", backend.ErrInvalid, name, joinNames(s.Names()))
}

// targets are the backends a read addresses: the named one, or all of them.
func (s *Service) targets(name string) ([]backend.Backend, error) {
	if strings.TrimSpace(name) == "" {
		all := s.all()
		if len(all) == 0 {
			return nil, backend.ErrNoBackend
		}
		return all, nil
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
	resp := BackendResponse{Info: b.Info(ctx), Source: s.Source(b.Name()), Capabilities: s.capabilities(b)}
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
	all := s.all()
	out := make([]BackendResponse, 0, len(all))
	for _, b := range all {
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
	for _, b := range s.all() {
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
	if strings.TrimSpace(name) != "" || len(s.all()) == 1 {
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
	for _, b := range s.all() {
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
// every backend that lists them. On a serve-lifecycle backend each entry
// carries its ModelConfig, and a served model model-manager manages that has
// none is wired by this read, as the caller (loadedViews).
func (s *Service) ListLoaded(ctx context.Context, name string) ([]LoadedView, Errors, error) {
	targets, err := s.targetsWith(name, func(c backend.Capabilities) bool { return c.LoadedModels }, "listing loaded models")
	if err != nil {
		return nil, nil, err
	}
	return aggregate(targets, func(b backend.Backend) ([]LoadedView, error) {
		loaded, err := b.ListLoaded(ctx)
		if err != nil {
			return nil, err
		}
		return s.loadedViews(ctx, b, loaded), nil
	})
}

// loadedViews joins b's loaded models with their ModelConfigs and, on a
// serve-lifecycle backend with auto-wiring, wires — as the caller — every
// served model model-manager manages (its managed-by label) that has none.
// In caller-only mode no reconciler runs, and the load job that wired at
// readiness died with every model-manager restart during a cold start,
// leaving a served model no agent could use (giantswarm/model-manager#115);
// the caller's reads are what runs with a caller, so they mend it and say
// so. Objects someone else manages are theirs to wire; an object on its way
// out is not re-wired behind the unload that removed its ModelConfig.
func (s *Service) loadedViews(ctx context.Context, b backend.Backend, loaded []backend.LoadedModel) []LoadedView {
	out := make([]LoadedView, 0, len(loaded))
	_, serves := serveLifecycle(b)
	joins := serves && s.wirer != nil
	var wired wiredView
	if joins {
		wired = s.wiredIndex(ctx, b)
	}
	for _, l := range loaded {
		l.Backend = b.Name()
		v := LoadedView{LoadedModel: l}
		if joins {
			if mc, ok := wired.lookup(l.Name, &l); ok {
				v.ModelConfig = &mc
			} else if s.cfg.AutoWire && l.ManagedBy == wiring.ManagedByValue && !terminating(l) {
				v.Wiring = s.wire(ctx, b, l.Name, WiredOnRead)
				v.ModelConfig = v.Wiring.ModelConfig
			}
		}
		out = append(out, v)
	}
	return out
}

// terminating reports whether a served model's object is being deleted.
func terminating(l backend.LoadedModel) bool {
	return l.Phase == backend.PhaseTerminating || l.Status == "Terminating"
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
			mcRef, err := s.wireModel(jobCtx, b, req.Ref, backend.WireOptions{})
			if err != nil {
				return nil, fmt.Errorf("pulled %s but wiring failed: %w", req.Ref, err)
			}
			return mcRef, nil
		})
}

// Load loads/serves a model. With AutoWire the ModelConfig is ensured in the
// same call — on ServeLifecycle backends too, at the address the served
// model will answer on, before it is ready; there a `load` job then follows
// the model to readiness and refreshes the ModelConfig from the address the
// backend published.
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
	// Loaded at the context window agents will ask for, so their first turn
	// does not reload the model at another size.
	req.ContextLength = b.AgentEndpoint(m.Name).FitTo(*m).ContextLength
	if req.Preset == "" && m.Preset != "" {
		req.Preset = m.Preset
	}
	var fit *backend.FitResult
	if srv, ok := b.(backend.Server); ok {
		res, err := srv.Serve(ctx, req)
		if err != nil {
			return nil, err
		}
		if res != nil {
			fit = res.Fit
		}
	} else if err := b.Load(ctx, req); err != nil {
		return nil, err
	}
	s.log.Info("model loaded", "backend", b.Name(), "model", m.Name, "keepAlive", keepAlive, "preset", req.Preset, "node", req.Node, identity.LogAttr(ctx))
	if s.cfg.AutoWire && s.wirer != nil {
		if sl, ok := serveLifecycle(b); ok {
			// The answer names the serving object the load created
			// (running.resource and running.kind, with its state and the
			// initial steps) so the caller can refer to it and render the
			// timeline; the load job that follows the object carries the
			// name too. Reading the state first also lets the backend
			// compose the endpoint from the object it just created.
			view := s.loadedView(ctx, b, m)
			view.Fit = fit
			// Wired now, with the caller's token, at the address the
			// backend knows the model will answer on: the only wiring
			// that survives a restart of this process during the cold
			// start (giantswarm/model-manager#115). The job refreshes it
			// at readiness.
			view.Wiring = s.wire(ctx, b, m.Name, WiredOnLoad)
			if view.Wiring.ModelConfig != nil {
				view.ModelConfig = view.Wiring.ModelConfig
			}
			s.startLoadJob(ctx, b, sl, m.Name, view.Running)
			return view, nil
		}
		if _, err := s.wireModel(ctx, b, m.Name, backend.WireOptions{}); err != nil {
			s.log.Warn("auto-wire after load failed", "backend", b.Name(), "model", m.Name, "error", err)
		}
	}
	view := s.loadedView(ctx, b, m)
	view.Fit = fit
	return view, nil
}

// wire wires model on b as the caller and says what it did: Wired with the
// ModelConfig and reason as the occasion (WiredOnLoad, WiredOnRead) — or
// AlreadyWired when a ModelConfig someone else created already points at
// the served model, which is reported and left alone — else WiringFailed
// with the error. Never silent: the answer carries the result, the log the
// failure.
func (s *Service) wire(ctx context.Context, b backend.Backend, model, reason string) *WiringResult {
	ref, err := s.wireModel(ctx, b, model, backend.WireOptions{})
	if err != nil {
		s.log.Warn("wiring failed", "backend", b.Name(), "model", model, "occasion", reason, "error", err, identity.LogAttr(ctx))
		return &WiringResult{Reason: WiringFailed, Error: err.Error()}
	}
	if !ref.Managed {
		reason = AlreadyWired
	}
	return &WiringResult{Wired: true, Reason: reason, ModelConfig: ref}
}

// loadedView is what a load answers: the model as the backend lists it right
// after the load (running.status / running.reason of the serving object), or
// — when that read fails, the caller's deadline having run out after the
// object was created — the model as resolved. A load that succeeded never
// answers as a cancelled call (giantswarm/model-manager#104).
func (s *Service) loadedView(ctx context.Context, b backend.Backend, m *backend.Model) *ModelView {
	view, err := s.GetModel(ctx, string(b.Name()), m.Name)
	if err != nil {
		s.log.Warn("reading the model's state after the load failed; answering with the resolved model", "backend", b.Name(), "model", m.Name, "error", err, identity.LogAttr(ctx))
		return &ModelView{Model: *m}
	}
	return view
}

// startLoadJob follows a served model to readiness and then refreshes its
// ModelConfig from the address the backend published — the same ModelConfig
// the load wired, updated in place (Ensure is idempotent), never a second
// one. running is the serving object as the backend lists it right after
// the load, nil when it lists none. The job lives in this process: a
// restart drops it, and loses nothing but its progress entry.
func (s *Service) startLoadJob(ctx context.Context, b backend.Backend, sl backend.ServeLifecycle, model string, running *backend.LoadedModel) {
	start := jobs.StartRequest{Type: jobs.TypeLoad, Backend: b.Name(), Model: model, Wire: true, Context: ctx}
	object := model
	if running != nil {
		start.Resource, start.Preset = running.Resource, running.Preset
		if running.Kind != "" && running.Resource != "" {
			object = running.Kind + " " + running.Resource
		}
	}
	job, created := s.jobs.Start(start,
		func(jobCtx context.Context, report func(backend.Progress)) (any, error) {
			report(backend.Progress{Status: "waiting for " + object + " to become ready"})
			if err := sl.WaitReady(jobCtx, model); err != nil {
				return nil, err
			}
			report(backend.Progress{Status: "ready; refreshing the ModelConfig from the published address"})
			ref, err := s.wireModel(jobCtx, b, model, backend.WireOptions{})
			if err != nil {
				return nil, fmt.Errorf("%s is ready but refreshing its ModelConfig failed: %w", model, err)
			}
			report(backend.Progress{Status: "wired"})
			return ref, nil
		})
	if created {
		s.log.Info("load job started", "backend", b.Name(), "model", model, "job", job.ID, identity.LogAttr(ctx))
	}
}

// UnloadView is what an unload answers: the backend the model was on, the
// reference as the caller gave it, and — on a backend.Stopper (kserve) —
// what follows the deletion in the cache inventory.
type UnloadView struct {
	Backend   backend.Name              `json:"backend"`
	Model     string                    `json:"model"`
	Loaded    bool                      `json:"loaded"`
	Inventory *backend.InventoryRefresh `json:"inventory,omitempty"`
}

// Unload evicts a model and reports the backend it was on. On ServeLifecycle
// backends the ModelConfig goes with the endpoint. A backend.Stopper named by
// the caller — or the only backend — is asked to stop by the reference
// directly: it finds the served object itself, so no inventory read stands
// between the call and the deletion, and the answer carries what follows
// (giantswarm/model-manager#119); every other backend is resolved first.
func (s *Service) Unload(ctx context.Context, name, ref string) (*UnloadView, error) {
	b, model, res, err := s.stop(ctx, name, ref)
	if err != nil {
		return nil, err
	}
	s.log.Info("model unloaded", "backend", b.Name(), "model", model, identity.LogAttr(ctx))
	if _, ok := serveLifecycle(b); ok && s.wirer != nil {
		if err := s.wirer.Remove(ctx, b.Name(), model); err != nil {
			s.log.Warn("unwire after unload failed", "backend", b.Name(), "model", model, "error", err)
		} else {
			s.log.Info("model unwired", "backend", b.Name(), "model", model, identity.LogAttr(ctx))
		}
	}
	view := &UnloadView{Backend: b.Name(), Model: strings.TrimSpace(ref)}
	if res != nil {
		view.Inventory = &res.Inventory
	}
	return view, nil
}

// stop unloads ref and returns the backend, the model's name on it and, on
// a backend.Stopper, its answer. The Stopper is addressed by the reference as
// given when the caller named its backend or it is the only one; unqualified
// among several, the reference is resolved across backends as every other
// call does.
func (s *Service) stop(ctx context.Context, name, ref string) (backend.Backend, string, *backend.UnloadResult, error) {
	if strings.TrimSpace(name) != "" || len(s.all()) == 1 {
		b, err := s.named(name)
		if err != nil {
			return nil, "", nil, err
		}
		if st, ok := b.(backend.Stopper); ok {
			if err := unloadable(b); err != nil {
				return nil, "", nil, err
			}
			res, err := st.Stop(ctx, ref)
			if err != nil {
				return nil, "", nil, err
			}
			return b, res.Model, res, nil
		}
	}
	b, m, err := s.resolve(ctx, name, ref)
	if err != nil {
		return nil, "", nil, err
	}
	if err := unloadable(b); err != nil {
		return nil, "", nil, err
	}
	if err := b.Unload(ctx, m.Name); err != nil {
		return nil, "", nil, err
	}
	return b, m.Name, nil, nil
}

// unloadable is the error for a backend without the unload capability; nil
// for one with it.
func unloadable(b backend.Backend) error {
	if b.Capabilities().Unload {
		return nil
	}
	return fmt.Errorf("%w: unload on %s", backend.ErrUnsupported, b.Name())
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
func (s *Service) Wire(ctx context.Context, name, ref string, opts backend.WireOptions) (*wiring.ModelConfigRef, error) {
	if s.wirer == nil {
		return nil, ErrWiringDisabled
	}
	b, m, err := s.resolve(ctx, name, ref)
	if err != nil {
		return nil, err
	}
	return s.wireModel(ctx, b, m.Name, opts)
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
	if strings.TrimSpace(name) != "" || len(s.all()) == 1 {
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
			b, _ = s.lookup(owners[0])
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
		if _, ok := s.lookup(owner); !ok {
			if d := s.Default(); d != nil {
				owner = d.Name()
			}
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
			presets[i].Target = backend.TargetOf(b)
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
			nodes[i].Target = backend.TargetOf(b)
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
// survived a restart (PullAdopter backends); on ServeLifecycle backends,
// wiring served models that became ready without a load job watching them
// (model-manager restarted, or the LLMInferenceService was created by someone
// else with the preset label); on the others, bringing the context window and
// think of the ModelConfigs model-manager owns to the configured ones
// (refreshModelConfigs).
func (s *Service) Run(ctx context.Context) {
	if s.cfg.CallerOnly {
		s.log.Info("caller-only mode: no download adoption and no wiring reconciler (every Kubernetes call carries the caller's token; the ServiceAccount holds no permissions); a restart loses the in-memory jobs and nothing else — a served model's ModelConfig is written by the load itself and, when missing, by the caller's next read, a running download Job is joined by the next pull")
		return
	}
	for _, b := range s.all() {
		s.adoptPulls(ctx, b)
	}
	if s.cfg.ReconcileInterval <= 0 || s.wirer == nil || !s.cfg.AutoWire {
		return
	}
	ticker := time.NewTicker(s.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		// Recomputed every tick: a backend registered at runtime joins.
		for _, b := range s.all() {
			if _, ok := serveLifecycle(b); ok {
				s.reconcileWiring(ctx, b)
			} else {
				s.refreshModelConfigs(ctx, b)
			}
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
		if _, ok := wired.lookup(l.Name, &l); ok {
			continue // model-manager's own, or someone else's (the portal)
		}
		if s.hasActiveJob(jobs.TypeLoad, b.Name(), l.Name) {
			continue
		}
		if _, err := s.wireModel(rctx, b, l.Name, backend.WireOptions{}); err != nil {
			s.log.Warn("reconcile: wiring served model failed", "backend", b.Name(), "model", l.Name, "error", err)
		}
	}
}

// refreshModelConfigs re-wires every ModelConfig model-manager owns on b
// whose agent settings — the context window and think — are not the ones a
// wiring writes now: one written before a setting existed, under another
// value, or edited by hand. So a changed ollama.contextLength or ollama.think
// reaches the agents of models wired long ago, which on Ollama are loaded by
// the agents' own requests and never by a load that would re-wire them. The
// re-wire is the one a load does: the backend's own endpoint, fitted to the
// model. The comparison is against what the served ModelConfig schema can
// carry (Wirer.Writable), so a field the apiserver prunes never re-wires on
// every pass. Up-to-date ModelConfigs are not touched; one whose model is gone
// from b is left to unwire.
func (s *Service) refreshModelConfigs(ctx context.Context, b backend.Backend) {
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	refs, err := s.wirer.List(rctx)
	if err != nil {
		s.log.Warn("refresh: listing ModelConfigs failed", "backend", b.Name(), "error", err)
		return
	}
	for _, r := range refs {
		if r.Backend != "" && r.Backend != b.Name() {
			continue
		}
		if ep := b.AgentEndpoint(r.Model); ep.ContextLength == 0 && ep.Think == nil && r.ContextLength == 0 && r.Think == nil {
			continue // nothing to write, nothing written
		}
		m, err := b.GetModel(rctx, r.Model)
		if err != nil {
			continue
		}
		want, err := s.wirer.Writable(rctx, b.AgentEndpoint(m.Name).FitTo(*m))
		if err != nil {
			s.log.Warn("refresh: reading what a ModelConfig carries failed", "backend", b.Name(), "error", err)
			return
		}
		if r.Carries(want) {
			continue
		}
		if _, err := s.wireModel(rctx, b, m.Name, backend.WireOptions{}); err != nil {
			s.log.Warn("refresh: re-wiring for the agent settings failed", "backend", b.Name(), "model", m.Name, "modelConfig", r.Namespace+"/"+r.Name, "error", err)
			continue
		}
		s.log.Info("ModelConfig agent settings refreshed", "backend", b.Name(), "model", m.Name, "modelConfig", r.Namespace+"/"+r.Name,
			"contextLength", fmt.Sprintf("%d → %d", r.ContextLength, want.ContextLength), "think", thinkString(r.Think)+" → "+thinkString(want.Think))
	}
}

// thinkString renders a think setting for the log: true, false or unset.
func thinkString(think *bool) string {
	if think == nil {
		return "unset"
	}
	return strconv.FormatBool(*think)
}

func (s *Service) hasActiveJob(t jobs.Type, b backend.Name, model string) bool {
	for _, j := range s.jobs.List() {
		if j.Type == t && j.Backend == b && j.Model == model && !j.Done() {
			return true
		}
	}
	return false
}

// wireModel wires model on b: the backend's endpoint with the caller's
// WireOptions laid over it (the API-key shape), refused before anything is
// written when the shape is not one the ModelConfig can carry.
func (s *Service) wireModel(ctx context.Context, b backend.Backend, model string, opts backend.WireOptions) (*wiring.ModelConfigRef, error) {
	// Resolve the canonical name so "smollm2:135m" and "smollm2:135m" pulled
	// as "smollm2" end up in one ModelConfig, and the model the endpoint is
	// fitted to: its own context length caps the window the ModelConfig asks
	// for, and think is written only for a model with the thinking
	// capability.
	var fit backend.Model
	if m, err := b.GetModel(ctx, model); err == nil {
		model, fit = m.Name, *m
	}
	ep := opts.Apply(b.AgentEndpoint(model)).FitTo(fit)
	ep.Backend = b.Name()
	if err := ep.Validate(); err != nil {
		return nil, err
	}
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

// lookup finds a model's ModelConfig: model-manager's own by model reference,
// else — for a served model l — anyone's by served endpoint and the name the
// provider serves the model under (the object's `spec.model.name` for a
// InferenceService, the model id for an LLMInferenceService: both are tried).
func (w wiredView) lookup(model string, l *backend.LoadedModel) (wiring.ModelConfigRef, bool) {
	if mc, ok := w.byModel[model]; ok {
		return mc, true
	}
	if l == nil || w.byEndpoint == nil {
		return wiring.ModelConfigRef{}, false
	}
	for _, servedAs := range []string{l.Resource, l.Name} {
		if mc, ok := w.byEndpoint[endpointKey(l.Endpoint, servedAs)]; ok {
			return mc, true
		}
	}
	return wiring.ModelConfigRef{}, false
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
	m.Target = backend.TargetOf(b)
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
	if mc, ok := wired.lookup(m.Name, v.Running); ok {
		v.ModelConfig = &mc
	}
	return v
}

// IsNotFound reports whether err is a not-found from the backend or jobs.
func IsNotFound(err error) bool {
	return errors.Is(err, backend.ErrNotFound) || errors.Is(err, jobs.ErrNotFound)
}

// UnwireBackend removes every model-manager-owned ModelConfig of backend
// name — what remove_backend does before the document goes. Nothing wired,
// or wiring disabled, is not an error; the removed model references are
// returned.
func (s *Service) UnwireBackend(ctx context.Context, name backend.Name) ([]string, error) {
	if s.wirer == nil {
		return nil, nil
	}
	refs, err := s.wirer.List(ctx)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, r := range refs {
		if r.Backend != name {
			continue
		}
		if err := s.wirer.Remove(ctx, name, r.Model); err != nil {
			return removed, fmt.Errorf("unwire %s on %s: %w", r.Model, name, err)
		}
		removed = append(removed, r.Model)
		s.log.Info("model unwired with its backend", "backend", name, "model", r.Model, identity.LogAttr(ctx))
	}
	return removed, nil
}

// WiredModels lists the model references of backend name's model-manager-owned
// ModelConfigs; nil when wiring is disabled.
func (s *Service) WiredModels(ctx context.Context, name backend.Name) ([]string, error) {
	if s.wirer == nil {
		return nil, nil
	}
	refs, err := s.wirer.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range refs {
		if r.Backend == name {
			out = append(out, r.Model)
		}
	}
	return out, nil
}
