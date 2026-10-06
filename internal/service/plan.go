package service

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/gitops"
	"github.com/giantswarm/model-manager/internal/jobs"
)

// OpPlan is the dry run of an operational write — pull, load, unload,
// delete — on any backend: what the call would do, and nothing done. The
// manifests are the objects it would create (kserve load: the serving
// object) or delete (kserve unload); wiring is the ModelConfig the call
// would write or remove.
type OpPlan struct {
	Backend backend.Name `json:"backend"`
	Model   string       `json:"model"`
	// Present says the model is downloaded; Loaded that it is loaded or
	// served now.
	Present bool `json:"present"`
	Loaded  bool `json:"loaded"`
	// KeepAlive is what a load would keep the model loaded for (ollama).
	KeepAlive      string             `json:"keepAlive,omitempty"`
	Fit            *backend.FitResult `json:"fit,omitempty"`
	AlreadyServing bool               `json:"alreadyServing,omitempty"`
	ServingNodes   []string           `json:"servingNodes,omitempty"`
	Manifests      []map[string]any   `json:"manifests"`
	Wiring         *WirePlan          `json:"wiring,omitempty"`
	// Commit is the pull request mode commit opened, or would open.
	Commit *gitops.Result `json:"commit,omitempty"`

	objects []*unstructured.Unstructured
	// refusal is why the live call in mode apply would refuse what the plan
	// lists (a serving object Flux applies from git, ErrGitOpsOwned); nil
	// when it would do it. The dry run answers it, mode commit sets it
	// aside.
	refusal error
}

func (p *OpPlan) add(objs ...*unstructured.Unstructured) {
	for _, obj := range objs {
		p.objects = append(p.objects, obj)
		p.Manifests = append(p.Manifests, Manifest(obj))
	}
}

// planWiring is the wiring plan of model on b: what Ensure would write,
// through the same endpoint a wire uses. Nil without a wirer.
func (s *Service) planWiring(ctx context.Context, b backend.Backend, model string) (*WirePlan, error) {
	if s.wirer == nil {
		return nil, nil
	}
	model, ep, existing, err := s.endpointFor(ctx, b, model, backend.WireOptions{})
	if err != nil {
		return nil, err
	}
	plan := &WirePlan{Backend: b.Name(), Model: model, Manifests: []map[string]any{}}
	if existing != nil {
		plan.AlreadyWired, plan.ModelConfig = existing, existing.Name
		return plan, nil
	}
	r, err := s.wirer.Render(ctx, model, ep)
	if err != nil {
		return nil, err
	}
	plan.fill(r)
	return plan, nil
}

// unwiring is the removal plan of model's ModelConfig on b; nil without a
// wirer.
func (s *Service) unwiring(ctx context.Context, b backend.Name, model string) (*WirePlan, error) {
	if s.wirer == nil {
		return nil, nil
	}
	r, err := s.wirer.Removal(ctx, b, model)
	if err != nil {
		return nil, err
	}
	plan := &WirePlan{Backend: b, Model: model, Manifests: []map[string]any{}}
	plan.fill(r)
	return plan, nil
}

