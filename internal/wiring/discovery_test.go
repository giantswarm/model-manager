package wiring

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/giantswarm/model-manager/internal/backend"
)

func TestDiscoverAPIVersionPrefersAPIKagentDev(t *testing.T) {
	cases := map[string]struct {
		served  []*metav1.APIResourceList
		version string
		want    string
		wantErr string
	}{
		"a set version, both groups serving it: the new group wins": {
			served:  []*metav1.APIResourceList{apiResources("kagent.dev/v1alpha3", "modelconfigs"), apiResources("api.kagent.dev/v1alpha3", "modelconfigs")},
			version: "v1alpha3",
			want:    "api.kagent.dev/v1alpha3",
		},
		"a set version only the old group serves": {
			served:  []*metav1.APIResourceList{apiResources("kagent.dev/v1alpha3", "modelconfigs")},
			version: "v1alpha3",
			want:    "kagent.dev/v1alpha3",
		},
		"a set version no group serves": {
			served:  []*metav1.APIResourceList{apiResources("kagent.dev/v1alpha2", "modelconfigs")},
			version: "v1alpha3",
			wantErr: "has no modelconfigs resource at v1alpha3",
		},
		"kagent 1.3 without the old CRD": {
			served: []*metav1.APIResourceList{apiResources("api.kagent.dev/v1alpha3", "agents", "modelconfigs")},
			want:   "api.kagent.dev/v1alpha3",
		},
		"kagent 1.3 with the old CRD left in place: the new group wins": {
			served: []*metav1.APIResourceList{apiResources("kagent.dev/v1alpha3", "agents", "modelconfigs"), apiResources("api.kagent.dev/v1alpha3", "agents", "modelconfigs")},
			want:   "api.kagent.dev/v1alpha3",
		},
		"kagent API v2 before 1.3": {
			served: []*metav1.APIResourceList{apiResources("kagent.dev/v1alpha3", "agenttemplates", "modelconfigs")},
			want:   "kagent.dev/v1alpha3",
		},
		"kagent 0.x prefers v1alpha2 and still serves v1alpha1": {
			served: []*metav1.APIResourceList{apiResources("kagent.dev/v1alpha2", "agents", "modelconfigs"), apiResources("kagent.dev/v1alpha1", "agents", "modelconfigs")},
			want:   "kagent.dev/v1alpha2",
		},
		"the preferred version lacks the resource, another has it": {
			served: []*metav1.APIResourceList{apiResources("kagent.dev/v1alpha3", "agenttemplates"), apiResources("kagent.dev/v1alpha2", "modelconfigs")},
			want:   "kagent.dev/v1alpha2",
		},
		"api.kagent.dev without modelconfigs": {
			served: []*metav1.APIResourceList{apiResources("api.kagent.dev/v1alpha3", "agents"), apiResources("kagent.dev/v1alpha3", "modelconfigs")},
			want:   "kagent.dev/v1alpha3",
		},
		"no group serves modelconfigs": {
			served:  []*metav1.APIResourceList{apiResources("api.kagent.dev/v1alpha3", "agents"), apiResources("kagent.dev/v1alpha3", "agenttemplates")},
			wantErr: "API group api.kagent.dev, kagent.dev has no modelconfigs resource",
		},
		"kagent is not installed": {
			served:  []*metav1.APIResourceList{apiResources("apps/v1", "deployments")},
			wantErr: "not found (is kagent installed?)",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dc := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: tc.served}}
			got, err := DiscoverAPIVersion(dc, tc.version)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseAPIVersion(t *testing.T) {
	for _, v := range []string{"api.kagent.dev/v1alpha3", "kagent.dev/v1alpha3", "kagent.dev/v1alpha2"} {
		gv, err := ParseAPIVersion(v)
		require.NoError(t, err, v)
		assert.Equal(t, v, gv.String())
	}
	for _, v := range []string{"v1alpha3", "kagent.io/v1alpha3", "api.kagent.dev/", "api.kagent.dev/v1/x"} {
		_, err := ParseAPIVersion(v)
		require.Error(t, err, v)
	}
}

func TestParseAPIVersionSetting(t *testing.T) {
	cases := map[string]struct {
		want     APIVersionSetting
		fallback string
	}{
		"":                        {APIVersionSetting{}, DefaultAPIVersion},
		"auto":                    {APIVersionSetting{}, DefaultAPIVersion},
		"v1alpha3":                {APIVersionSetting{Version: "v1alpha3"}, "api.kagent.dev/v1alpha3"},
		"kagent.dev/v1alpha3":     {APIVersionSetting{Pinned: "kagent.dev/v1alpha3"}, "kagent.dev/v1alpha3"},
		"api.kagent.dev/v1alpha3": {APIVersionSetting{Pinned: "api.kagent.dev/v1alpha3"}, "api.kagent.dev/v1alpha3"},
	}
	for in, tc := range cases {
		got, err := ParseAPIVersionSetting(in)
		require.NoError(t, err, in)
		assert.Equal(t, tc.want, got, in)
		assert.Equal(t, tc.fallback, got.Fallback(), in)
	}
	for _, in := range []string{"alpha3", "v0", "kagent.io/v1alpha3", "v1alpha3 "} {
		_, err := ParseAPIVersionSetting(in)
		require.Error(t, err, in)
	}
}

// TestDefaultAPIVersionIsTheNewGroup: what the service runs on when
// discovery fails (cmd/serve falls back to DefaultAPIVersion) or is bypassed.
func TestDefaultAPIVersionIsTheNewGroup(t *testing.T) {
	assert.Equal(t, "api.kagent.dev/v1alpha3", DefaultAPIVersion)
	k := NewKagent(nil, nil, "kagent", "", "", testInstance)
	assert.Equal(t, DefaultAPIVersion, k.APIVersion())
}

// cluster serves ModelConfigs in a set of group/versions, as discovery lists
// them: a call at any other one fails NotFound, as the apiserver answers a
// group/version no CRD serves.
type cluster struct {
	served      map[string]bool
	discovery   *discoveryfake.FakeDiscovery
	client      *dynamicfake.FakeDynamicClient
	misses      int
	discoveries int
}

func newCluster(served ...string) *cluster {
	c := &cluster{discovery: &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{}}}
	c.client = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: LegacyKagentGroup, Version: "v1alpha2", Resource: ModelConfigResource}: "ModelConfigList",
		legacyGVR: "ModelConfigList",
		testGVR:   "ModelConfigList",
		secretGVR: "SecretList",
	})
	c.client.PrependReactor("*", ModelConfigResource, func(a clienttesting.Action) (bool, runtime.Object, error) {
		if gv := a.GetResource().GroupVersion().String(); !c.served[gv] {
			c.misses++
			return true, nil, apierrors.NewGenericServerResponse(404, a.GetVerb(), a.GetResource().GroupResource(), "", "", 0, true)
		}
		return false, nil, nil
	})
	c.serve(served...)
	return c
}

