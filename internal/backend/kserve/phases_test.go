package kserve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/identity"
)

// The proof-1 timeline of a fresh serve on a scale-to-zero L4 pool
// (gazelle, 2026-09-17): the pod is created, Karpenter nominates a NodeClaim,
// the node binds ≈ 3.5 min later, the initializer downloads 8 GB in 72 s,
// the runtime image pulls for ≈ 4 min, vLLM loads for ≈ 1 min, the route
// resolves, the endpoint answers ≈ 12 min after the load.
var (
	t0          = time.Date(2026, 9, 17, 6, 59, 0, 0, time.UTC)
	tNominated  = t0.Add(35 * time.Second)
	tLaunched   = tNominated.Add(28 * time.Second)
	tBound      = t0.Add(4*time.Minute + 2*time.Second)
	tInitStart  = tBound.Add(20 * time.Second)
	tInitDone   = tInitStart.Add(72 * time.Second)
	tPulled     = tInitDone.Add(4 * time.Minute)
	tRunning    = tPulled.Add(2 * time.Second)
	tContReady  = tRunning.Add(61 * time.Second)
	tObjReady   = tContReady.Add(9 * time.Second)
	tNow        = tObjReady.Add(time.Minute)
	mainName    = "main"
	mainField   = "spec.containers{main}"
	predictorNS = testServingNS
)

// predictorFixture is a KServe LLMInferenceService predictor pod on llm-d as
// gazelle runs it: init container storage-initializer, main container the
// llm-d-cuda runtime with a startup probe, one GPU requested.
func predictorFixture(name string) *corev1.Pod {
	gpu := resource.MustParse("1")
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-kserve-workload-7d9f8", Namespace: predictorNS, UID: types.UID("uid-" + name),
			Labels:            map[string]string{llmisvcPodLabel: name, "app.kubernetes.io/part-of": "llminferenceservice"},
			CreationTimestamp: metav1.NewTime(t0),
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: initContainerName, Image: "docker.io/kserve/storage-initializer:v0.16.0"}},
			Containers: []corev1.Container{{
				Name: mainName, Image: "ghcr.io/llm-d/llm-d-cuda:v0.4.0",
				Resources:    corev1.ResourceRequirements{Limits: corev1.ResourceList{DefaultGPUResourceName: gpu}, Requests: corev1.ResourceList{DefaultGPUResourceName: gpu}},
				StartupProbe: &corev1.Probe{FailureThreshold: 120},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/12 nodes are available: 12 Insufficient nvidia.com/gpu. preemption: 0/12 nodes are available: 12 No preemption victims found for incoming pod.", LastTransitionTime: metav1.NewTime(t0)}}},
	}
}

func event(p *corev1.Pod, reason, fieldPath, message string, first, last time.Time) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: p.Name + "." + reason + first.Format("150405"), Namespace: p.Namespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, UID: p.UID, FieldPath: fieldPath},
		Reason:         reason, Message: message, Type: corev1.EventTypeNormal,
		FirstTimestamp: metav1.NewTime(first), LastTimestamp: metav1.NewTime(last), Count: 1,
	}
}

// Mutations that move the fixture along the timeline.

func nominated(p *corev1.Pod) []corev1.Event {
	return []corev1.Event{
		event(p, eventFailedScheduling, "", "0/12 nodes are available: 12 Insufficient nvidia.com/gpu.", t0, t0.Add(30*time.Second)),
		event(p, eventNominated, "", "Pod should schedule on: nodeclaim/gpu-l4-x7k2q", tNominated, tNominated),
	}
}

func bound(p *corev1.Pod) {
	p.Spec.NodeName = "ip-10-0-1-23.eu-west-2.compute.internal"
	p.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(tBound)},
		{Type: corev1.PodInitialized, Status: corev1.ConditionFalse, Reason: "ContainersNotInitialized", LastTransitionTime: metav1.NewTime(tBound)},
	}
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: initContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: mainName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}
}

func downloading(p *corev1.Pod) {
	bound(p)
	p.Status.InitContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(tInitStart)}}
}

func downloaded(p *corev1.Pod, took time.Duration) {
	downloading(p)
	p.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", StartedAt: metav1.NewTime(tInitStart), FinishedAt: metav1.NewTime(tInitStart.Add(took))}}
	p.Status.Conditions[1] = corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(tInitStart.Add(took))}
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
}

func pullingEvents(p *corev1.Pod) []corev1.Event {
	return append(nominated(p), event(p, eventPulling, mainField, `Pulling image "ghcr.io/llm-d/llm-d-cuda:v0.4.0"`, tInitDone, tInitDone))
}

func pulledEvents(p *corev1.Pod) []corev1.Event {
	return append(pullingEvents(p),
		event(p, eventPulled, mainField, `Successfully pulled image "ghcr.io/llm-d/llm-d-cuda:v0.4.0" in 4m0.1s (4m0.1s including waiting)`, tPulled, tPulled),
		event(p, "Created", mainField, "Created container: main", tPulled, tPulled),
		event(p, "Started", mainField, "Started container main", tRunning, tRunning),
	)
}

func loading(p *corev1.Pod) {
	downloaded(p, 72*time.Second)
	p.Status.Phase = corev1.PodRunning
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(tRunning)}}
	p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady", LastTransitionTime: metav1.NewTime(tRunning)})
}

func podReadyAt(p *corev1.Pod, at time.Time) {
	loading(p)
	p.Status.ContainerStatuses[0].Ready = true
	p.Status.Conditions[2] = corev1.PodCondition{Type: corev1.ContainersReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(at)}
}

// restarted: the runtime died — count times so far, the last one with the
// exit given — and the kubelet runs it again.
func restarted(p *corev1.Pod, count, exit int32, reason string) {
	loading(p)
	cs := &p.Status.ContainerStatuses[0]
	cs.RestartCount = count
	cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: exit, Reason: reason, StartedAt: metav1.NewTime(tRunning), FinishedAt: metav1.NewTime(tRunning.Add(90 * time.Second))}
	cs.State.Running.StartedAt = metav1.NewTime(tRunning.Add(2 * time.Minute))
}

