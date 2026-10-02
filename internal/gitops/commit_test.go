package gitops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/identity"
)

var repo = commit.Repository{Owner: "giantswarm", Name: "lab-gitops"}

func obj(apiVersion, kind, ns, name string, labels map[string]any, spec map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if ns != "" {
		meta["namespace"] = ns
	}
	if labels != nil {
		meta["labels"] = labels
	}
	o := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}
	if spec != nil {
		o["spec"] = spec
	}
	return &unstructured.Unstructured{Object: o}
}

// cluster holds the kagent namespace applied by the Kustomization flux,
// which builds ./platform from the GitRepository of lab-gitops.
func cluster(extra ...runtime.Object) dynamic.Interface {
	objs := append([]runtime.Object{
		obj("v1", "Namespace", "", "kagent", map[string]any{LabelKustomizeName: "flux", LabelKustomizeNamespace: "flux-giantswarm"}, nil),
		obj("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-giantswarm", "flux", nil, map[string]any{
			"path": "./platform", "prune": true, "sourceRef": map[string]any{"kind": "GitRepository", "name": "lab-gitops"},
		}),
		obj("source.toolkit.fluxcd.io/v1", "GitRepository", "flux-giantswarm", "lab-gitops", nil, map[string]any{
			"url": "https://github.com/giantswarm/lab-gitops", "ref": map[string]any{"branch": "main"},
		}),
	}, extra...)
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		KustomizationGVR: "KustomizationList", GitRepositoryGVR: "GitRepositoryList", HelmReleaseGVR: "HelmReleaseList", namespaceGVR: "NamespaceList",
	}, objs...)
}

func committer(dyn dynamic.Interface, fake *commit.Fake) *Committer {
	return NewCommitter(func(string) (Remote, error) { return fake, nil }, func(context.Context) dynamic.Interface { return dyn })
}

func asPerson() context.Context {
	return identity.ContextWithGitHub(context.Background(), &identity.GitHub{Login: "jane", Token: "ghu_x"})
}

func modelConfig(name string) *unstructured.Unstructured {
	return obj("kagent.dev/v1alpha3", "ModelConfig", "kagent", name, map[string]any{"app.kubernetes.io/managed-by": "model-manager"}, map[string]any{"provider": "Ollama", "model": "qwen3:0.6b"})
}

