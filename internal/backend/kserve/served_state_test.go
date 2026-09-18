package kserve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// pendingLLMISVC creates the LLMInferenceService of a preset as KServe leaves
// it while nothing serves yet: Ready False for the route, the workloads not
// ready.
func (f *fixture) pendingLLMISVC(ctx context.Context, preset string) *unstructured.Unstructured {
	f.t.Helper()
	obj := f.b.composeLLM(mustPreset(f.t, f, preset), f.b.cfg.settings(ctx), "")
	obj.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": "False", "reason": "HTTPRoutesNotReady", "message": "HTTPRoute is not ready"},
		map[string]any{"type": "WorkloadsReady", "status": "False", "reason": "WorkloadsNotReady", "message": "Deployment has 0 ready replicas"},
	}}
	created, err := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Create(ctx, obj, metav1.CreateOptions{})
	require.NoError(f.t, err)
	return created
}

// workloadPod is the workload pod KServe's controller derives from an
// LLMInferenceService, in the given phase.
func workloadPod(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-kserve-workload-7f9c4", Namespace: testServingNS, Labels: map[string]string{"app.kubernetes.io/part-of": "llminferenceservice", llmisvcPodLabel: name}},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

// The scenario of giantswarm/model-manager#92: a serving object whose
// predictor never got a node — a pool at scale-to-zero, a node without an
// allocatable GPU — is listed with its state and reason and can be unloaded
// by preset name and by repository id.
func TestPendingLLMInferenceServiceIsListedAndUnloaded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serveLLMAPI()
	f.pendingLLMISVC(ctx, "tiny")

	// No pod yet (the Deployment is not admitted): the object's own condition.
	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1, "a serving object that is not Ready is listed")
	assert.Equal(t, tinyRepo, loaded[0].Name)
	assert.Equal(t, "tiny", loaded[0].Resource)
	assert.Equal(t, kindLLMInferenceService, loaded[0].Kind)
	assert.Equal(t, statusNotReady, loaded[0].Status)
	assert.Equal(t, "HTTPRoutesNotReady", loaded[0].Reason)
	assert.Equal(t, "HTTPRoutesNotReady HTTPRoute is not ready", loaded[0].Message)

	// The workload pod waits for a node: Pending, with the scheduler's reason.
	pod := workloadPod("tiny", corev1.PodPending)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/3 nodes are available: 3 Insufficient nvidia.com/gpu."}}
	pods := f.cs.CoreV1().Pods(testServingNS)
	_, err = pods.Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, statusPending, loaded[0].Status)
	assert.Equal(t, corev1.PodReasonUnschedulable, loaded[0].Reason)
	assert.Equal(t, "Unschedulable 0/3 nodes are available: 3 Insufficient nvidia.com/gpu.", loaded[0].Message)
	assert.Empty(t, loaded[0].Node, "no node while the pod waits for one")

	// Scheduled and pulling the image: the kubelet's reason, and the node.
	pod.Spec.NodeName = testGPUNode
	pod.Status.Conditions = nil
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: llmisvcMainContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}}}}
	_, err = pods.Update(ctx, pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, statusPending, loaded[0].Status)
	assert.Equal(t, "ImagePullBackOff", loaded[0].Reason)
	assert.Equal(t, testGPUNode, loaded[0].Node)

	// A second pod of the same object that terminates does not hide the one
	// that stays.
	gone := workloadPod("tiny", corev1.PodRunning)
	gone.Name = "tiny-kserve-workload-old"
	gone.Spec.NodeName = testCacheNode
	gone.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	gone.Finalizers = []string{"test/keep"}
	_, err = pods.Create(ctx, gone, metav1.CreateOptions{})
	require.NoError(t, err)
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	assert.Equal(t, testGPUNode, loaded[0].Node)
	assert.Equal(t, statusPending, loaded[0].Status)

	// Unload by preset name deletes the object whatever its state ...
	require.NoError(t, f.b.Unload(ctx, "tiny"))
	llmisvcs := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS)
	_, err = llmisvcs.Get(ctx, "tiny", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "deleted by preset name: %v", err)
	// ... and so does unload by repository id.
	f.pendingLLMISVC(ctx, "tiny")
	require.NoError(t, f.b.Unload(ctx, tinyRepo))
	_, err = llmisvcs.Get(ctx, "tiny", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "deleted by repository id: %v", err)
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	assert.Empty(t, loaded)
}