// backedOff: the runtime died again and the kubelet waits before the next
// restart — the container's restart count still counts the starts before
// the dead instance, as the kubelet reports it.
func backedOff(p *corev1.Pod, count, exit int32, reason string) {
	restarted(p, count, exit, reason)
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container=main pod=tiny-kserve-workload-7d9f8_serving(uid-tiny)"}}
}

// vllmCrashLog is the tail of a vLLM runtime that died at start-up: the
// engine's traceback, logged line by line with the logger's prefix.
const vllmCrashLog = `INFO 09-18 00:35:10 [core.py:76] Initializing a V1 LLM engine (v0.11.0) with config: model='/mnt/models'
(EngineCore_0 pid=94) ERROR 09-18 00:35:12 [core.py:708] EngineCore failed to start.
(EngineCore_0 pid=94) ERROR 09-18 00:35:12 [core.py:708] Traceback (most recent call last):
(EngineCore_0 pid=94) ERROR 09-18 00:35:12 [core.py:708]   File "/opt/vllm/lib/python3.12/site-packages/vllm/v1/engine/core.py", line 699, in run_engine_core
(EngineCore_0 pid=94) ERROR 09-18 00:35:12 [core.py:708]     os.makedirs(cache_dir, exist_ok=True)
(EngineCore_0 pid=94) ERROR 09-18 00:35:12 [core.py:708] PermissionError: [Errno 13] Permission denied: '/mnt/models/.cache/vllm'
INFO 09-18 00:35:13 [launcher.py:60] Shutting down FastAPI HTTP server.
`

const crashLine = "runtime crashed 2× (exit 1): PermissionError: [Errno 13] Permission denied: '/mnt/models/.cache/vllm'"

func facts(p *corev1.Pod, events []corev1.Event, nodeGPUs int64) podFacts {
	return podFacts{Pod: p, Events: events, NodeKnown: p.Spec.NodeName != "", NodeGPUs: nodeGPUs, GPUResource: DefaultGPUResourceName, Now: tNow}
}

func stepByName(steps []backend.Step, name string) backend.Step {
	for _, s := range steps {
		if s.Name == name {
			return s
		}
	}
	return backend.Step{}
}

func assertStep(t *testing.T, steps []backend.Step, name, state string, since, finished time.Time) backend.Step {
	t.Helper()
	s := stepByName(steps, name)
	require.Equal(t, name, s.Name)
	assert.Equal(t, state, s.State, name)
	if since.IsZero() {
		assert.Nil(t, s.Since, name+" since")
	} else {
		require.NotNil(t, s.Since, name+" since")
		assert.Equal(t, since, s.Since.UTC(), name+" since")
	}
	if finished.IsZero() {
		assert.Nil(t, s.FinishedAt, name+" finishedAt")
	} else {
		require.NotNil(t, s.FinishedAt, name+" finishedAt")
		assert.Equal(t, finished, s.FinishedAt.UTC(), name+" finishedAt")
	}
	return s
}

func notReadyServed(reason, message string) served {
	return served{Kind: ServingKindLLM, Name: "tiny", Model: tinyRepo, GPUs: 1, Status: statusNotReady, Reason: reason, Message: message, Created: t0.Add(-2 * time.Second)}
}