// serve makes the cluster serve ModelConfigs in exactly the given
// group/versions.
func (c *cluster) serve(served ...string) {
	c.served = map[string]bool{}
	c.discovery.Resources = nil
	for _, gv := range served {
		c.served[gv] = true
		c.discovery.Resources = append(c.discovery.Resources, apiResources(gv, "agents", ModelConfigResource))
	}
}

func (c *cluster) discover() (string, error) {
	c.discoveries++
	return DiscoverAPIVersion(c.discovery, "")
}

func (c *cluster) wirer(apiVersion string) *Kagent {
	return NewKagent(c.client, servedOpenAPI(testAPIVersion, kagentOllamaFields...), "kagent", apiVersion, "", testInstance).
		WithDiscovery(c.discover, nil)
}

// keylessOllama is an Ollama endpoint whose write needs no served schema.
func keylessOllama(model string) backend.AgentEndpoint {
	ep := ollamaEndpoint(model)
	ep.Think = nil
	return ep
}

// TestWiringMovesToTheNewGroupOnceItAppears: kagent 1.3 adds api.kagent.dev
// under a running process and leaves kagent.dev served, so no call misses;
// the wirer moves within discoveryTTL, and discovers no more often.
func TestWiringMovesToTheNewGroupOnceItAppears(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		c := newCluster(legacyAPIVersion)
		k := c.wirer(legacyAPIVersion)

		before, err := k.Ensure(ctx, "smollm2:135m", keylessOllama("smollm2:135m"))
		require.NoError(t, err)
		assert.Equal(t, legacyAPIVersion, before.APIVersion)

		c.serve(legacyAPIVersion, testAPIVersion)
		time.Sleep(discoveryTTL - time.Second)
		still, err := k.Ensure(ctx, "qwen2.5:0.5b", keylessOllama("qwen2.5:0.5b"))
		require.NoError(t, err)
		assert.Equal(t, legacyAPIVersion, still.APIVersion, "within discoveryTTL the version in use holds")
		assert.Zero(t, c.discoveries)

		time.Sleep(time.Second)
		after, err := k.Ensure(ctx, "qwen2.5:0.5b", keylessOllama("qwen2.5:0.5b"))
		require.NoError(t, err)
		assert.Equal(t, testAPIVersion, after.APIVersion)
		assert.Equal(t, testAPIVersion, k.APIVersion())
		assert.Equal(t, 1, c.discoveries)
		assert.Zero(t, c.misses, "the old group never stopped answering")

		refs, err := k.List(ctx)
		require.NoError(t, err)
		require.Len(t, refs, 1, "the live kagent.dev ModelConfigs are not part of the view")
		assert.Equal(t, testAPIVersion, refs[0].APIVersion)
		assert.Equal(t, 1, c.discoveries, "the next discovery waits for discoveryTTL")
	})
}