// unauthorizedClients are the Kubernetes clients of a caller whose token
// expired: the API server answers 401 to every request.
func unauthorizedClients() (kubernetes.Interface, dynamic.Interface) {
	deny := func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewUnauthorized("token expired")
	}
	cs := kubefake.NewSimpleClientset()
	cs.PrependReactor("*", "*", deny)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), llmisvcListKinds())
	dyn.PrependReactor("*", "*", deny)
	return cs, dyn
}

// callerClients routes the requests that carry a caller token to the given
// clients, as the server does with downstream OAuth; requests without one keep
// using the ServiceAccount's.
func (f *fixture) callerClients(cs kubernetes.Interface, dyn dynamic.Interface) {
	clientsFor := func(ctx context.Context) (kubernetes.Interface, dynamic.Interface) {
		if _, ok := identity.TokenFromContext(ctx); ok {
			return cs, dyn
		}
		return nil, nil
	}
	f.b.opts.ClientsFor = clientsFor
	f.b.cfg.opts.ClientsFor = clientsFor
}

// The incident behind giantswarm/model-manager#92: a load job kept polling
// readiness with a caller token that had expired, refreshed the shared
// settings with it, and "LLMInferenceService API not served" was cached for
// everyone — the namespace's LLMInferenceServices vanished from every list.
func TestSettingsSurviveACallerWhoseTokenExpired(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serveLLMAPI()
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "tiny"}))
	f.callerClients(unauthorizedClients())
	expired := identity.ContextWithToken(ctx, "expired-token")

	// The caller's own calls fail as they must, and the readiness wait ends
	// at once rather than at its timeout.
	_, err := f.b.ListLoaded(expired)
	require.Error(t, err)
	assert.True(t, apierrors.IsUnauthorized(err), err)
	err = f.b.WaitReady(expired, tinyRepo)
	require.Error(t, err)
	assert.True(t, apierrors.IsUnauthorized(err), err)
	assert.ErrorContains(t, err, "wire_model")

	// The settings do not follow the dead token: the ServiceAccount answers
	// the discovery and the control-plane lookup the caller's client cannot,
	// and the objects stay listed for everyone else.
	f.expireSettings()
	s := f.b.cfg.settings(expired)
	assert.True(t, s.LLMServed, "the ServiceAccount's client answers the API discovery")
	assert.Equal(t, testControlPlaneNS, s.ControlPlane, "the ServiceAccount's client finds the well-known config")
	assert.Empty(t, s.servingUnavailable())
	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1, "the LLMInferenceService stays visible")
	assert.Equal(t, "tiny", loaded[0].Resource)

	// With downstream OAuth the ServiceAccount holds no permissions either:
	// a refresh that no client can answer keeps the last good settings and
	// is retried on the next call.
	chain := f.cs.ReactionChain
	f.cs.PrependReactor("get", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, DefaultDiscoveryConfigMap, nil)
	})
	f.expireSettings()
	s = f.b.cfg.settings(expired)
	assert.True(t, s.LLMServed, "a refresh that fails keeps the last good settings")
	assert.NotEmpty(t, f.b.cfg.refreshErr)
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)

	// Nothing good known yet: the failed resolve is served but not cached,
	// so the next call resolves again.
	f.resetSettings()
	s = f.b.cfg.settings(expired)
	assert.False(t, s.LLMServed, "the defaults until a refresh succeeds")
	assert.Nil(t, f.b.cfg.cached, "a failed resolve is not cached")
	f.cs.ReactionChain = chain
	s = f.b.cfg.settings(ctx)
	assert.True(t, s.LLMServed, "the next successful resolve is cached")
	assert.NotNil(t, f.b.cfg.cached)
	assert.Empty(t, f.b.cfg.refreshErr, "the recovery is noted")
	require.NoError(t, f.b.Unload(ctx, "tiny"))
}