func TestServePhaseFollowsTheProofTimeline(t *testing.T) {
	sv := notReadyServed("PredictorNotReady", "the predictor is not ready")

	t.Run("no pod yet: scheduling since the object's creation", func(t *testing.T) {
		phase, steps := servePhase(sv, podFacts{Now: tNow})
		assert.Equal(t, backend.PhaseScheduling, phase)
		s := assertStep(t, steps, backend.PhaseScheduling, backend.StepInProgress, sv.Created, time.Time{})
		assert.Equal(t, reasonWaitingForPod, s.Reason)
		assert.Len(t, steps, 7)
		assert.Equal(t, backend.StepPending, steps[6].State)
	})

	t.Run("unschedulable: scheduling with the scheduler's reason, never an empty Pending", func(t *testing.T) {
		p := predictorFixture("tiny")
		phase, steps := servePhase(sv, facts(p, nil, 0))
		assert.Equal(t, backend.PhaseScheduling, phase)
		s := assertStep(t, steps, backend.PhaseScheduling, backend.StepInProgress, t0, time.Time{})
		assert.Equal(t, corev1.PodReasonUnschedulable, s.Reason)
		assert.Contains(t, s.Message, "Insufficient nvidia.com/gpu")
	})

	t.Run("nominated: a NodeClaim, not a node — still scheduling", func(t *testing.T) {
		p := predictorFixture("tiny")
		phase, steps := servePhase(sv, facts(p, nominated(p), 0))
		assert.Equal(t, backend.PhaseScheduling, phase)
		s := assertStep(t, steps, backend.PhaseScheduling, backend.StepInProgress, t0, time.Time{})
		assert.Equal(t, reasonNodeLaunching, s.Reason)
		assert.Contains(t, s.Message, "nodeclaim/gpu-l4-x7k2q")
		assertStep(t, steps, backend.PhaseNodeStarting, backend.StepPending, time.Time{}, time.Time{})
	})

	t.Run("launched: nodeStarting since the NodeClaim's instance came", func(t *testing.T) {
		p := predictorFixture("tiny")
		f := facts(p, nominated(p), 0)
		f.Launch = launched(claimName, "")
		phase, steps := servePhase(sv, f)
		assert.Equal(t, backend.PhaseNodeStarting, phase)
		assertStep(t, steps, backend.PhaseScheduling, backend.StepDone, t0, tLaunched)
		s := assertStep(t, steps, backend.PhaseNodeStarting, backend.StepInProgress, tLaunched, time.Time{})
		assert.Equal(t, reasonNodeStarting, s.Reason)
		assert.Contains(t, s.Message, "NodeClaim gpu-l4-x7k2q launched an instance")
	})

	t.Run("bound, GPU not allocatable yet: still nodeStarting", func(t *testing.T) {
		p := predictorFixture("tiny")
		bound(p)
		phase, steps := servePhase(sv, facts(p, nominated(p), 0))
		assert.Equal(t, backend.PhaseNodeStarting, phase)
		s := assertStep(t, steps, backend.PhaseNodeStarting, backend.StepInProgress, tNominated, time.Time{})
		assert.Contains(t, s.Message, "nvidia.com/gpu not allocatable yet")
	})

	t.Run("downloading: the initializer runs, bytesTotal from the preset", func(t *testing.T) {
		p := predictorFixture("tiny")
		downloading(p)
		phase, steps := servePhase(sv, facts(p, nominated(p), 1))
		assert.Equal(t, backend.PhaseDownloadingWeights, phase)
		assertStep(t, steps, backend.PhaseNodeStarting, backend.StepDone, tNominated, tBound)
		s := assertStep(t, steps, backend.PhaseDownloadingWeights, backend.StepInProgress, tInitStart, time.Time{})
		assert.Equal(t, reasonDownloadingWeights, s.Reason)
		assert.Nil(t, s.Cached)
	})

	t.Run("pulling: the initializer took 72 s (not cached), the image pulls", func(t *testing.T) {
		p := predictorFixture("tiny")
		downloaded(p, 72*time.Second)
		phase, steps := servePhase(sv, facts(p, pullingEvents(p), 1))
		assert.Equal(t, backend.PhasePullingImage, phase)
		w := assertStep(t, steps, backend.PhaseDownloadingWeights, backend.StepDone, tInitStart, tInitDone)
		require.NotNil(t, w.Cached)
		assert.False(t, *w.Cached)
		s := assertStep(t, steps, backend.PhasePullingImage, backend.StepInProgress, tInitDone, time.Time{})
		assert.Contains(t, s.Message, "Pulling image")
	})

	t.Run("cached: an initializer that finished within seconds found the weights in the claim", func(t *testing.T) {
		p := predictorFixture("tiny")
		downloaded(p, 320*time.Millisecond)
		_, steps := servePhase(sv, facts(p, nominated(p), 1))
		w := stepByName(steps, backend.PhaseDownloadingWeights)
		require.NotNil(t, w.Cached)
		assert.True(t, *w.Cached)
		assert.Contains(t, w.Message, "already held the weights")
	})

	t.Run("loading: pulled in 4 min, vLLM loads until the startup probe passes", func(t *testing.T) {
		p := predictorFixture("tiny")
		loading(p)
		events := append(pulledEvents(p), event(p, eventUnhealthy, mainField, "Startup probe failed: Get \"http://10.0.1.23:8000/health\": dial tcp 10.0.1.23:8000: connect: connection refused", tRunning.Add(10*time.Second), tRunning.Add(50*time.Second)))
		phase, steps := servePhase(sv, facts(p, events, 1))
		assert.Equal(t, backend.PhaseLoading, phase)
		img := assertStep(t, steps, backend.PhasePullingImage, backend.StepDone, tInitDone, tPulled)
		assert.Contains(t, img.Message, "in 4m0.1s")
		s := assertStep(t, steps, backend.PhaseLoading, backend.StepInProgress, tRunning, time.Time{})
		assert.Equal(t, reasonLoadingModel, s.Reason)
		assert.Contains(t, s.Message, "Startup probe failed")
		assert.NotContains(t, s.Message, "crashed", "a first start is not a crash")
	})

	t.Run("routing: the pod is ready, the route is not", func(t *testing.T) {
		p := predictorFixture("tiny")
		podReadyAt(p, tContReady)
		routing := notReadyServed("HTTPRoutesNotReady", "HTTPRoutesNotReady the HTTPRoute is not accepted yet")
		phase, steps := servePhase(routing, facts(p, pulledEvents(p), 1))
		assert.Equal(t, backend.PhaseRouting, phase)
		assertStep(t, steps, backend.PhaseLoading, backend.StepDone, tRunning, tContReady)
		s := assertStep(t, steps, backend.PhaseRouting, backend.StepInProgress, tContReady, time.Time{})
		assert.Equal(t, "HTTPRoutesNotReady", s.Reason)
	})

	t.Run("ready: every step done, ready since the Ready condition", func(t *testing.T) {
		p := predictorFixture("tiny")
		podReadyAt(p, tContReady)
		ready := served{Kind: ServingKindLLM, Name: "tiny", GPUs: 1, Status: statusReady, Ready: true, ReadyAt: tObjReady, Created: t0}
		phase, steps := servePhase(ready, facts(p, pulledEvents(p), 1))
		assert.Equal(t, backend.PhaseReady, phase)
		for _, s := range steps {
			assert.Equal(t, backend.StepDone, s.State, s.Name)
		}
		assertStep(t, steps, backend.PhaseRouting, backend.StepDone, tContReady, tObjReady)
		assertStep(t, steps, backend.PhaseReady, backend.StepDone, tObjReady, tObjReady)
	})

	t.Run("terminating: the phase says so, the steps stay", func(t *testing.T) {
		p := predictorFixture("tiny")
		podReadyAt(p, tContReady)
		going := served{Kind: ServingKindLLM, Name: "tiny", GPUs: 1, Status: statusReady, Ready: true, ReadyAt: tObjReady, Deleting: true}
		phase, steps := servePhase(going, facts(p, pulledEvents(p), 1))
		assert.Equal(t, backend.PhaseTerminating, phase)
		assert.Equal(t, backend.StepDone, stepByName(steps, backend.PhaseReady).State)
	})
}