func TestCommitOpensThePullRequestAsThePerson(t *testing.T) {
	fake := commit.NewFake()
	fake.AddBranch(repo, "main", map[string][]byte{"platform/kustomization.yaml": []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- kagent.yaml\n")})
	c := committer(cluster(), fake)
	req := Request{Namespace: "kagent", Write: []*unstructured.Unstructured{modelConfig("qwen3-0-6b")}, Verb: "wire", Subject: "qwen3-0-6b", Tool: "wire_model", Summary: "Wires qwen3:0.6b"}

	dry := req
	dry.DryRun = true
	plan, err := c.Commit(asPerson(), dry)
	require.NoError(t, err)
	assert.Equal(t, "giantswarm/lab-gitops", plan.Repository)
	assert.Equal(t, "platform/model-manager", plan.Directory)
	assert.Equal(t, "flux-giantswarm/flux", plan.Kustomization)
	assert.Empty(t, plan.PullRequest)
	assert.Empty(t, fake.PullRequests(), "a dry run opens nothing")
	paths := map[string]string{}
	for _, f := range plan.Files {
		paths[f.Path] = f.Action
	}
	assert.Equal(t, map[string]string{
		"platform/model-manager/qwen3-0-6b.yaml":    "add",
		"platform/model-manager/kustomization.yaml": "add",
		"platform/kustomization.yaml":               "update",
	}, paths)

	res, err := c.Commit(asPerson(), req)
	require.NoError(t, err)
	require.NotEmpty(t, res.PullRequest)
	assert.Equal(t, "jane", res.Author)
	prs := fake.PullRequests()
	require.Len(t, prs, 1)
	assert.Equal(t, "model-manager/wire-qwen3-0-6b", prs[0].Head)
	files := fake.Files(repo, "model-manager/wire-qwen3-0-6b")
	assert.Contains(t, string(files["platform/model-manager/qwen3-0-6b.yaml"]), "kind: ModelConfig")
	assert.Contains(t, string(files["platform/kustomization.yaml"]), "model-manager")
}

func TestCommitWithoutGitHubAuthorizationNamesTheConsent(t *testing.T) {
	c := committer(cluster(), commit.NewFake())
	_, err := c.Commit(context.Background(), Request{Namespace: "kagent", Write: []*unstructured.Unstructured{modelConfig("x")}, Verb: "wire", Subject: "x"})
	require.ErrorIs(t, err, ErrAuthRequired)
	assert.Contains(t, err.Error(), "core_auth_login server=model-manager")
}

func TestCommitRefusesAPlaintextSecret(t *testing.T) {
	fake := commit.NewFake()
	fake.AddBranch(repo, "main", map[string][]byte{})
	sec := obj("v1", "Secret", "kagent", "x-api-key", nil, nil)
	sec.Object["stringData"] = map[string]any{"OPENAI_API_KEY": "placeholder"}
	_, err := committer(cluster(), fake).Commit(asPerson(), Request{Namespace: "kagent", Write: []*unstructured.Unstructured{modelConfig("x"), sec}, Verb: "wire", Subject: "x"})
	require.ErrorIs(t, err, backend.ErrInvalid)
	assert.Contains(t, err.Error(), "no .sops.yaml")
	assert.Contains(t, err.Error(), "apiKeyPassthrough")
}

func TestCommitRemovalOfAnObjectWrittenLiveIsRefused(t *testing.T) {
	fake := commit.NewFake()
	fake.AddBranch(repo, "main", map[string][]byte{})
	_, err := committer(cluster(), fake).Commit(asPerson(), Request{Namespace: "kagent", Remove: []*unstructured.Unstructured{modelConfig("x")}, Verb: "unwire", Subject: "x"})
	require.ErrorIs(t, err, backend.ErrConflict)
	assert.Contains(t, err.Error(), "written live")
}

func TestCommitRemovesTheFiles(t *testing.T) {
	fake := commit.NewFake()
	fake.AddBranch(repo, "main", map[string][]byte{
		"platform/model-manager/kustomization.yaml": []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- x.yaml\n"),
		"platform/model-manager/x.yaml":             []byte("kind: ModelConfig\n"),
	})
	res, err := committer(cluster(), fake).Commit(asPerson(), Request{Namespace: "kagent", Remove: []*unstructured.Unstructured{modelConfig("x")}, Verb: "unwire", Subject: "x"})
	require.NoError(t, err)
	require.NotEmpty(t, res.PullRequest)
	files := fake.Files(repo, "model-manager/unwire-x")
	assert.NotContains(t, files, "platform/model-manager/x.yaml")
	assert.NotContains(t, files, "platform/model-manager/kustomization.yaml", "the directory goes with its last file")
}

func TestCommitRefusesAnObjectFluxAppliesFromAnotherFile(t *testing.T) {
	fake := commit.NewFake()
	fake.AddBranch(repo, "main", map[string][]byte{"platform/models.yaml": []byte("kind: ModelConfig\n")})
	owner := &Owner{Kind: KindKustomization, Namespace: "flux-giantswarm", Name: "flux"}
	_, err := committer(cluster(), fake).Commit(asPerson(), Request{Namespace: "kagent", Write: []*unstructured.Unstructured{modelConfig("x")}, Owner: owner, Verb: "wire", Subject: "x"})
	require.ErrorIs(t, err, backend.ErrConflict)
	assert.Contains(t, err.Error(), "change it in the file it is written in")
}

func TestCommitTargets(t *testing.T) {
	fake := commit.NewFake()
	fake.AddBranch(commit.Repository{Owner: "acme", Name: "fleet"}, "prod", map[string][]byte{})
	bare := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{namespaceGVR: "NamespaceList"}, obj("v1", "Namespace", "", "kagent", nil, nil))
	c := committer(bare, fake)
	req := Request{Namespace: "kagent", Write: []*unstructured.Unstructured{modelConfig("x")}, Verb: "wire", Subject: "x", DryRun: true}

	_, err := c.Commit(asPerson(), req)
	require.ErrorIs(t, err, backend.ErrInvalid, "no provenance, no explicit target")
	assert.Contains(t, err.Error(), "pass repository, branch and path")

	req.Target = Target{Repository: "acme/fleet", Branch: "prod", Path: "clusters/a"}
	res, err := c.Commit(asPerson(), req)
	require.NoError(t, err)
	assert.Equal(t, "acme/fleet", res.Repository)
	assert.Equal(t, "clusters/a/model-manager", res.Directory)

	req.Target = Target{Repository: "acme/fleet"}
	_, err = c.Commit(asPerson(), req)
	require.ErrorIs(t, err, backend.ErrInvalid)

	helm := &Owner{Kind: KindHelmRelease, Namespace: "giantswarm", Name: "agent-platform"}
	_, err = c.Commit(asPerson(), Request{Namespace: "kagent", Write: req.Write, Owner: helm, Verb: "wire", Subject: "x"})
	require.ErrorIs(t, err, backend.ErrConflict)
	assert.True(t, strings.Contains(err.Error(), "release's values"))
}

func TestCommitRefusesAKustomizationThatMovesTheNamespace(t *testing.T) {
	ks := obj("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-giantswarm", "other", nil, map[string]any{
		"path": "./x", "targetNamespace": "default", "sourceRef": map[string]any{"kind": "GitRepository", "name": "lab-gitops"},
	})
	fake := commit.NewFake()
	fake.AddBranch(repo, "main", map[string][]byte{})
	owner := &Owner{Kind: KindKustomization, Namespace: "flux-giantswarm", Name: "other"}
	_, err := committer(cluster(ks), fake).Commit(asPerson(), Request{Namespace: "kagent", Write: []*unstructured.Unstructured{modelConfig("x")}, Owner: owner, Verb: "wire", Subject: "x", DryRun: true})
	require.ErrorIs(t, err, backend.ErrInvalid)
	assert.Contains(t, err.Error(), "targetNamespace default")
}

func TestServingObjectAndItsModelConfigGetTheirOwnFiles(t *testing.T) {
	fake := commit.NewFake()
	fake.AddBranch(repo, "main", map[string][]byte{})
	llm := obj("serving.kserve.io/v1alpha1", "LLMInferenceService", "kagent", "tiny", nil, map[string]any{"model": map[string]any{"uri": "hf://x/tiny"}})
	res, err := committer(cluster(), fake).Commit(asPerson(), Request{Namespace: "kagent", Write: []*unstructured.Unstructured{llm, modelConfig("tiny")}, Verb: "serve", Subject: "tiny", DryRun: true})
	require.NoError(t, err)
	var paths []string
	for _, f := range res.Files {
		paths = append(paths, f.Path)
	}
	assert.Contains(t, paths, "platform/model-manager/tiny-llminferenceservice.yaml")
	assert.Contains(t, paths, "platform/model-manager/tiny.yaml")
}

var fleet = commit.Repository{Owner: "giantswarm", Name: "lab-fleet"}

// remoteClusters are model-manager's own cluster — the kagent namespace of
// cluster(), a stale model-serving namespace naming a release that is gone,
// the HelmRelease rendering the target's serving namespace through the
// kubeconfig Secret wc-kubeconfig, and the Kustomizations of its namespace
// (extra) — and the target, whose model-serving namespace that release
// labels.
func remoteClusters(kustomizations ...runtime.Object) (local, target dynamic.Interface) {
	objs := append([]runtime.Object{
		obj("v1", "Namespace", "", "model-serving", map[string]any{LabelHelmName: "gone-connectivity", LabelHelmNamespace: "org-old"}, nil),
		obj("helm.toolkit.fluxcd.io/v2", "HelmRelease", "org-lab", "wc-connectivity", nil, map[string]any{"kubeConfig": map[string]any{"secretRef": map[string]any{"name": "wc-kubeconfig"}}}),
		obj("source.toolkit.fluxcd.io/v1", "GitRepository", "org-lab", "lab-fleet", nil, map[string]any{
			"url": "https://github.com/giantswarm/lab-fleet", "ref": map[string]any{"branch": "main"},
		}),
	}, kustomizations...)
	local = cluster(objs...)
	target = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{namespaceGVR: "NamespaceList"},
		obj("v1", "Namespace", "", "model-serving", map[string]any{LabelHelmName: "wc-connectivity", LabelHelmNamespace: "org-lab"}, nil))
	return local, target
}