// TestASetVersionStillDiscoversTheGroup: the platform sets the version
// (v1alpha3) and leaves the group to discovery, so a process started before
// kagent 1.3 moves to api.kagent.dev like one on auto.
func TestASetVersionStillDiscoversTheGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCluster(legacyAPIVersion)
		setting, err := ParseAPIVersionSetting("v1alpha3")
		require.NoError(t, err)
		discover := func() (string, error) { c.discoveries++; return DiscoverAPIVersion(c.discovery, setting.Version) }
		start, err := discover()
		require.NoError(t, err)
		k := NewKagent(c.client, servedOpenAPI(testAPIVersion, kagentOllamaFields...), "kagent", start, "", testInstance).WithDiscovery(discover, nil)
		assert.Equal(t, legacyAPIVersion, k.APIVersion())

		c.serve(legacyAPIVersion, testAPIVersion)
		time.Sleep(discoveryTTL)
		ref, err := k.Ensure(t.Context(), "qwen3:0.6b", keylessOllama("qwen3:0.6b"))
		require.NoError(t, err)
		assert.Equal(t, testAPIVersion, ref.APIVersion)
	})
}

// TestWiringFollowsACRDVersionCutOver: a call that misses the version in use
// re-discovers at once, without waiting for discoveryTTL.
func TestWiringFollowsACRDVersionCutOver(t *testing.T) {
	ctx := t.Context()
	c := newCluster("kagent.dev/v1alpha2")
	k := c.wirer("kagent.dev/v1alpha2")

	before, err := k.Ensure(ctx, "smollm2:135m", keylessOllama("smollm2:135m"))
	require.NoError(t, err)
	assert.Equal(t, "kagent.dev/v1alpha2", before.APIVersion)

	c.serve(testAPIVersion)
	after, err := k.Ensure(ctx, "qwen2.5:0.5b", keylessOllama("qwen2.5:0.5b"))
	require.NoError(t, err, "the first call after the cut-over re-discovers and retries")
	assert.Positive(t, c.misses, "the call failed NotFound at the old version")
	assert.Equal(t, testAPIVersion, after.APIVersion)
	assert.Equal(t, testAPIVersion, k.APIVersion())

	misses := c.misses
	refs, err := k.List(ctx)
	require.NoError(t, err)
	assert.Len(t, refs, 1, "the new version's ModelConfigs")
	assert.Equal(t, misses+1, c.misses, "later calls go to the new version at once; the kagent.dev look-up misses")
}

func TestWiringRediscoversWhenTheSchemaVersionIsGone(t *testing.T) {
	c := newCluster(testAPIVersion)
	k := c.wirer(legacyAPIVersion)

	ep, err := k.Writable(t.Context(), ollamaEndpoint("qwen3:0.6b"))
	require.NoError(t, err)
	assert.Equal(t, new(false), ep.Think, "read from the api.kagent.dev schema")
	assert.Equal(t, testAPIVersion, k.APIVersion())
}

func TestAnExplicitAPIVersionNeverRediscovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCluster(testAPIVersion)
		k := NewKagent(c.client, servedOpenAPI(testAPIVersion, kagentOllamaFields...), "kagent", legacyAPIVersion, "", testInstance)

		time.Sleep(discoveryTTL)
		_, err := k.List(t.Context())
		require.Error(t, err)
		assert.True(t, apierrors.IsNotFound(err))
		assert.Equal(t, legacyAPIVersion, k.APIVersion())
		assert.Zero(t, c.discoveries)
	})
}

func TestARediscoveryOfTheSameVersionKeepsTheError(t *testing.T) {
	c := newCluster(legacyAPIVersion)
	calls := 0
	k := NewKagent(c.client, servedOpenAPI(testAPIVersion, kagentOllamaFields...), "kagent", legacyAPIVersion, "", testInstance).
		WithDiscovery(func() (string, error) { calls++; return legacyAPIVersion, nil }, nil)
	c.serve()

	_, err := k.List(t.Context())
	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, 1, c.misses, "no retry at an unchanged version")
}