func TestServePhaseFailures(t *testing.T) {
	sv := notReadyServed("PredictorNotReady", "")

	t.Run("a failed image pull fails the image step", func(t *testing.T) {
		p := predictorFixture("tiny")
		downloaded(p, 72*time.Second)
		p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: `Back-off pulling image "ghcr.io/llm-d/llm-d-cuda:v0.4.0"`}
		phase, steps := servePhase(sv, facts(p, pullingEvents(p), 1))
		assert.Equal(t, backend.PhaseFailed, phase)
		s := assertStep(t, steps, backend.PhasePullingImage, backend.StepFailed, tInitDone, time.Time{})
		assert.Equal(t, "ImagePullBackOff", s.Reason)
		assert.Contains(t, s.Message, "Back-off pulling image")
	})

	t.Run("a stalled download fails the weights step with DownloadStalled", func(t *testing.T) {
		p := predictorFixture("tiny")
		downloading(p)
		stale := facts(p, nominated(p), 1)
		stale.Now = tInitStart.Add(downloadStallAfter + time.Minute)
		phase, steps := servePhase(sv, stale)
		assert.Equal(t, backend.PhaseFailed, phase)
		s := assertStep(t, steps, backend.PhaseDownloadingWeights, backend.StepFailed, tInitStart, time.Time{})
		assert.Equal(t, reasonDownloadStalled, s.Reason)
		assert.Contains(t, s.Message, "without finishing")
	})

	t.Run("an initializer the kubelet restarted has stalled too", func(t *testing.T) {
		p := predictorFixture("tiny")
		downloading(p)
		p.Status.InitContainerStatuses[0].RestartCount = 2
		p.Status.InitContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}
		phase, steps := servePhase(sv, facts(p, nominated(p), 1))
		assert.Equal(t, backend.PhaseFailed, phase)
		s := stepByName(steps, backend.PhaseDownloadingWeights)
		assert.Equal(t, reasonDownloadStalled, s.Reason)
		assert.Contains(t, s.Message, "OOMKilled")
	})

	t.Run("the object's own failure fails the step under way", func(t *testing.T) {
		p := predictorFixture("tiny")
		loading(p)
		failed := notReadyServed("RuntimeUnhealthy", "RuntimeUnhealthy the runtime exited")
		failed.Failed = true
		phase, steps := servePhase(failed, facts(p, pulledEvents(p), 1))
		assert.Equal(t, backend.PhaseFailed, phase)
		s := stepByName(steps, backend.PhaseLoading)
		assert.Equal(t, backend.StepFailed, s.State)
		assert.Equal(t, "RuntimeUnhealthy", s.Reason)
	})
}

// The NodeClaim Karpenter nominated in nominated(): the pool gpu-l4, the
// claim's five-character suffix.
const (
	claimName = "gpu-l4-x7k2q"
	claimPool = "gpu-l4"
	// iceMessage is Karpenter's InsufficientCapacityError event as the cloud
	// words it, with the event's prefix naming the claim.
	iceMessage = "NodeClaim %s event: creating instance, insufficient capacity, with fleet error(s), InsufficientInstanceCapacity: We currently do not have sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1b). Our system will be working on provisioning additional capacity. You can currently get g6e.2xlarge capacity by not specifying an Availability Zone in your request or choosing eu-central-1a, eu-central-1c."
)

// launched is the nominated claim with its instance up since tLaunched, on
// node when given.
func launched(claim, node string) launchFacts {
	return launchFacts{Claim: claim, State: &claimState{Created: tNominated, Launched: corev1.ConditionTrue, Since: tLaunched, Node: node}}
}

// refusalEvent is Karpenter's Warning on a claim it could not launch, in
// the default namespace.
func refusalEvent(claim string, at time.Time) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: claim + "." + at.Format("150405"), Namespace: karpenterEventsNamespace},
		InvolvedObject: corev1.ObjectReference{Kind: kindNodeClaim, Name: claim, APIVersion: "karpenter.sh/v1"},
		Reason:         "InsufficientCapacityError", Message: fmt.Sprintf(iceMessage, claim), Type: corev1.EventTypeWarning,
		FirstTimestamp: metav1.NewTime(at), LastTimestamp: metav1.NewTime(at), Count: 1,
	}
}

func refusalOf(e corev1.Event) launchRefusal {
	return launchRefusal{Claim: e.InvolvedObject.Name, Reason: e.Reason, Message: claimRefusalMessage(e.InvolvedObject.Name, e.Message), At: e.LastTimestamp.Time}
}

// nodeClaimObject is a karpenter.sh/v1 NodeClaim as the API serves it.
func nodeClaimObject(name string, created time.Time, launched corev1.ConditionStatus, at time.Time, reason, message, node string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.sh/v1", "kind": kindNodeClaim,
		"metadata": map[string]any{"name": name, "creationTimestamp": created.UTC().Format(time.RFC3339), "labels": map[string]any{"karpenter.sh/nodepool": claimPool}},
		"status":   map[string]any{},
	}}
	if launched != "" {
		obj.Object["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": conditionLaunched, "status": string(launched), "lastTransitionTime": at.UTC().Format(time.RFC3339), "reason": reason, "message": message}}
	}
	if node != "" {
		obj.Object["status"].(map[string]any)["nodeName"] = node
	}
	return obj
}

