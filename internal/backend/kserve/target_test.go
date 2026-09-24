package kserve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/identity"
)

const testModelsHost = "https://models.gpu01.example"

var testTarget = backend.Target{Cluster: "gpu01", Organization: "giantswarm", APIServer: "https://api.gpu01.example:6443", CABundle: "ca", ServingNamespace: testServingNS}

// remoteFixture is the driver of an installation whose kserve backend targets
// a workload cluster, as cmd's backendBuilder wires it: the fixture's clients
// are the target reached with the caller's token; the configured clients are
// the target without a credential — the apiserver refuses every call on them
// as system:anonymous, and the test counts them.
type remoteFixture struct {
	*fixture
	anonCS  *kubefake.Clientset
	anonDyn *dynamicfake.FakeDynamicClient
}

func newRemoteFixture(t *testing.T) *remoteFixture {
	t.Helper()
	f := newFixture(t)
	anonCS := kubefake.NewSimpleClientset()
	anonDyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), llmisvcListKinds())
	deny := func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: a.GetResource().Resource}, "", errors.New(`User "system:anonymous" cannot do this`))
	}
	anonCS.PrependReactor("*", "*", deny)
	anonDyn.PrependReactor("*", "*", deny)
	clientsFor := func(ctx context.Context) (kubernetes.Interface, dynamic.Interface) {
		if _, ok := identity.TokenFromContext(ctx); ok {
			return f.cs, f.dyn
		}
		return anonCS, anonDyn
	}
	for _, o := range []*backend.KServeOptions{&f.b.opts, &f.b.cfg.opts} {
		o.Clientset, o.Dynamic, o.ClientsFor, o.Target = anonCS, anonDyn, clientsFor, testTarget
	}
	f.b.cs, f.b.dyn = anonCS, anonDyn
	f.setDiscoveryOpts(context.Background(), discoveryOpts{gateway: testModelsHost})
	return &remoteFixture{fixture: f, anonCS: anonCS, anonDyn: anonDyn}
}

// assertNothingAnonymous: no call reached the target without the caller.
func (rf *remoteFixture) assertNothingAnonymous(t *testing.T) {
	t.Helper()
	assert.Empty(t, rf.anonCS.Actions(), "typed calls made without the caller's token")
	assert.Empty(t, rf.anonDyn.Actions(), "dynamic calls made without the caller's token")
}

func (rf *remoteFixture) created(resource string) []runtime.Object {
	var out []runtime.Object
	for _, a := range rf.cs.Actions() {
		if c, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == resource {
			out = append(out, c.GetObject())
		}
	}
	return out
}

func callerContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(identity.ContextWithToken(context.Background(), "caller-token"), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestRemoteTargetActsAsTheCaller: with a workload cluster as the target,
// every read and write — the discovery ConfigMap, presets, nodes, the cache
// claim, the pod-mode inventory scan, the download Job, the cache index, the
// LLMInferenceService — lands on the target with the caller's token, and the
// agents' endpoint is the target's models host with the token forwarded.
func TestRemoteTargetActsAsTheCaller(t *testing.T) {
	rf := newRemoteFixture(t)
	ctx := callerContext(t)

	info := rf.b.Info(ctx)
	assert.True(t, info.Healthy, info.Message)
	assert.Empty(t, info.Message)
	assert.Equal(t, &backend.Target{Cluster: "gpu01", Organization: "giantswarm"}, info.Target, "cluster and organization only")
	assert.Equal(t, testModelsHost, info.AgentEndpoint, "the descriptor names the target's models host")
	assert.Equal(t, testModelsHost, rf.b.cfg.settings(ctx).GatewayEndpoint, "read from the target's discovery ConfigMap")

	presets, err := rf.b.ListPresets(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, presets, "the presets are the target's")
	nodes, err := rf.b.ListNodes(ctx)
	require.NoError(t, err)
	var names []string
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	assert.Contains(t, names, testGPUNode, "the nodes are the target's")

	// The pod-mode inventory: the real scan, a Job on the target read back
	// through the target apiserver.
	rf.b.scan = rf.b.scanNode
	rf.b.inv.invalidate()
	rf.completePods(ctx)
	scanOut := lineDir + "\ttiny\t100\t3\t0\t1\n" + lineEnd + "\n"
	rf.b.logs = func(context.Context, string, string, corev1.PodLogOptions) (string, error) { return scanOut, nil }
	_, err = rf.b.ListModels(ctx)
	require.NoError(t, err)
	var scanJobs int
	for _, o := range rf.created("jobs") {
		if j := o.(*batchv1.Job); len(j.Name) > len(scanPrefix) && j.Name[:len(scanPrefix)] == scanPrefix {
			scanJobs++
		}
	}
	assert.Positive(t, scanJobs, "the inventory scan pod runs on the target")

	// The download Job and the cache index.
	rf.b.scan = func(context.Context, string) ([]cacheEntry, string, error) { return nil, testCacheNode, nil }
	rf.b.inv.invalidate()
	plan := downloadPlan{Dir: "tiny"}
	rf.completeJob(ctx, plan.jobName(), "INFO start\nPROGRESS 1000\n")
	require.NoError(t, rf.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo}, nil))
	var pulled bool
	for _, o := range rf.created("jobs") {
		pulled = pulled || o.(*batchv1.Job).Name == plan.jobName()
	}
	assert.True(t, pulled, "the download Job runs on the target")

	// The LLMInferenceService, and the endpoint agents on the installation
	// are wired to: the models host, the caller's token forwarded, no
	// placeholder key.
	require.NoError(t, rf.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))
	assert.NotNil(t, rf.llmisvc(ctx, "tiny"), "the LLMInferenceService is composed on the target")
	_, err = rf.b.ListModels(ctx)
	require.NoError(t, err)
	_, err = rf.cs.CoreV1().ConfigMaps(testServingNS).Get(ctx, DefaultCacheIndexConfigMap, metav1.GetOptions{})
	assert.NoError(t, err, "the cache index is recorded on the target")
	ep := rf.b.AgentEndpoint(tinyRepo)
	assert.Equal(t, testModelsHost+"/"+testServingNS+"/tiny/v1", ep.BaseURL)
	assert.True(t, ep.APIKeyPassthrough)
	assert.False(t, ep.PlaceholderAPIKey)

	rf.assertNothingAnonymous(t)
}

