// Package service orchestrates the backend driver, the job manager and the
// agent wiring behind one backend-agnostic API that both the REST and the MCP
// surfaces expose.
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
}

// BackendResponse is the backend identity plus effective capabilities.
type BackendResponse struct {
	backend.Info
	Capabilities backend.Capabilities `json:"capabilities"`
	// Wiring describes where ModelConfigs are created, when wiring is enabled.
	Wiring *WiringInfo `json:"wiring,omitempty"`
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
	Model string
	// Wire nil means the AutoWire default.
	Wire *bool
	// Preset / Node are kserve concerns (cache directory and target node).
	Preset string
	Node   string
}

// LoadOptions describe a load / serve request.
type LoadOptions struct {
	Model     string
	KeepAlive string
	Preset    string
	Node      string
}

// Service is the orchestration layer.
type Service struct {
	backend backend.Backend
	jobs    *jobs.Manager
	wirer   wiring.Wirer
	wiring  *WiringInfo
	cfg     Config
	log     *slog.Logger
}

// New builds a Service. wirer may be nil (wiring disabled).
func New(b backend.Backend, jm *jobs.Manager, wirer wiring.Wirer, info *WiringInfo, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if info != nil {
		info.AutoWire = cfg.AutoWire
	}
	return &Service{backend: b, jobs: jm, wirer: wirer, wiring: info, cfg: cfg, log: log}
}

// Capabilities returns the effective flags: the driver's plus Wire.
func (s *Service) Capabilities() backend.Capabilities {
	caps := s.backend.Capabilities()
	caps.Wire = s.wirer != nil
	return caps
}