// giantswarm/model-manager#121: Karpenter's Nominated event names a
// NodeClaim, not a node — a claim the cloud refuses for capacity is deleted
// within seconds and another nominated, for as long as Karpenter retries.
// The scheduling step ends when the claim launched an instance; a refusal
// is the step's reason, and fails it once the scale-up budget is spent.
func TestServePhaseSchedulingEndsWhenTheNodeLaunched(t *testing.T) {
	sv := notReadyServed("PredictorNotReady", "the predictor is not ready")
	tRefused := tNominated.Add(3 * time.Second)
	refusals := []launchRefusal{
		refusalOf(refusalEvent(claimName, tRefused)),
		refusalOf(refusalEvent("gpu-l4-75lh8", tRefused.Add(3*time.Minute))),
		refusalOf(refusalEvent("gpu-l4-c67br", tRefused.Add(6*time.Minute))),
	}
	within := tNominated.Add(8 * time.Minute)
	spent := t0.Add(DefaultScaleUpTimeout + time.Second)

	t.Run("the pod names a node: nodeStarting", func(t *testing.T) {
		p := predictorFixture("tiny")
		p.Status.NominatedNodeName = "ip-10-0-1-23.eu-west-2.compute.internal"
		phase, steps := servePhase(sv, facts(p, nil, 0))
		assert.Equal(t, backend.PhaseNodeStarting, phase)
		s := assertStep(t, steps, backend.PhaseNodeStarting, backend.StepInProgress, tNow, time.Time{})
		assert.Contains(t, s.Message, "nominated to node ip-10-0-1-23")
	})

	t.Run("the claim registered its node: nodeStarting names it", func(t *testing.T) {
		p := predictorFixture("tiny")
		f := facts(p, nominated(p), 0)
		f.Launch = launched(claimName, "ip-10-0-1-23.eu-west-2.compute.internal")
		phase, steps := servePhase(sv, f)
		assert.Equal(t, backend.PhaseNodeStarting, phase)
		s := stepByName(steps, backend.PhaseNodeStarting)
		assert.Contains(t, s.Message, "launched node ip-10-0-1-23")
	})

	t.Run("a claim nominated and launching: scheduling, NodeLaunching", func(t *testing.T) {
		p := predictorFixture("tiny")
		f := facts(p, nominated(p), 0)
		f.Launch = launchFacts{Claim: claimName, State: &claimState{Created: tNominated, Launched: corev1.ConditionUnknown}}
		f.ScaleUpTimeout = DefaultScaleUpTimeout
		phase, steps := servePhase(sv, f)
		assert.Equal(t, backend.PhaseScheduling, phase)
		s := assertStep(t, steps, backend.PhaseScheduling, backend.StepInProgress, t0, time.Time{})
		assert.Equal(t, reasonNodeLaunching, s.Reason)
		assert.Equal(t, "Karpenter nominated NodeClaim gpu-l4-x7k2q; no instance has launched yet", s.Message)
	})

	t.Run("the claim refuses for capacity: scheduling, CapacityUnavailable with Karpenter's words", func(t *testing.T) {
		p := predictorFixture("tiny")
		f := facts(p, nominated(p), 0)
		f.Launch = launchFacts{Claim: claimName, State: &claimState{Created: tNominated, Launched: corev1.ConditionFalse, Since: tRefused, Reason: "InsufficientCapacityError", Message: claimRefusalMessage(claimName, fmt.Sprintf(iceMessage, claimName))}}
		f.ScaleUpTimeout, f.Now = DefaultScaleUpTimeout, within
		phase, steps := servePhase(sv, f)
		assert.Equal(t, backend.PhaseScheduling, phase)
		s := assertStep(t, steps, backend.PhaseScheduling, backend.StepInProgress, t0, time.Time{})
		assert.Equal(t, reasonCapacityUnavailable, s.Reason)
		assert.Contains(t, s.Message, "Karpenter could not launch a node: 1 NodeClaim refused, the last (gpu-l4-x7k2q) at "+tRefused.Format(time.RFC3339)+" — InsufficientCapacityError: creating instance, insufficient capacity")
		assert.Contains(t, s.Message, "sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1b)")
		assert.NotContains(t, s.Message, "event:", "the event's prefix naming the claim is dropped")
		assert.Contains(t, s.Message, "it retries while the pod waits")
		assert.Equal(t, backend.StepPending, stepByName(steps, backend.PhaseNodeStarting).State)
	})

	t.Run("the claim is gone, the refusals stand in the events: the count and the last", func(t *testing.T) {
		p := predictorFixture("tiny")
		f := facts(p, nominated(p), 0)
		f.Launch = launchFacts{Claim: claimName, Refusals: refusals}
		f.ScaleUpTimeout, f.Now = DefaultScaleUpTimeout, within
		phase, steps := servePhase(sv, f)
		assert.Equal(t, backend.PhaseScheduling, phase)
		s := stepByName(steps, backend.PhaseScheduling)
		assert.Equal(t, reasonCapacityUnavailable, s.Reason)
		assert.Contains(t, s.Message, "3 NodeClaims refused, the last (gpu-l4-c67br) at "+refusals[2].At.Format(time.RFC3339))
	})

	t.Run("a claim created after the refusals is Karpenter's retry: the claim speaks", func(t *testing.T) {
		p := predictorFixture("tiny")
		f := facts(p, nominated(p), 0)
		f.Launch = launchFacts{Claim: claimName, State: &claimState{Created: refusals[2].At.Add(time.Second), Launched: corev1.ConditionUnknown}, Refusals: refusals}
		f.ScaleUpTimeout, f.Now = DefaultScaleUpTimeout, within
		_, steps := servePhase(sv, f)
		s := stepByName(steps, backend.PhaseScheduling)
		assert.Equal(t, reasonNodeLaunching, s.Reason)
	})

	t.Run("the scale-up budget spent against a refusal: the step and the phase fail naming it", func(t *testing.T) {
		p := predictorFixture("tiny")
		f := facts(p, nominated(p), 0)
		f.Launch = launchFacts{Claim: claimName, Refusals: refusals}
		f.ScaleUpTimeout, f.Now = DefaultScaleUpTimeout, spent
		phase, steps := servePhase(sv, f)
		assert.Equal(t, backend.PhaseFailed, phase)
		s := assertStep(t, steps, backend.PhaseScheduling, backend.StepFailed, t0, time.Time{})
		assert.Equal(t, reasonCapacityUnavailable, s.Reason)
		assert.Contains(t, s.Message, "3 NodeClaims refused")
		assert.Contains(t, s.Message, "; no node came within the scale-up budget of 10m0s")
	})

	t.Run("the budget spent without a refusal: still scheduling — nothing to name", func(t *testing.T) {
		p := predictorFixture("tiny")
		f := facts(p, nominated(p), 0)
		f.Launch = launchFacts{Claim: claimName, State: &claimState{Created: tNominated, Launched: corev1.ConditionUnknown}}
		f.ScaleUpTimeout, f.Now = DefaultScaleUpTimeout, spent
		phase, steps := servePhase(sv, f)
		assert.Equal(t, backend.PhaseScheduling, phase)
		assert.Equal(t, backend.StepInProgress, stepByName(steps, backend.PhaseScheduling).State)
	})

	t.Run("no budget (0) never fails the step", func(t *testing.T) {
		p := predictorFixture("tiny")
		f := facts(p, nominated(p), 0)
		f.Launch = launchFacts{Claim: claimName, Refusals: refusals}
		f.Now = spent
		phase, _ := servePhase(sv, f)
		assert.Equal(t, backend.PhaseScheduling, phase)
	})

	t.Run("reads that failed are named, never silent", func(t *testing.T) {
		p := predictorFixture("tiny")
		f := facts(p, nominated(p), 0)
		f.Launch = launchFacts{
			Claim:       claimName,
			ClaimErr:    apierrors.NewForbidden(schema.GroupResource{Group: "karpenter.sh", Resource: "nodeclaims"}, claimName, errors.New(`User "viewer" cannot get resource "nodeclaims" in API group "karpenter.sh" at the cluster scope`)),
			RefusalsErr: apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New(`User "viewer" cannot list resource "events" in API group "" in the namespace "default"`)),
		}
		_, steps := servePhase(sv, f)
		s := stepByName(steps, backend.PhaseScheduling)
		assert.Equal(t, reasonNodeLaunching, s.Reason)
		assert.Contains(t, s.Message, "whether NodeClaim gpu-l4-x7k2q launched could not be read (")
		assert.Contains(t, s.Message, `cannot get resource "nodeclaims"`)
		assert.Contains(t, s.Message, "Karpenter's events in default could not be read (")
	})

	t.Run("the claim is gone and no refusal is recorded: said so", func(t *testing.T) {
		p := predictorFixture("tiny")
		f := facts(p, nominated(p), 0)
		f.Launch = launchFacts{Claim: claimName}
		_, steps := servePhase(sv, f)
		s := stepByName(steps, backend.PhaseScheduling)
		assert.Equal(t, reasonNodeLaunching, s.Reason)
		assert.Contains(t, s.Message, "which is gone, and no refusal is recorded for the pool")
	})

	t.Run("bound: scheduling ended when the claim launched, when known", func(t *testing.T) {
		p := predictorFixture("tiny")
		bound(p)
		f := facts(p, nominated(p), 0)
		f.Launch = launched(claimName, p.Spec.NodeName)
		_, steps := servePhase(sv, f)
		assertStep(t, steps, backend.PhaseScheduling, backend.StepDone, t0, tLaunched)
		assertStep(t, steps, backend.PhaseNodeStarting, backend.StepInProgress, tLaunched, time.Time{})
	})

	t.Run("parseClaim reads the Launched condition, the node and the creation", func(t *testing.T) {
		obj := nodeClaimObject(claimName, tNominated, corev1.ConditionFalse, tRefused, "InsufficientCapacityError", fmt.Sprintf(iceMessage, claimName), "")
		s := parseClaim(obj)
		assert.Equal(t, tNominated, s.Created)
		assert.Equal(t, corev1.ConditionFalse, s.Launched)
		assert.Equal(t, tRefused, s.Since)
		assert.Equal(t, "InsufficientCapacityError", s.Reason)
		assert.True(t, strings.HasPrefix(s.Message, "creating instance, insufficient capacity"), s.Message)
		assert.Equal(t, claimPool, nodeClaimPool(claimName))
		assert.Empty(t, nodeClaimPool("ip-10-0-1-23"), "not a claim name")
	})
}