func wcKustomization(name, secret string) runtime.Object {
	spec := map[string]any{"path": "./clusters/wc/" + name, "prune": true, "sourceRef": map[string]any{"kind": "GitRepository", "name": "lab-fleet"}}
	if secret != "" {
		spec["kubeConfig"] = map[string]any{"secretRef": map[string]any{"name": secret}}
	}
	return obj("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "org-lab", name, nil, spec)
}

func servingRequest(target dynamic.Interface) Request {
	isvc := obj("serving.kserve.io/v1alpha2", "LLMInferenceService", "model-serving", "qwen3", map[string]any{"app.kubernetes.io/managed-by": "model-manager"}, map[string]any{"model": map[string]any{"name": "qwen3"}})
	return Request{
		Namespace: "model-serving", Write: []*unstructured.Unstructured{isvc},
		Cluster: &Cluster{Name: "wc", Dynamic: func(context.Context) dynamic.Interface { return target }},
		Also:    []Part{{Namespace: "kagent", Write: []*unstructured.Unstructured{modelConfig("qwen3")}}},
		Verb:    "serve", Subject: "qwen3", Tool: "load_model", Summary: "Serves qwen3",
	}
}

// TestCommitOnARemoteTarget resolves the serving part on the target — its
// namespace read there, the Kustomization applying through the target's
// kubeconfig Secret — and the ModelConfig on model-manager's own cluster:
// two repositories, two pull requests.
func TestCommitOnARemoteTarget(t *testing.T) {
	local, target := remoteClusters(wcKustomization("wc-out-of-band", "wc-kubeconfig"), wcKustomization("mc-apps", ""), wcKustomization("other-wc", "other-kubeconfig"))
	fake := commit.NewFake()
	fake.AddBranch(repo, "main", map[string][]byte{"platform/kustomization.yaml": []byte("resources: []\n")})
	fake.AddBranch(fleet, "main", map[string][]byte{"clusters/wc/wc-out-of-band/kustomization.yaml": []byte("resources: []\n")})
	c := committer(local, fake)

	dry := servingRequest(target)
	dry.DryRun = true
	plan, err := c.Commit(asPerson(), dry)
	require.NoError(t, err)
	assert.Equal(t, "giantswarm/lab-fleet", plan.Repository)
	assert.Equal(t, "clusters/wc/wc-out-of-band/model-manager", plan.Directory)
	assert.Equal(t, "org-lab/wc-out-of-band", plan.Kustomization)
	assert.Equal(t, "wc", plan.Cluster)
	require.Len(t, plan.Also, 1)
	assert.Equal(t, "giantswarm/lab-gitops", plan.Also[0].Repository)
	assert.Equal(t, "platform/model-manager", plan.Also[0].Directory)
	assert.Empty(t, plan.Also[0].Cluster)
	assert.Empty(t, fake.PullRequests())

	res, err := c.Commit(asPerson(), servingRequest(target))
	require.NoError(t, err)
	require.NotEmpty(t, res.PullRequest)
	require.NotEmpty(t, res.Also[0].PullRequest)
	assert.NotEqual(t, res.PullRequest, res.Also[0].PullRequest)
	assert.Len(t, fake.PullRequests(), 2)
	assert.Contains(t, string(fake.Files(fleet, "model-manager/serve-qwen3")["clusters/wc/wc-out-of-band/model-manager/qwen3-llminferenceservice.yaml"]), "kind: LLMInferenceService")
	assert.Contains(t, string(fake.Files(repo, "model-manager/serve-qwen3")["platform/model-manager/qwen3.yaml"]), "kind: ModelConfig")
}

func TestCommitOnARemoteTargetNeedsOneKustomizationApplyingThere(t *testing.T) {
	for name, ks := range map[string][]runtime.Object{
		"none":    {wcKustomization("mc-apps", "")},
		"several": {wcKustomization("wc-a", "wc-kubeconfig"), wcKustomization("wc-b", "wc-kubeconfig")},
	} {
		local, target := remoteClusters(ks...)
		fake := commit.NewFake()
		_, err := committer(local, fake).Commit(asPerson(), servingRequest(target))
		require.ErrorIs(t, err, backend.ErrInvalid, name)
		assert.Contains(t, err.Error(), "wc-kubeconfig", name)
		assert.Contains(t, err.Error(), "pass repository, branch and path", name)
		assert.Empty(t, fake.PullRequests(), name)
	}
}

func TestCommitOnARemoteTargetRefusesAKustomizationApplyingLocally(t *testing.T) {
	local, _ := remoteClusters(wcKustomization("mc-apps", ""))
	target := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{namespaceGVR: "NamespaceList"},
		obj("v1", "Namespace", "", "model-serving", map[string]any{LabelKustomizeName: "mc-apps", LabelKustomizeNamespace: "org-lab"}, nil))
	_, err := committer(local, commit.NewFake()).Commit(asPerson(), servingRequest(target))
	require.ErrorIs(t, err, backend.ErrInvalid)
	assert.Contains(t, err.Error(), "applies to model-manager's own cluster")
}

