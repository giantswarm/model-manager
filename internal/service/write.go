package service

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/gitops"
	"github.com/giantswarm/model-manager/internal/wiring"
)

// WirePlan is what a wiring write lands — or, on a dry run, would land —
// without landing it: the backend and canonical model, the ModelConfig's
// name, the manifests it writes (removal: the objects it deletes), the Flux
// object applying the ModelConfig as it exists, and the ModelConfig of
// another owner that already wires the served model (then nothing is
// written).
type WirePlan struct {
	Backend      backend.Name                 `json:"backend"`
	Model        string                       `json:"model"`
	ModelConfig  string                       `json:"modelConfig,omitempty"`
	Manifests    []map[string]any             `json:"manifests"`
	GitOps       *gitops.Owner                `json:"gitops,omitempty"`
	Replaces     string                       `json:"replaces,omitempty"`
	AlreadyWired *wiring.ModelConfigRef       `json:"alreadyWired,omitempty"`
	objects      []*unstructured.Unstructured // the rendered objects, as committed
}

// Objects are the plan's objects as rendered, for commit mode's files.
func (p *WirePlan) Objects() []*unstructured.Unstructured { return p.objects }

// PlanWire is wire_model's dry run: what Wire would write for ref, nothing
// written. A ModelConfig Flux applies from git is planned like any other —
// the plan names its owner, and Wire in apply mode would refuse it.
func (s *Service) PlanWire(ctx context.Context, name, ref string, opts backend.WireOptions) (*WirePlan, error) {
	if s.wirer == nil {
		return nil, ErrWiringDisabled
	}
	b, m, err := s.resolve(ctx, name, ref)
	if err != nil {
		return nil, err
	}
	model, ep, existing, err := s.endpointFor(ctx, b, m.Name, opts)
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

// PlanUnwire is unwire_model's dry run: what Unwire would delete for ref,
// nothing deleted; no manifests when nothing is wired.
func (s *Service) PlanUnwire(ctx context.Context, name, ref string) (*WirePlan, error) {
	b, ref, err := s.unwireTarget(ctx, name, ref)
	if err != nil {
		return nil, err
	}
	plan := &WirePlan{Model: ref, Manifests: []map[string]any{}}
	if b == nil {
		return plan, nil
	}
	plan.Backend = b.Name()
	r, err := s.wirer.Removal(ctx, b.Name(), ref)
	if err != nil {
		return nil, err
	}
	plan.fill(r)
	return plan, nil
}

func (p *WirePlan) fill(r *wiring.Rendered) {
	p.ModelConfig, p.GitOps, p.Replaces, p.objects = r.Name, r.GitOps, r.Replaces, r.Objects
	for _, obj := range r.Objects {
		p.Manifests = append(p.Manifests, Manifest(obj))
	}
}

// Manifest is obj as a dry run shows it: what a kubectl or GitOps user
// would apply — no server-set metadata, no status — and a Secret's data read
// from the cluster left out (its keys stay); the stringData model-manager
// renders itself is kept.
func Manifest(obj *unstructured.Unstructured) map[string]any {
	out := obj.DeepCopy()
	for _, f := range []string{"resourceVersion", "uid", "generation", "creationTimestamp", "managedFields", "ownerReferences"} {
		unstructured.RemoveNestedField(out.Object, "metadata", f)
	}
	delete(out.Object, "status")
	if out.GetKind() == "Secret" {
		if data, ok := out.Object["data"].(map[string]any); ok {
			redacted := make(map[string]any, len(data))
			for k := range data {
				redacted[k] = "(redacted)"
			}
			out.Object["data"] = redacted
		}
	}
	return out.Object
}

// WithCommitter offers commit mode: wiring writes land as a pull request
// opened as the person. Call before the service serves.
func (s *Service) WithCommitter(c *gitops.Committer) *Service {
	s.commit = c
	return s
}

// ErrCommitUnavailable is mode commit on a server without its GitHub App pin.
var ErrCommitUnavailable = fmt.Errorf("%w: mode commit (a pull request opened as you) is not offered by this server: it is not registered with its GitHub App (chart value github.enabled), so it holds no GitHub authorization of yours — use mode apply", backend.ErrUnsupported)

// CommitView is a write in commit mode: the plan and the pull request.
type CommitView struct {
	*WirePlan
	Commit *gitops.Result `json:"commit,omitempty"`
}

// CommitWire is wire_model in commit mode: the ModelConfig (and its
// placeholder Secret) as files in the repository that owns the kagent
// namespace, one pull request opened as the caller; dryRun answers it
// without opening it. A ModelConfig of another owner that already wires the
// served model is answered, and nothing is committed.
func (s *Service) CommitWire(ctx context.Context, name, ref string, opts backend.WireOptions, target gitops.Target, dryRun bool) (*CommitView, error) {
	if s.commit == nil {
		return nil, ErrCommitUnavailable
	}
	plan, err := s.PlanWire(ctx, name, ref, opts)
	if err != nil || plan.AlreadyWired != nil {
		return &CommitView{WirePlan: plan}, err
	}
	res, err := s.commit.Commit(ctx, gitops.Request{
		Namespace: s.wiring.Namespace, Write: plan.objects, Owner: plan.GitOps, Target: target,
		Verb: "wire", Subject: plan.ModelConfig, Tool: "wire_model", DryRun: dryRun,
		Summary: fmt.Sprintf("Wires the model `%s` (backend %s) into kagent: ModelConfig `%s/%s`", plan.Model, plan.Backend, s.wiring.Namespace, plan.ModelConfig),
	})
	if err != nil {
		return nil, err
	}
	return &CommitView{WirePlan: plan, Commit: res}, nil
}

// CommitUnwire is unwire_model in commit mode: the removing pull request of
// the ModelConfig's files (and its placeholder Secret's). Nothing wired:
// nothing to commit.
func (s *Service) CommitUnwire(ctx context.Context, name, ref string, target gitops.Target, dryRun bool) (*CommitView, error) {
	if s.commit == nil {
		return nil, ErrCommitUnavailable
	}
	plan, err := s.PlanUnwire(ctx, name, ref)
	if err != nil || len(plan.objects) == 0 {
		return &CommitView{WirePlan: plan}, err
	}
	res, err := s.commit.Commit(ctx, gitops.Request{
		Namespace: s.wiring.Namespace, Remove: plan.objects, Owner: plan.GitOps, Target: target,
		Verb: "unwire", Subject: plan.ModelConfig, Tool: "unwire_model", DryRun: dryRun,
		Summary: fmt.Sprintf("Unwires the model `%s` (backend %s) from kagent: removes ModelConfig `%s/%s`", plan.Model, plan.Backend, s.wiring.Namespace, plan.ModelConfig),
	})
	if err != nil {
		return nil, err
	}
	return &CommitView{WirePlan: plan, Commit: res}, nil
}