// giantswarm/model-manager#117: a runtime that dies at start-up is named —
// crash count, exit code, the crashed instance's last error line — instead
// of the probe's "connection refused", and the phase fails once the kubelet
// backs off.
func TestServePhaseNamesACrashLoopingRuntime(t *testing.T) {
	sv := notReadyServed("PredictorNotReady", "the predictor is not ready")
	crash := crashLog{Line: lastErrorLine(vllmCrashLog)}
	// After a crash the kubelet re-pulls the image (present on the node): a
	// second Pulled event, later; the probe kept failing meanwhile.
	events := func(p *corev1.Pod) []corev1.Event {
		return append(pulledEvents(p),
			event(p, eventUnhealthy, mainField, `Startup probe failed: Get "http://10.0.1.23:8000/health": dial tcp 10.0.1.23:8000: connect: connection refused`, tRunning.Add(10*time.Second), tRunning.Add(4*time.Minute)),
			event(p, eventPulled, mainField, `Container image "ghcr.io/llm-d/llm-d-cuda:v0.4.0" already present on machine`, tRunning.Add(2*time.Minute), tRunning.Add(2*time.Minute)),
		)
	}

	t.Run("restarted and running again: loading stays under way with CrashLoop, since the first pull", func(t *testing.T) {
		p := predictorFixture("tiny")
		restarted(p, 2, 1, "Error")
		f := facts(p, events(p), 1)
		f.Crash = crash
		phase, steps := servePhase(sv, f)
		assert.Equal(t, backend.PhaseLoading, phase)
		img := assertStep(t, steps, backend.PhasePullingImage, backend.StepDone, tInitDone, tPulled)
		assert.Contains(t, img.Message, "in 4m0.1s", "the image step keeps its first pull")
		s := assertStep(t, steps, backend.PhaseLoading, backend.StepInProgress, tPulled, time.Time{})
		assert.Equal(t, reasonCrashLoop, s.Reason)
		assert.Equal(t, crashLine, s.Message)
	})

	t.Run("the kubelet backed off: the step fails with the crash and the back-off, the phase is failed", func(t *testing.T) {
		p := predictorFixture("tiny")
		backedOff(p, 1, 1, "Error")
		f := facts(p, events(p), 1)
		f.Crash = crash
		phase, steps := servePhase(sv, f)
		assert.Equal(t, backend.PhaseFailed, phase)
		assertStep(t, steps, backend.PhasePullingImage, backend.StepDone, tInitDone, tPulled)
		s := assertStep(t, steps, backend.PhaseLoading, backend.StepFailed, tPulled, time.Time{})
		assert.Equal(t, "CrashLoopBackOff", s.Reason)
		assert.Equal(t, crashLine+"; back-off 5m0s restarting failed container=main pod=tiny-kserve-workload-7d9f8_serving(uid-tiny)", s.Message)
	})

	t.Run("backed off with the Events expired: still the loading step, never pullingImage", func(t *testing.T) {
		p := predictorFixture("tiny")
		backedOff(p, 3, 137, "OOMKilled")
		f := facts(p, nil, 1)
		f.Crash = crash
		phase, steps := servePhase(sv, f)
		assert.Equal(t, backend.PhaseFailed, phase)
		assertStep(t, steps, backend.PhasePullingImage, backend.StepDone, tInitDone, tInitDone)
		s := assertStep(t, steps, backend.PhaseLoading, backend.StepFailed, tInitDone, time.Time{})
		assert.Contains(t, s.Message, "runtime crashed 4× (exit 137, OOMKilled): PermissionError")
	})

	t.Run("the log could not be read: the message says why, never silently", func(t *testing.T) {
		p := predictorFixture("tiny")
		restarted(p, 1, 1, "Error")
		f := facts(p, events(p), 1)
		f.Crash = crashLog{Err: apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, p.Name, errors.New(`User "viewer" cannot get resource "pods/log"`))}
		_, steps := servePhase(sv, f)
		s := stepByName(steps, backend.PhaseLoading)
		assert.Equal(t, reasonCrashLoop, s.Reason)
		assert.Contains(t, s.Message, "runtime crashed 1× (exit 1); the crashed container's log could not be read (")
		assert.Contains(t, s.Message, "is forbidden")
	})

	t.Run("no error line in the log: the message says so", func(t *testing.T) {
		p := predictorFixture("tiny")
		restarted(p, 1, 1, "Error")
		_, steps := servePhase(sv, facts(p, events(p), 1))
		assert.Contains(t, stepByName(steps, backend.PhaseLoading).Message, "runtime crashed 1× (exit 1); no error line in the last 200 lines")
	})

	t.Run("terminated and never restarted (restartPolicy Never): failed with the exit", func(t *testing.T) {
		p := predictorFixture("tiny")
		loading(p)
		p.Spec.RestartPolicy = corev1.RestartPolicyNever
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"}}
		f := facts(p, pulledEvents(p), 1)
		f.Crash = crash
		phase, steps := servePhase(sv, f)
		assert.Equal(t, backend.PhaseFailed, phase)
		s := stepByName(steps, backend.PhaseLoading)
		assert.Equal(t, backend.StepFailed, s.State)
		assert.Equal(t, "Error", s.Reason)
		assert.Contains(t, s.Message, "runtime crashed 1× (exit 2): PermissionError")
	})
}