// PlanPull is pull_model's dry run: whether the model is there already, on
// kserve the fit verdict and the node the download would go to, and the
// ModelConfig a successful pull would wire. No download starts.
func (s *Service) PlanPull(ctx context.Context, opts PullOptions) (*OpPlan, error) {
	b, err := s.named(opts.Backend)
	if err != nil {
		return nil, err
	}
	if !b.Capabilities().Pull {
		return nil, fmt.Errorf("%w: pull on %s", backend.ErrUnsupported, b.Name())
	}
	ref := strings.TrimSpace(opts.Model)
	if ref == "" {
		return nil, fmt.Errorf("%w: model reference is required", backend.ErrInvalid)
	}
	plan := &OpPlan{Backend: b.Name(), Model: ref, Manifests: []map[string]any{}}
	if m, err := b.GetModel(ctx, ref); err == nil {
		plan.Model, plan.Present = m.Name, true
	}
	if b.Capabilities().FitCheck {
		fit, err := s.FitCheck(ctx, string(b.Name()), backend.FitRequest{Model: ref, Preset: strings.TrimSpace(opts.Preset), Node: strings.TrimSpace(opts.Node)})
		if err != nil {
			return nil, err
		}
		plan.Fit = fit
	}
	_, servesOnLoad := serveLifecycle(b)
	doWire := s.cfg.AutoWire && s.wirer != nil && !servesOnLoad
	if opts.Wire != nil {
		doWire = *opts.Wire && !servesOnLoad
	}
	if doWire {
		if plan.Wiring, err = s.planWiring(ctx, b, plan.Model); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// PlanLoad is load_model's dry run: on kserve the fit verdict and the
// serving object the load would create (or where it serves already), on
// every backend the loaded state and the ModelConfig the auto-wire would
// ensure. Nothing is loaded, created or wired.
func (s *Service) PlanLoad(ctx context.Context, opts LoadOptions) (*OpPlan, error) {
	b, m, req, err := s.loadTarget(ctx, opts)
	if err != nil {
		return nil, err
	}
	plan := &OpPlan{Backend: b.Name(), Model: m.Name, Present: true, Manifests: []map[string]any{}}
	_, plan.Loaded = s.loadedIndex(ctx, b)[m.Name]
	srv, isServer := b.(backend.Server)
	if !isServer {
		plan.KeepAlive = req.KeepAlive
	} else {
		req.DryRun = true
		res, err := srv.Serve(ctx, req)
		if err != nil {
			return nil, err
		}
		if res != nil {
			plan.Fit, plan.AlreadyServing, plan.ServingNodes = res.Fit, res.AlreadyServing, res.ServingNodes
			plan.add(res.Manifests...)
		}
	}
	if s.cfg.AutoWire && !plan.AlreadyServing {
		if plan.Wiring, err = s.planWiring(ctx, b, m.Name); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// PlanUnload is unload_model's dry run: on kserve the serving objects the
// unload would delete and the ModelConfig it would unwire, refused as the
// unload is for a serving object Flux applies from git; elsewhere the
// loaded state. Nothing is unloaded.
func (s *Service) PlanUnload(ctx context.Context, name, ref string) (*OpPlan, error) {
	plan, err := s.planUnload(ctx, name, ref)
	if err != nil {
		return nil, err
	}
	if plan.refusal != nil {
		return nil, plan.refusal
	}
	return plan, nil
}

// planUnload is PlanUnload with the refusal of a GitOps-owned serving object
// kept on the plan rather than answered: mode commit removes the object in
// git.
func (s *Service) planUnload(ctx context.Context, name, ref string) (*OpPlan, error) {
	if b, stop, ok, err := s.stopPlan(ctx, name, ref); ok || err != nil {
		if err != nil {
			return nil, err
		}
		plan := &OpPlan{Backend: b.Name(), Model: stop.Model, Present: true, Loaded: true, Manifests: []map[string]any{}, refusal: stop.Refusal}
		plan.add(stop.Objects...)
		if plan.Wiring, err = s.unwiring(ctx, b.Name(), stop.Model); err != nil {
			return nil, err
		}
		return plan, nil
	}
	b, m, err := s.resolve(ctx, name, ref)
	if err != nil {
		return nil, err
	}
	if err := unloadable(b); err != nil {
		return nil, err
	}
	plan := &OpPlan{Backend: b.Name(), Model: m.Name, Present: true, Manifests: []map[string]any{}}
	_, plan.Loaded = s.loadedIndex(ctx, b)[m.Name]
	return plan, nil
}

// stopPlan asks a StopPlanner — named by the caller, or the only backend,
// as stop addresses a Stopper — what an unload would do; ok false when the
// unload is not a StopPlanner's.
func (s *Service) stopPlan(ctx context.Context, name, ref string) (backend.Backend, *backend.StopPlan, bool, error) {
	if strings.TrimSpace(name) == "" && len(s.all()) != 1 {
		return nil, nil, false, nil
	}
	b, err := s.named(name)
	if err != nil {
		return nil, nil, false, err
	}
	sp, ok := b.(backend.StopPlanner)
	if !ok {
		return nil, nil, false, nil
	}
	if err := unloadable(b); err != nil {
		return nil, nil, true, err
	}
	stop, err := sp.StopPlan(ctx, ref)
	return b, stop, true, err
}

// PlanDelete is delete_model's dry run: whether the model is loaded and the
// ModelConfig the delete would unwire. Nothing is deleted.
func (s *Service) PlanDelete(ctx context.Context, name, ref string, unwire bool) (*OpPlan, error) {
	b, m, err := s.resolve(ctx, name, ref)
	if err != nil {
		return nil, err
	}
	if !b.Capabilities().Delete {
		return nil, fmt.Errorf("%w: delete on %s", backend.ErrUnsupported, b.Name())
	}
	plan := &OpPlan{Backend: b.Name(), Model: m.Name, Present: true, Manifests: []map[string]any{}}
	_, plan.Loaded = s.loadedIndex(ctx, b)[m.Name]
	if unwire {
		if plan.Wiring, err = s.unwiring(ctx, b.Name(), m.Name); err != nil {
			return nil, err
		}
		if plan.Wiring != nil && plan.Wiring.Left != nil {
			return nil, fmt.Errorf("unwire %s: %w; nothing would be deleted — repeat with unwire=false to delete the weights alone", m.Name, plan.Wiring.Left)
		}
	}
	return plan, nil
}

// PlanCancel is cancel_job's dry run: the job it would cancel.
func (s *Service) PlanCancel(id string) (jobs.Job, error) { return s.jobs.Get(id) }

// errCommitsOnKServe is mode commit on a load or unload a backend lands
// operationally.
func errCommitsOnKServe(tool string, b backend.Name) error {
	return fmt.Errorf("%w: mode commit on %s: %s serves no manifest a repository could hold (only kserve's serving object is one); use mode apply, and wire_model mode commit for the ModelConfig", backend.ErrUnsupported, tool, b)
}

// CommitLoad is load_model in mode commit (kserve): the serving object and
// the ModelConfig wiring it — its address is known from the object's name —
// as files in the repository that owns the serving namespace, one pull
// request opened as the caller. No load job follows: the ModelConfig becomes
// ready when the served model does, after Flux applies the merge.
func (s *Service) CommitLoad(ctx context.Context, opts LoadOptions, target gitops.Target, dryRun bool) (*OpPlan, error) {
	if s.commit == nil {
		return nil, ErrCommitUnavailable
	}
	plan, err := s.PlanLoad(ctx, opts)
	if err != nil {
		return nil, err
	}
	b, _ := s.lookup(plan.Backend)
	if _, ok := b.(backend.Server); !ok {
		return nil, errCommitsOnKServe("load_model", plan.Backend)
	}
	if plan.AlreadyServing || len(plan.objects) == 0 {
		return plan, nil
	}
	serving := plan.objects[0]
	req := gitops.Request{
		Namespace: serving.GetNamespace(), Target: target,
		Verb: "serve", Subject: serving.GetName(), Tool: "load_model", DryRun: dryRun,
		Summary: fmt.Sprintf("Serves the model `%s` as %s `%s/%s`, wired into kagent", plan.Model, serving.GetKind(), serving.GetNamespace(), serving.GetName()),
	}
	var wiring []*unstructured.Unstructured
	if plan.Wiring != nil {
		wiring = plan.Wiring.objects
	}
	if err := splitForTarget(&req, b, plan.objects, wiring, false); err != nil {
		return nil, err
	}
	plan.Commit, err = s.commit.Commit(ctx, req)
	if err != nil {
		return nil, err
	}
	plan.Commit.LiveSteps = append(plan.Commit.LiveSteps, "the ModelConfig turns ready once the served model does; its entry on the platform's LLM endpoint follows with the next read of list_loaded_models")
	return plan, nil
}

// splitForTarget puts a serving commit's objects into req: on a local
// target the serving objects and their wiring share the serving namespace's
// location; on a remote target the serving objects land on the target (its
// namespace's provenance read there as the caller) and the wiring — the
// ModelConfig and its Secret in the kagent namespace — on model-manager's
// own cluster, a part of its own. remove says the objects go.
func splitForTarget(req *gitops.Request, b backend.Backend, serving, wiring []*unstructured.Unstructured, remove bool) error {
	put := func(objs []*unstructured.Unstructured) ([]*unstructured.Unstructured, []*unstructured.Unstructured) {
		if remove {
			return nil, objs
		}
		return objs, nil
	}
	t := backend.TargetOf(b)
	if t == nil {
		req.Write, req.Remove = put(append(slices.Clone(serving), wiring...))
		return nil
	}
	client, ok := b.(backend.TargetClient)
	if !ok {
		return fmt.Errorf("%w: the backend on cluster %s offers no client of its target to read the serving namespace's provenance there", backend.ErrUnsupported, t.Cluster)
	}
	req.Cluster = &gitops.Cluster{Name: t.Cluster, Dynamic: client.TargetDynamic}
	req.Write, req.Remove = put(serving)
	if len(wiring) > 0 {
		part := gitops.Part{Namespace: wiring[0].GetNamespace(), Owner: gitops.OwnerOf(wiring[0].GetLabels())}
		part.Write, part.Remove = put(wiring)
		req.Also = []gitops.Part{part}
	}
	return nil
}

// CommitUnload is unload_model in mode commit (kserve): the removing pull
// request of the serving object's file and its ModelConfig's. The serving
// object being applied from git is the case here, no refusal.
func (s *Service) CommitUnload(ctx context.Context, name, ref string, target gitops.Target, dryRun bool) (*OpPlan, error) {
	if s.commit == nil {
		return nil, ErrCommitUnavailable
	}
	plan, err := s.planUnload(ctx, name, ref)
	if err != nil {
		return nil, err
	}
	b, _ := s.lookup(plan.Backend)
	if _, ok := b.(backend.StopPlanner); !ok {
		return nil, errCommitsOnKServe("unload_model", plan.Backend)
	}
	serving := plan.objects[0]
	req := gitops.Request{
		Namespace: serving.GetNamespace(), Owner: gitops.OwnerOf(serving.GetLabels()), Target: target,
		Verb: "unserve", Subject: serving.GetName(), Tool: "unload_model", DryRun: dryRun,
		Summary: fmt.Sprintf("Stops serving the model `%s`: removes %s `%s/%s` and its ModelConfig", plan.Model, serving.GetKind(), serving.GetNamespace(), serving.GetName()),
	}
	var wiring []*unstructured.Unstructured
	if plan.Wiring != nil {
		wiring = plan.Wiring.objects
	}
	if err := splitForTarget(&req, b, plan.objects, wiring, true); err != nil {
		return nil, err
	}
	plan.Commit, err = s.commit.Commit(ctx, req)
	if err != nil {
		return nil, err
	}
	return plan, nil
}