// TestRemoteTargetCancelledPullCleansUpAsTheCaller: the Job of a cancelled
// download is deleted with the caller's token, not anonymously.
func TestRemoteTargetCancelledPullCleansUpAsTheCaller(t *testing.T) {
	rf := newRemoteFixture(t)
	ctx, cancel := context.WithCancel(callerContext(t))
	done := make(chan error, 1)
	go func() { done <- rf.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo}, nil) }()
	plan := downloadPlan{Dir: "tiny"}
	require.Eventually(t, func() bool {
		_, err := rf.cs.BatchV1().Jobs(testServingNS).Get(context.Background(), plan.jobName(), metav1.GetOptions{})
		return err == nil
	}, 2*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("pull did not stop on cancel")
	}
	_, err := rf.cs.BatchV1().Jobs(testServingNS).Get(context.Background(), plan.jobName(), metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "the cancelled Job is deleted on the target: %v", err)
	rf.assertNothingAnonymous(t)
}

// TestRemoteTargetDetachedWork: a background rescan runs as the caller who
// caused it, past the call's cancellation; without a caller it is skipped
// with the reason, never run anonymously.
func TestRemoteTargetDetachedWork(t *testing.T) {
	rf := newRemoteFixture(t)
	got := rf.b.refreshInventory(context.Background())
	assert.False(t, got.Refreshing)
	assert.Contains(t, got.Reason, "no background cache scan on the remote target gpu01 without a caller")
	rf.assertNothingAnonymous(t)

	type scanCall struct {
		caller    bool
		cancelled bool
	}
	scans := make(chan scanCall, 4)
	rf.b.scan = func(ctx context.Context, node string) ([]cacheEntry, string, error) {
		_, caller := identity.TokenFromContext(ctx)
		scans <- scanCall{caller: caller, cancelled: ctx.Err() != nil}
		return nil, node, nil
	}
	ctx, cancel := context.WithCancel(callerContext(t))
	got = rf.b.refreshInventory(ctx)
	cancel()
	assert.True(t, got.Refreshing, got.Reason)
	select {
	case s := <-scans:
		assert.True(t, s.caller, "the background scan presents the caller's token")
		assert.False(t, s.cancelled, "and outlives the call that caused it")
	case <-time.After(3 * time.Second):
		t.Fatal("no background scan")
	}
	rf.assertNothingAnonymous(t)
}

// TestRemoteTargetRefusalIsNotRetriedAnonymously: a caller the target does
// not accept gets the target's refusal; the credential-less client is never
// tried behind it (it would mask the 401 with an anonymous 403).
func TestRemoteTargetRefusalIsNotRetriedAnonymously(t *testing.T) {
	rf := newRemoteFixture(t)
	cs, dyn := unauthorizedClients()
	clientsFor := func(ctx context.Context) (kubernetes.Interface, dynamic.Interface) {
		if _, ok := identity.TokenFromContext(ctx); ok {
			return cs, dyn
		}
		return rf.anonCS, rf.anonDyn
	}
	rf.b.opts.ClientsFor, rf.b.cfg.opts.ClientsFor = clientsFor, clientsFor
	rf.resetSettings()

	info := rf.b.Info(callerContext(t))
	assert.False(t, info.Healthy)
	assert.Contains(t, info.Message, "token expired")
	assert.NotContains(t, info.Message, "system:anonymous")
	rf.assertNothingAnonymous(t)
}

// TestRemoteTargetWithoutGateway: a target whose discovery document enables
// no models Gateway says so — agents on the installation cannot reach a model
// served on the target's cluster-local address.
func TestRemoteTargetWithoutGateway(t *testing.T) {
	rf := newRemoteFixture(t)
	ctx := callerContext(t)
	rf.setDiscoveryOpts(ctx, discoveryOpts{})
	info := rf.b.Info(ctx)
	assert.Empty(t, info.AgentEndpoint)
	assert.Contains(t, info.Message, "the discovery document on gpu01 enables no models Gateway")
	rf.assertNothingAnonymous(t)
}

func TestNewRefusesDaemonSetInventoryForARemoteTarget(t *testing.T) {
	opts := backend.KServeOptions{
		Clientset:     kubefake.NewSimpleClientset(),
		Dynamic:       dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		InventoryMode: InventoryModeDaemonSet,
		Target:        testTarget,
	}
	_, err := New(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kserve target gpu01: inventory mode daemonset dials the cache agents' pod IPs")

	opts.Target = backend.Target{Cluster: backend.TargetLocal}
	_, err = New(opts)
	assert.NoError(t, err, "the daemonset inventory stays for the local cluster")
}