func TestLastErrorLine(t *testing.T) {
	assert.Equal(t, "PermissionError: [Errno 13] Permission denied: '/mnt/models/.cache/vllm'", lastErrorLine(vllmCrashLog), "the last error line, from the exception on")
	assert.Equal(t, "level=error msg=\"open /mnt/models/config.json: permission denied\"", lastErrorLine("level=info msg=starting\nlevel=error msg=\"open /mnt/models/config.json: permission denied\"\n"), "a line without an exception stays whole")
	assert.Empty(t, lastErrorLine("INFO all good\nINFO still good\n"))
	got := lastErrorLine("INFO\nRuntimeError: " + strings.Repeat("x", 300) + "\n")
	assert.Len(t, []rune(got), crashLineMax)
	assert.True(t, strings.HasSuffix(got, "…"), got)
}

// The wiring of giantswarm/model-manager#117: list_loaded_models reads the
// crashed runtime's previous log as the caller, and a caller who may not
// read pods/log gets the crash without the line and the reason.
func TestListLoadedNamesACrashLoopingRuntime(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.serveLLMAPI()
	f.pendingLLMISVC(ctx, "tiny")
	p := predictorFixture("tiny")
	restarted(p, 2, 1, "Error")
	f.setPreviousLogs(p.Name, vllmCrashLog)
	_, err := f.cs.CoreV1().Nodes().Create(ctx, withGPUs(node(p.Spec.NodeName, "64Gi", nil), 1), metav1.CreateOptions{})
	require.NoError(t, err)
	pods := f.cs.CoreV1().Pods(testServingNS)
	_, err = pods.Create(ctx, p, metav1.CreateOptions{})
	require.NoError(t, err)

	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, backend.PhaseLoading, loaded[0].Phase)
	s := stepByName(loaded[0].Steps, backend.PhaseLoading)
	assert.Equal(t, backend.StepInProgress, s.State)
	assert.Equal(t, reasonCrashLoop, s.Reason)
	assert.Equal(t, crashLine, s.Message)

	// The kubelet backs off: the phase fails and the answer's own reason and
	// message are the crash, not the object's Ready condition.
	backedOff(p, 2, 1, "Error")
	_, err = pods.Update(ctx, p, metav1.UpdateOptions{})
	require.NoError(t, err)
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, backend.PhaseFailed, loaded[0].Phase)
	assert.Equal(t, "CrashLoopBackOff", loaded[0].Reason)
	assert.Contains(t, loaded[0].Message, "runtime crashed 3× (exit 1): PermissionError")

	// A caller who may not read pods/log.
	f.b.logs = func(ctx context.Context, _, name string, _ corev1.PodLogOptions) (string, error) {
		if _, ok := identity.TokenFromContext(ctx); ok {
			return "", apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, name, errors.New(`User "viewer" cannot get resource "pods/log" in API group "" in the namespace "serving"`))
		}
		return vllmCrashLog, nil
	}
	loaded, err = f.b.ListLoaded(identity.ContextWithToken(ctx, "viewer-token"))
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Contains(t, loaded[0].Message, "runtime crashed 3× (exit 1); the crashed container's log could not be read (")
	assert.Contains(t, loaded[0].Message, `cannot get resource "pods/log"`)
	assert.NotContains(t, loaded[0].Message, "PermissionError")
}