// Backend reports identity, health and capabilities.
func (s *Service) Backend(ctx context.Context) BackendResponse {
	resp := BackendResponse{Info: s.backend.Info(ctx), Capabilities: s.Capabilities()}
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

// Ready reports whether the backend answers.
func (s *Service) Ready(ctx context.Context) error {
	info := s.backend.Info(ctx)
	if !info.Healthy {
		return fmt.Errorf("backend %s not healthy: %s", info.Backend, info.Message)
	}
	return nil
}

// serveLifecycle reports whether the backend's endpoints exist only while a
// model is loaded (wire on ready, unwire on unload, never wire after a pull).
func (s *Service) serveLifecycle() (backend.ServeLifecycle, bool) {
	sl, ok := s.backend.(backend.ServeLifecycle)
	return sl, ok
}

// ListModels returns the downloaded models enriched with loaded state and
// ModelConfig references. Enrichment failures are logged, not fatal.
func (s *Service) ListModels(ctx context.Context) ([]ModelView, error) {
	models, err := s.backend.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	loaded := s.loadedIndex(ctx)
	wired := s.wiredIndex(ctx)
	out := make([]ModelView, 0, len(models))
	for _, m := range models {
		out = append(out, s.view(m, loaded, wired))
	}
	return out, nil
}

// GetModel returns one enriched model.
func (s *Service) GetModel(ctx context.Context, name string) (*ModelView, error) {
	m, err := s.backend.GetModel(ctx, name)
	if err != nil {
		return nil, err
	}
	v := s.view(*m, s.loadedIndex(ctx), s.wiredIndex(ctx))
	return &v, nil
}

// ListLoaded returns the loaded/running models.
func (s *Service) ListLoaded(ctx context.Context) ([]backend.LoadedModel, error) {
	if !s.backend.Capabilities().LoadedModels {
		return nil, fmt.Errorf("%w: listing loaded models", backend.ErrUnsupported)
	}
	return s.backend.ListLoaded(ctx)
}

// Pull starts (or joins) an import job.
func (s *Service) Pull(ctx context.Context, opts PullOptions) (jobs.Job, bool, error) {
	if !s.backend.Capabilities().Pull {
		return jobs.Job{}, false, fmt.Errorf("%w: pull", backend.ErrUnsupported)
	}
	ref := strings.TrimSpace(opts.Model)
	if ref == "" {
		return jobs.Job{}, false, fmt.Errorf("%w: model reference is required", backend.ErrInvalid)
	}
	_, servesOnLoad := s.serveLifecycle()
	doWire := s.cfg.AutoWire && s.wirer != nil && !servesOnLoad
	if opts.Wire != nil {
		doWire = *opts.Wire
	}
	if doWire && s.wirer == nil {
		return jobs.Job{}, false, ErrWiringDisabled
	}
	if doWire && servesOnLoad {
		return jobs.Job{}, false, fmt.Errorf("%w: models on this backend are wired when loaded (served), not when pulled", backend.ErrInvalid)
	}
	req := backend.PullRequest{Ref: ref, Preset: strings.TrimSpace(opts.Preset), Node: strings.TrimSpace(opts.Node)}
	job, created := s.startPull(req, doWire)
	if created {
		s.log.Info("pull started", "model", ref, "job", job.ID, "wire", doWire, "preset", req.Preset, "node", req.Node)
	}
	return job, created, nil
}

func (s *Service) startPull(req backend.PullRequest, doWire bool) (jobs.Job, bool) {
	return s.jobs.Start(jobs.StartRequest{Type: jobs.TypePull, Model: req.Ref, Wire: doWire, Node: req.Node, Preset: req.Preset},
		func(jobCtx context.Context, report func(backend.Progress)) (any, error) {
			if err := s.backend.Pull(jobCtx, req, report); err != nil {
				return nil, err
			}
			if !doWire {
				return nil, nil
			}
			mcRef, err := s.wireModel(jobCtx, req.Ref)
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
	if !s.backend.Capabilities().Load {
		return nil, fmt.Errorf("%w: load", backend.ErrUnsupported)
	}
	name := strings.TrimSpace(opts.Model)
	if name == "" && opts.Preset != "" {
		name = opts.Preset
	}
	m, err := s.backend.GetModel(ctx, name)
	if err != nil {
		return nil, err
	}
	keepAlive := opts.KeepAlive
	if keepAlive == "" {
		keepAlive = s.cfg.DefaultKeepAlive
	}
	req := backend.LoadRequest{Name: m.Name, KeepAlive: keepAlive, Preset: strings.TrimSpace(opts.Preset), Node: strings.TrimSpace(opts.Node)}
	if req.Preset == "" && m.Preset != "" {
		req.Preset = m.Preset
	}
	if err := s.backend.Load(ctx, req); err != nil {
		return nil, err
	}
	s.log.Info("model loaded", "model", m.Name, "keepAlive", keepAlive, "preset", req.Preset, "node", req.Node)
	if s.cfg.AutoWire && s.wirer != nil {
		if sl, ok := s.serveLifecycle(); ok {
			s.startLoadJob(sl, m.Name)
		} else if _, err := s.wireModel(ctx, m.Name); err != nil {
			s.log.Warn("auto-wire after load failed", "model", m.Name, "error", err)
		}
	}
	return s.GetModel(ctx, m.Name)
}

// startLoadJob follows a served model to readiness and wires it.
func (s *Service) startLoadJob(sl backend.ServeLifecycle, model string) {
	job, created := s.jobs.Start(jobs.StartRequest{Type: jobs.TypeLoad, Model: model, Wire: true},
		func(jobCtx context.Context, report func(backend.Progress)) (any, error) {
			report(backend.Progress{Status: "waiting for the served model to become ready"})
			if err := sl.WaitReady(jobCtx, model); err != nil {
				return nil, err
			}
			report(backend.Progress{Status: "ready; wiring into kagent"})
			ref, err := s.wireModel(jobCtx, model)
			if err != nil {
				return nil, fmt.Errorf("%s is ready but wiring failed: %w", model, err)
			}
			report(backend.Progress{Status: "wired"})
			return ref, nil
		})
	if created {
		s.log.Info("load job started", "model", model, "job", job.ID)
	}
}

// Unload evicts a model. On ServeLifecycle backends the ModelConfig goes with
// the endpoint.
func (s *Service) Unload(ctx context.Context, name string) error {
	if !s.backend.Capabilities().Unload {
		return fmt.Errorf("%w: unload", backend.ErrUnsupported)
	}
	m, err := s.backend.GetModel(ctx, name)
	if err != nil {
		return err
	}
	if err := s.backend.Unload(ctx, m.Name); err != nil {
		return err
	}
	s.log.Info("model unloaded", "model", m.Name)
	if _, ok := s.serveLifecycle(); ok && s.wirer != nil {
		if err := s.wirer.Remove(ctx, m.Name); err != nil {
			s.log.Warn("unwire after unload failed", "model", m.Name, "error", err)
		} else {
			s.log.Info("model unwired", "model", m.Name)
		}
	}
	return nil
}

// Delete removes a downloaded model, unwiring it first when requested.
func (s *Service) Delete(ctx context.Context, name string, unwire bool) error {
	if !s.backend.Capabilities().Delete {
		return fmt.Errorf("%w: delete", backend.ErrUnsupported)
	}
	m, err := s.backend.GetModel(ctx, name)
	if err != nil {
		return err
	}
	if unwire && s.wirer != nil {
		if err := s.wirer.Remove(ctx, m.Name); err != nil {
			return fmt.Errorf("unwire %s: %w", m.Name, err)
		}
	}
	if err := s.backend.Delete(ctx, m.Name); err != nil {
		return err
	}
	s.log.Info("model deleted", "model", m.Name, "unwired", unwire && s.wirer != nil)
	return nil
}

// Wire creates the ModelConfig for an existing model.
func (s *Service) Wire(ctx context.Context, name string) (*wiring.ModelConfigRef, error) {
	if s.wirer == nil {
		return nil, ErrWiringDisabled
	}
	m, err := s.backend.GetModel(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.wireModel(ctx, m.Name)
}

// Unwire removes the ModelConfig for a model (which need not exist anymore).
func (s *Service) Unwire(ctx context.Context, name string) error {
	if s.wirer == nil {
		return ErrWiringDisabled
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: model name is required", backend.ErrInvalid)
	}
	if m, err := s.backend.GetModel(ctx, name); err == nil {
		name = m.Name
	}
	if err := s.wirer.Remove(ctx, name); err != nil {
		return err
	}
	s.log.Info("model unwired", "model", name)
	return nil
}

// Presets lists the serving presets (capability presets).
func (s *Service) Presets(ctx context.Context) ([]backend.Preset, error) {
	pl, ok := s.backend.(backend.PresetLister)
	if !ok || !s.backend.Capabilities().Presets {
		return nil, fmt.Errorf("%w: presets", backend.ErrUnsupported)
	}
	presets, err := pl.ListPresets(ctx)
	if err != nil {
		return nil, err
	}
	if presets == nil {
		presets = []backend.Preset{}
	}
	return presets, nil
}

// Search proxies the model hub (capability search).
func (s *Service) Search(ctx context.Context, query string, limit int) ([]backend.SearchResult, error) {
	sr, ok := s.backend.(backend.Searcher)
	if !ok || !s.backend.Capabilities().Search {
		return nil, fmt.Errorf("%w: search", backend.ErrUnsupported)
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: query is required", backend.ErrInvalid)
	}
	hits, err := sr.Search(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	if hits == nil {
		hits = []backend.SearchResult{}
	}
	return hits, nil
}

// FitCheck sizes a model against node budgets (capability fitCheck).
func (s *Service) FitCheck(ctx context.Context, req backend.FitRequest) (*backend.FitResult, error) {
	fc, ok := s.backend.(backend.FitChecker)
	if !ok || !s.backend.Capabilities().FitCheck {
		return nil, fmt.Errorf("%w: fit check", backend.ErrUnsupported)
	}
	if strings.TrimSpace(req.Model) == "" && strings.TrimSpace(req.Preset) == "" {
		return nil, fmt.Errorf("%w: model or preset is required", backend.ErrInvalid)
	}
	return fc.FitCheck(ctx, req)
}

// Nodes lists node budgets and caches (capability nodeInventory).
func (s *Service) Nodes(ctx context.Context) ([]backend.NodeInfo, error) {
	nl, ok := s.backend.(backend.NodeLister)
	if !ok || !s.backend.Capabilities().NodeInventory {
		return nil, fmt.Errorf("%w: node inventory", backend.ErrUnsupported)
	}
	nodes, err := nl.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	if nodes == nil {
		nodes = []backend.NodeInfo{}
	}
	return nodes, nil
}

// Jobs lists jobs, newest first.
func (s *Service) Jobs() []jobs.Job { return s.jobs.List() }

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
	s.adoptPulls(ctx)
	sl, ok := s.serveLifecycle()
	if !ok || s.cfg.ReconcileInterval <= 0 || s.wirer == nil || !s.cfg.AutoWire {
		return
	}
	ticker := time.NewTicker(s.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		s.reconcileWiring(ctx, sl)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) adoptPulls(ctx context.Context) {
	pa, ok := s.backend.(backend.PullAdopter)
	if !ok {
		return
	}
	pulls, err := pa.RunningPulls(ctx)
	if err != nil {
		s.log.Warn("listing running pulls for adoption failed", "error", err)
		return
	}
	for _, req := range pulls {
		job, created := s.startPull(req, false)
		if created {
			s.log.Info("adopted running pull", "model", req.Ref, "job", job.ID)
		}
	}
}

// reconcileWiring ensures a ModelConfig for every ready served model that has
// none. It never touches ModelConfigs of models that are not served: unload
// removes those explicitly.
func (s *Service) reconcileWiring(ctx context.Context, _ backend.ServeLifecycle) {
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	loaded, err := s.backend.ListLoaded(rctx)
	if err != nil {
		s.log.Warn("reconcile: listing served models failed", "error", err)
		return
	}
	wired := s.wiredIndex(rctx)
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
		if s.hasActiveJob(jobs.TypeLoad, l.Name) {
			continue
		}
		if _, err := s.wireModel(rctx, l.Name); err != nil {
			s.log.Warn("reconcile: wiring served model failed", "model", l.Name, "error", err)
		}
	}
}

func (s *Service) hasActiveJob(t jobs.Type, model string) bool {
	for _, j := range s.jobs.List() {
		if j.Type == t && j.Model == model && !j.Done() {
			return true
		}
	}
	return false
}

func (s *Service) wireModel(ctx context.Context, model string) (*wiring.ModelConfigRef, error) {
	// Resolve the canonical name so "smollm2:135m" and "smollm2:135m" pulled
	// as "smollm2" end up in one ModelConfig.
	if m, err := s.backend.GetModel(ctx, model); err == nil {
		model = m.Name
	}
	ep := s.backend.AgentEndpoint(model)
	// On serve-lifecycle backends the endpoint identifies the served model:
	// a ModelConfig someone else created for it (the portal's serve flow)
	// counts as wired — never a duplicate, never touched.
	if _, ok := s.serveLifecycle(); ok {
		if existing := s.foreignForEndpoint(ctx, ep); existing != nil {
			s.log.Info("model already wired by another owner", "model", model, "modelConfig", existing.Namespace+"/"+existing.Name)
			return existing, nil
		}
	}
	ref, err := s.wirer.Ensure(ctx, model, ep)
	if err != nil {
		return nil, err
	}
	s.log.Info("model wired", "model", model, "modelConfig", ref.Namespace+"/"+ref.Name)
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

func (s *Service) loadedIndex(ctx context.Context) map[string]backend.LoadedModel {
	idx := map[string]backend.LoadedModel{}
	if !s.backend.Capabilities().LoadedModels {
		return idx
	}
	loaded, err := s.backend.ListLoaded(ctx)
	if err != nil {
		s.log.Warn("listing loaded models failed", "error", err)
		return idx
	}
	for _, l := range loaded {
		idx[l.Name] = l
		if l.Resource != "" {
			idx[l.Resource] = l
		}
	}
	return idx
}

// wiredView is what the model views join against: model-manager's own
// ModelConfigs by model reference and, on serve-lifecycle backends, every
// ModelConfig by served endpoint so a portal-wired model shows its config.
type wiredView struct {
	byModel    map[string]wiring.ModelConfigRef
	byEndpoint map[string]wiring.ModelConfigRef
}

func (w wiredView) has(model string) bool {
	_, ok := w.byModel[model]
	return ok
}

func (s *Service) wiredIndex(ctx context.Context) wiredView {
	out := wiredView{}
	if s.wirer == nil {
		return out
	}
	wired, err := s.wirer.List(ctx)
	if err != nil {
		s.log.Warn("listing ModelConfigs failed", "error", err)
		return out
	}
	out.byModel = wired
	if _, ok := s.serveLifecycle(); !ok {
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

func (s *Service) view(m backend.Model, loaded map[string]backend.LoadedModel, wired wiredView) ModelView {
	v := ModelView{Model: m}
	l, ok := loaded[m.Name]
	if !ok && m.Path != "" {
		l, ok = loaded[m.Path]
	}
	if ok {
		v.Loaded = true
		lc := l
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