// staleNamespace is a namespace whose Flux labels name an owner that is gone:
// what disable_model_serving leaves behind.
func staleNamespace(name string, labels map[string]any) dynamic.Interface {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{namespaceGVR: "NamespaceList"},
		obj("v1", "Namespace", "", name, labels, nil))
}

func TestCommitRefusesANamespaceWhoseOwnerIsGone(t *testing.T) {
	gone := map[string]map[string]any{
		"HelmRelease":   {LabelHelmName: "wc-connectivity", LabelHelmNamespace: "org-old"},
		"Kustomization": {LabelKustomizeName: "wc-apps", LabelKustomizeNamespace: "org-old"},
	}
	for kind, labels := range gone {
		t.Run("local "+kind, func(t *testing.T) {
			fake := commit.NewFake()
			local := cluster(obj("v1", "Namespace", "", "model-serving", labels, nil))
			isvc := obj("serving.kserve.io/v1alpha2", "LLMInferenceService", "model-serving", "qwen3", nil, nil)
			_, err := committer(local, fake).Commit(asPerson(), Request{Namespace: "model-serving", Write: []*unstructured.Unstructured{isvc}, Verb: "serve", Subject: "qwen3"})
			require.ErrorIs(t, err, backend.ErrInvalid)
			assert.Contains(t, err.Error(), "namespace model-serving on model-manager's own cluster")
			assert.Contains(t, err.Error(), kind+" org-old/")
			assert.Contains(t, err.Error(), "does not exist")
			assert.Contains(t, err.Error(), "pass repository, branch and path")
			assert.Empty(t, fake.PullRequests())
		})
		t.Run("remote "+kind, func(t *testing.T) {
			local, _ := remoteClusters(wcKustomization("wc-out-of-band", "wc-kubeconfig"))
			fake := commit.NewFake()
			_, err := committer(local, fake).Commit(asPerson(), servingRequest(staleNamespace("model-serving", labels)))
			require.ErrorIs(t, err, backend.ErrInvalid)
			assert.Contains(t, err.Error(), "namespace model-serving on cluster wc")
			assert.Contains(t, err.Error(), kind+" org-old/")
			assert.Contains(t, err.Error(), "pass repository, branch and path")
			assert.Empty(t, fake.PullRequests())
		})
	}
}

func TestCommitKeepsOtherOwnerReadErrors(t *testing.T) {
	local, target := remoteClusters(wcKustomization("wc-out-of-band", "wc-kubeconfig"))
	local.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "helmreleases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(HelmReleaseGVR.GroupResource(), "wc-connectivity", errors.New("no"))
	})
	_, err := committer(local, commit.NewFake()).Commit(asPerson(), servingRequest(target))
	require.Error(t, err)
	assert.NotErrorIs(t, err, backend.ErrInvalid)
	assert.True(t, apierrors.IsForbidden(err))
}