// atLegacy is obj as written before the cut-over: at kagent.dev.
func atLegacy(obj *unstructured.Unstructured) *unstructured.Unstructured {
	obj.SetAPIVersion(legacyAPIVersion)
	return obj
}

// TestAGitOpsModelConfigAtKagentDevMovesInGit: one Flux applies at
// kagent.dev stays in the view after the move to api.kagent.dev; it is never
// shadowed by a live twin, and the write is the same file in git at the new
// group.
func TestAGitOpsModelConfigAtKagentDevMovesInGit(t *testing.T) {
	ctx := t.Context()
	k, client := newFakeKagent(t, atLegacy(committed("qwen3-0-6b-gguf", "Qwen3-0.6B-GGUF", kustomizeLabels)), applying(inventoryID(LegacyKagentGroup, "qwen3-0-6b-gguf")))

	r, err := k.Render(ctx, "Qwen3-0.6B-GGUF", lemonadeEndpoint("Qwen3-0.6B-GGUF"))
	require.NoError(t, err)
	assert.Equal(t, "qwen3-0-6b-gguf", r.Name)
	require.NotNil(t, r.GitOps, "the commit goes to the Kustomization applying it")
	assert.Equal(t, "flux", r.GitOps.Name)
	assert.Equal(t, testAPIVersion, r.ModelConfig().GetAPIVersion())

	_, err = k.Ensure(ctx, "Qwen3-0.6B-GGUF", lemonadeEndpoint("Qwen3-0.6B-GGUF"))
	require.ErrorIs(t, err, backend.ErrGitOpsOwned)
	list, err := client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items, "no live twin at api.kagent.dev")

	ref, err := k.Lookup(ctx, backend.NameLemonade, "Qwen3-0.6B-GGUF")
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, legacyAPIVersion, ref.APIVersion)
	assert.NotNil(t, ref.GitOps)
	for _, list := range []func() ([]ModelConfigRef, error){func() ([]ModelConfigRef, error) { return k.List(ctx) }, func() ([]ModelConfigRef, error) { return k.ListAll(ctx) }} {
		refs, err := list()
		require.NoError(t, err)
		require.Len(t, refs, 1)
		assert.Equal(t, legacyAPIVersion, refs[0].APIVersion)
	}

	removal, err := k.Removal(ctx, backend.NameLemonade, "Qwen3-0.6B-GGUF", CreatedHere)
	require.NoError(t, err)
	require.NotNil(t, removal.ModelConfig())
	assert.NotNil(t, removal.GitOps)
	require.ErrorIs(t, k.Remove(ctx, backend.NameLemonade, "Qwen3-0.6B-GGUF", CreatedHere), backend.ErrGitOpsOwned)
}

// TestALiveModelConfigAtKagentDevGetsATwin: model-manager's own live
// kagent.dev ModelConfig is not read by kagent 1.3; the write lands at
// api.kagent.dev and the old one is left to go with its CRD.
func TestALiveModelConfigAtKagentDevGetsATwin(t *testing.T) {
	ctx := t.Context()
	live := atLegacy(committed("qwen3-0-6b-gguf", "Qwen3-0.6B-GGUF", map[string]any{InstanceLabel: testInstance}))
	k, client := newFakeKagent(t, live)

	ref, err := k.Ensure(ctx, "Qwen3-0.6B-GGUF", lemonadeEndpoint("Qwen3-0.6B-GGUF"))
	require.NoError(t, err)
	assert.Equal(t, testAPIVersion, ref.APIVersion)
	assert.Equal(t, "qwen3-0-6b-gguf", ref.Name)

	refs, err := k.List(ctx)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, testAPIVersion, refs[0].APIVersion)
	_, err = client.Resource(legacyGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b-gguf", metav1.GetOptions{})
	require.NoError(t, err, "the kagent.dev one is left in place")
}

// TestAKagentDevGroupTheCallerMayNotReadIsNone: in caller-only mode a
// caller whose RBAC has api.kagent.dev only still wires.
func TestAKagentDevGroupTheCallerMayNotReadIsNone(t *testing.T) {
	ctx := t.Context()
	k, client := newFakeKagent(t)
	client.PrependReactor("list", ModelConfigResource, func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Group == LegacyKagentGroup {
			return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", nil)
		}
		return false, nil, nil
	})

	_, err := k.Ensure(ctx, "smollm2:135m", keylessOllama("smollm2:135m"))
	require.NoError(t, err)
	refs, err := k.List(ctx)
	require.NoError(t, err)
	assert.Len(t, refs, 1)
}