// The wiring: list_loaded_models reads the predictor pod, its Events (by the
// pod's UID) and its node, and a Pending pod whose initializer runs is never
// an empty Pending.
func TestListLoadedCarriesPhaseAndSteps(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.serveLLMAPI()
	f.pendingLLMISVC(ctx, "tiny")
	p := predictorFixture("tiny")
	downloading(p)
	p.Status.InitContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(time.Now().Add(-30 * time.Second))
	_, err := f.cs.CoreV1().Nodes().Create(ctx, withGPUs(node(p.Spec.NodeName, "64Gi", nil), 1), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = f.cs.CoreV1().Pods(testServingNS).Create(ctx, p, metav1.CreateOptions{})
	require.NoError(t, err)
	for _, e := range nominated(p) {
		_, err = f.cs.CoreV1().Events(testServingNS).Create(ctx, &e, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	other := predictorFixture("other")
	stray := event(other, eventNominated, "", "Pod should schedule on: nodeclaim/someone-else", tNominated.Add(time.Hour), tNominated.Add(time.Hour))
	_, err = f.cs.CoreV1().Events(testServingNS).Create(ctx, &stray, metav1.CreateOptions{})
	require.NoError(t, err)

	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	lm := loaded[0]
	assert.Equal(t, backend.PhaseDownloadingWeights, lm.Phase)
	assert.Equal(t, statusPending, lm.Status)
	assert.Equal(t, reasonDownloadingWeights, lm.Reason, "the step under way names the reason")
	assert.Contains(t, lm.Message, initContainerName)
	assert.Equal(t, p.Spec.NodeName, lm.Node)
	require.Len(t, lm.Steps, 7)
	assertStep(t, lm.Steps, backend.PhaseScheduling, backend.StepDone, t0, tNominated)
	assertStep(t, lm.Steps, backend.PhaseNodeStarting, backend.StepDone, tNominated, tBound)
	w := stepByName(lm.Steps, backend.PhaseDownloadingWeights)
	assert.Equal(t, backend.StepInProgress, w.State)
	assert.Positive(t, w.BytesTotal, "the preset's weights")
	assert.Zero(t, w.BytesCompleted, "no cache agent: no live bytes")
	assert.Equal(t, backend.StepPending, stepByName(lm.Steps, backend.PhaseReady).State)
}

// The wiring behind giantswarm/model-manager#121: for a pod without a node,
// list_loaded_models reads the NodeClaim Karpenter nominated (by the name in
// the pod's Nominated event) and, unless it launched, Karpenter's refusal
// events in the default namespace — as the caller — and the model's own
// reason and message carry Karpenter's answer instead of the scheduler's
// Unschedulable.
func TestListLoadedNamesAKarpenterRefusal(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.serveLLMAPI()
	f.pendingLLMISVC(ctx, "tiny")
	p := predictorFixture("tiny")
	created := time.Now().Add(-time.Minute)
	p.CreationTimestamp = metav1.NewTime(created)
	_, err := f.cs.CoreV1().Pods(testServingNS).Create(ctx, p, metav1.CreateOptions{})
	require.NoError(t, err)
	for _, e := range nominated(p) {
		_, err = f.cs.CoreV1().Events(testServingNS).Create(ctx, &e, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	claims := f.dyn.Resource(nodeClaimGVR)
	refused := created.Add(25 * time.Second)

	// The claim stands with Launched=False: its condition is the refusal.
	_, err = claims.Create(ctx, nodeClaimObject(claimName, created.Add(20*time.Second), corev1.ConditionFalse, refused, "InsufficientCapacityError", fmt.Sprintf(iceMessage, claimName), ""), metav1.CreateOptions{})
	require.NoError(t, err)
	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, backend.PhaseScheduling, loaded[0].Phase)
	assert.Equal(t, statusPending, loaded[0].Status)
	assert.Equal(t, reasonCapacityUnavailable, loaded[0].Reason)
	assert.Contains(t, loaded[0].Message, "1 NodeClaim refused, the last (gpu-l4-x7k2q)")
	assert.Contains(t, loaded[0].Message, "InsufficientInstanceCapacity")
	assert.Equal(t, backend.StepInProgress, stepByName(loaded[0].Steps, backend.PhaseScheduling).State)
	assert.Equal(t, backend.StepPending, stepByName(loaded[0].Steps, backend.PhaseNodeStarting).State)

	// Karpenter deleted the claim and retried twice more; the events carry
	// the refusals. A Warning on another pool's claim, and a Normal event,
	// are not counted.
	require.NoError(t, claims.Delete(ctx, claimName, metav1.DeleteOptions{}))
	events := f.cs.CoreV1().Events(karpenterEventsNamespace)
	for _, e := range []corev1.Event{
		refusalEvent(claimName, refused),
		refusalEvent("gpu-l4-75lh8", refused.Add(3*time.Minute)),
		refusalEvent("gpu-l4-c67br", refused.Add(6*time.Minute)),
		refusalEvent("other-pool-abcde", refused.Add(7*time.Minute)),
	} {
		_, err = events.Create(ctx, &e, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	normal := refusalEvent("gpu-l4-zzzzz", refused.Add(8*time.Minute))
	normal.Type, normal.Reason = corev1.EventTypeNormal, "Launched"
	_, err = events.Create(ctx, &normal, metav1.CreateOptions{})
	require.NoError(t, err)
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, backend.PhaseScheduling, loaded[0].Phase)
	assert.Equal(t, reasonCapacityUnavailable, loaded[0].Reason)
	assert.Contains(t, loaded[0].Message, "3 NodeClaims refused, the last (gpu-l4-c67br)")

	// The scale-up budget spent: the step and the phase fail, the model says so.
	f.b.opts.ScaleUpTimeout = 30 * time.Second
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, backend.PhaseFailed, loaded[0].Phase)
	assert.Equal(t, reasonCapacityUnavailable, loaded[0].Reason)
	assert.Contains(t, loaded[0].Message, "no node came within the scale-up budget of 30s")
	assert.Equal(t, backend.StepFailed, stepByName(loaded[0].Steps, backend.PhaseScheduling).State)
	f.b.opts.ScaleUpTimeout = DefaultScaleUpTimeout

	// A fresh claim launched: nodeStarting, the refusals before it are history.
	launchedAt := refused.Add(9 * time.Minute)
	_, err = claims.Create(ctx, nodeClaimObject(claimName, launchedAt.Add(-20*time.Second), corev1.ConditionTrue, launchedAt, "Launched", "", ""), metav1.CreateOptions{})
	require.NoError(t, err)
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, backend.PhaseNodeStarting, loaded[0].Phase)
	assert.Equal(t, reasonNodeStarting, loaded[0].Reason)
	assert.Contains(t, loaded[0].Message, "NodeClaim gpu-l4-x7k2q launched an instance")
	s := stepByName(loaded[0].Steps, backend.PhaseScheduling)
	assert.Equal(t, backend.StepDone, s.State)
	require.NotNil(t, s.FinishedAt)
	assert.Equal(t, launchedAt.UTC().Truncate(time.Second), s.FinishedAt.UTC())

	// A caller who may not read NodeClaims: the step says so.
	f.dyn.PrependReactor("get", "nodeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "karpenter.sh", Resource: "nodeclaims"}, claimName, errors.New(`User "viewer" cannot get resource "nodeclaims" in API group "karpenter.sh" at the cluster scope`))
	})
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, backend.PhaseScheduling, loaded[0].Phase, "the refusals stand (the pool's events), the claim's own state is unknown")
	assert.Equal(t, reasonCapacityUnavailable, loaded[0].Reason)
	assert.Contains(t, loaded[0].Message, "3 NodeClaims refused")
	assert.Contains(t, loaded[0].Message, "; whether NodeClaim gpu-l4-x7k2q launched could not be read (")
	assert.Contains(t, loaded[0].Message, `cannot get resource "nodeclaims"`)
}
