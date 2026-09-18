package kserve

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/giantswarm/model-manager/internal/backend"
)

// Where a serve is, read off the predictor pod, its Events and its node
// (giantswarm/model-manager#110). A fresh serve on a scale-to-zero pool moves
// through backend.ServePhases: the scheduler finds no node, the autoscaler
// nominates a NodeClaim and launches its instance (scheduling), the node
// registers and its GPU becomes allocatable (nodeStarting), the
// storage-initializer fills the cache directory (downloadingWeights), the
// kubelet pulls the runtime image (pullingImage), vLLM loads the weights
// until the startup probe passes (loading), KServe resolves the route
// (routing), the endpoint answers (ready). Conditions carry the transitions
// the pod records; Events carry the rest — Karpenter's nomination,
// Pulling/Pulled with the pull's duration, probe failures; the nominated
// NodeClaim says whether an instance came, and Karpenter's refusal to launch
// one is the scheduling step's reason until a node comes or the scale-up
// budget runs out (launch.go, giantswarm/model-manager#121). A runtime that
// dies while loading is named by its container status — how often it
// crashed, the exit code — and by the last error line of the crashed
// instance's log (giantswarm/model-manager#117).
const (
	initContainerName = "storage-initializer"

	// eventsTimeout bounds the Events read per pod so three served models
	// stay inside the caller's deadline.
	eventsTimeout = 3 * time.Second
	// cachedWithin: an initializer that finished this fast found the weights
	// in the claim (8 GB took 72 s to download, 0.3 s when cached).
	cachedWithin = 15 * time.Second
	// downloadStallAfter: an initializer running this long has stalled.
	downloadStallAfter = 30 * time.Minute
	// crashLogTail bounds the read of a crashed runtime's log; crashLineMax
	// bounds the line kept from it.
	crashLogTail = 200
	crashLineMax = 240

	eventNominated        = "Nominated"
	eventFailedScheduling = "FailedScheduling"
	eventPulling          = "Pulling"
	eventPulled           = "Pulled"
	eventUnhealthy        = "Unhealthy"

	reasonNodeStarting       = "NodeStarting"
	reasonDownloadingWeights = "DownloadingWeights"
	reasonDownloadStalled    = "DownloadStalled"
	reasonLoadingModel       = "LoadingModel"
	reasonWaitingForPod      = "WaitingForPod"
	// reasonCrashLoop: the runtime died and the kubelet restarts it; once it
	// backs off the container status says CrashLoopBackOff itself.
	reasonCrashLoop = "CrashLoop"
)

// errorLine matches the log lines a crash's cause is read from: a Python
// traceback and its exception line, a Go error, a permission refused.
// exceptionAt finds the exception itself in such a line (`PermissionError:
// [Errno 13] …`), so a logger's prefix in front of it is dropped.
var (
	errorLine   = regexp.MustCompile(`Error|Traceback|Exception|denied`)
	exceptionAt = regexp.MustCompile(`\b[A-Z]\w*(?:Error|Exception)\b: `)
)

// podFacts is what the phase computation reads besides the pod.
type podFacts struct {
	Pod    *corev1.Pod
	Events []corev1.Event
	// NodeKnown says the pod's node was read; NodeGPUs is its allocatable
	// accelerator count.
	NodeKnown bool
	NodeGPUs  int64
	// GPUResource names the accelerator resource (nvidia.com/gpu).
	GPUResource string
	Now         time.Time
	// Crash is read off the crashed runtime container's log, when one died.
	Crash crashLog
	// Launch is Karpenter's account of the node a pod without one waits for
	// (launch.go); ScaleUpTimeout is how long the scheduling step may stand
	// against a refusal before it fails (0: never).
	Launch         launchFacts
	ScaleUpTimeout time.Duration
}

// crashLog is what a crashed runtime container's log says: its last error
// line, or why the log could not be read (a caller without pods/log).
type crashLog struct {
	Line string
	Err  error
}

// timeline is the steps of a serve while they are computed.
type timeline struct {
	steps []backend.Step
}

func newTimeline() *timeline {
	t := &timeline{steps: make([]backend.Step, 0, len(backend.ServePhases))}
	for _, name := range backend.ServePhases {
		t.steps = append(t.steps, backend.Step{Name: name, State: backend.StepPending})
	}
	return t
}

func (t *timeline) begin(i int, at time.Time) {
	t.steps[i].State = backend.StepInProgress
	t.steps[i].Since = timePtr(at)
}

// note records what the objects say about the step under way.
func (t *timeline) note(i int, reason, message string) {
	t.steps[i].Reason = reason
	t.steps[i].Message = strings.TrimSpace(message)
}

func (t *timeline) done(i int, at time.Time, message string) {
	s := &t.steps[i]
	s.State = backend.StepDone
	if s.Since == nil {
		s.Since = timePtr(at)
	}
	s.FinishedAt = timePtr(at)
	s.Reason = ""
	s.Message = strings.TrimSpace(message)
}

func (t *timeline) fail(i int, reason, message string) {
	t.steps[i].State = backend.StepFailed
	t.note(i, reason, message)
}

// refused records what a capacity refusal says about the step — the sizes,
// the zones, the cache claim's pin — as the step's fields (launch.go).
func (t *timeline) refused(i int, cr *capacityRefusal) {
	if cr == nil {
		return
	}
	s := &t.steps[i]
	s.RefusedInstanceTypes, s.RequestedZones, s.AvailableZones, s.PinnedByCache = cr.RefusedInstanceTypes, cr.RequestedZones, cr.AvailableZones, cr.PinnedByCache
}

// phase is the current phase: terminating while the object goes, failed when
// a step failed, else the step under way, else ready.
func (t *timeline) phase(deleting bool) string {
	if deleting {
		return backend.PhaseTerminating
	}
	current := backend.PhaseReady
	for _, s := range t.steps {
		switch s.State {
		case backend.StepFailed:
			return backend.PhaseFailed
		case backend.StepInProgress:
			if current == backend.PhaseReady {
				current = s.Name
			}
		}
	}
	return current
}

// current is the step under way or failed; nil when every step is done or
// pending.
func (t *timeline) current() *backend.Step {
	for i := range t.steps {
		if s := &t.steps[i]; s.State == backend.StepInProgress || s.State == backend.StepFailed {
			return s
		}
	}
	return nil
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

// The indexes of the steps in backend.ServePhases.
const (
	stepScheduling = iota
	stepNodeStarting
	stepWeights
	stepImage
	stepLoading
	stepRouting
	stepReady
)

// servePhase computes the phase and the timeline of a served object from
// its pod. Without a pod the object waits for one (or is Ready without the
// driver seeing the pod). A serve that failed by the object's own account
// (modelStatus.lastFailureInfo) fails the step under way.
func servePhase(sv served, pf podFacts) (string, []backend.Step) {
	t := newTimeline()
	if pf.Pod == nil {
		if sv.Ready {
			for i := range t.steps {
				t.done(i, sv.ReadyAt, "")
			}
			return t.phase(sv.Deleting), t.steps
		}
		t.begin(stepScheduling, sv.Created)
		t.note(stepScheduling, reasonWaitingForPod, "waiting for the predictor pod")
		return finish(t, sv)
	}
	if !schedule(t, pf) || !download(t, pf) || !pull(t, pf) || !load(t, pf) {
		return finish(t, sv)
	}
	readyAt := conditionTime(pf.Pod, corev1.ContainersReady, pf.Now)
	t.begin(stepRouting, readyAt)
	if !sv.Ready {
		t.note(stepRouting, sv.Reason, sv.Message)
		return finish(t, sv)
	}
	t.done(stepRouting, sv.ReadyAt, "")
	t.done(stepReady, sv.ReadyAt, "")
	return finish(t, sv)
}

func finish(t *timeline, sv served) (string, []backend.Step) {
	if sv.Failed {
		if s := t.current(); s != nil && s.State != backend.StepFailed {
			t.fail(indexOf(s.Name), sv.Reason, sv.Message)
		}
	}
	return t.phase(sv.Deleting), t.steps
}

func indexOf(name string) int {
	for i, n := range backend.ServePhases {
		if n == name {
			return i
		}
	}
	return 0
}

// schedule covers scheduling and nodeStarting; false while one of them is
// under way. Scheduling ends when a node exists for the pod: the nominated
// NodeClaim launched an instance, the pod names a node, or it is bound — a
// nomination alone is a claim, not a node (launch.go).
func schedule(t *timeline, pf podFacts) bool {
	p := pf.Pod
	t.begin(stepScheduling, p.CreationTimestamp.Time)
	nominated := lastEvent(pf.Events, eventNominated, "")
	nominatedAt := eventTime(nominated, true)
	bound := conditionIs(p, corev1.PodScheduled, corev1.ConditionTrue)
	if !bound {
		launchedAt, message, launched := nodeLaunched(pf, nominatedAt)
		if !launched {
			reason, message, refusal := schedulingReason(pf, nominated)
			if refusal != nil && pf.ScaleUpTimeout > 0 && pf.Now.Sub(p.CreationTimestamp.Time) > pf.ScaleUpTimeout {
				t.fail(stepScheduling, reason, fmt.Sprintf("%s; no node came within the scale-up budget of %s", message, pf.ScaleUpTimeout))
			} else {
				t.note(stepScheduling, reason, message)
			}
			t.refused(stepScheduling, refusal)
			return false
		}
		t.done(stepScheduling, launchedAt, "")
		t.begin(stepNodeStarting, launchedAt)
		t.note(stepNodeStarting, reasonNodeStarting, message)
		return false
	}
	// A bound pod's node did launch: when its claim says, else at the
	// nomination — near enough, the instance follows it within seconds —,
	// else when the pod bound; the node started then.
	boundAt := conditionTime(p, corev1.PodScheduled, p.CreationTimestamp.Time)
	scheduledAt := boundAt
	if at, ok := pf.Launch.launched(); ok && at.Before(boundAt) {
		scheduledAt = at
	} else if !nominatedAt.IsZero() && nominatedAt.Before(boundAt) {
		scheduledAt = nominatedAt
	}
	t.done(stepScheduling, scheduledAt, "")
	t.begin(stepNodeStarting, scheduledAt)
	if pf.NodeKnown && pf.NodeGPUs == 0 && requestsGPU(p, pf.GPUResource) {
		t.note(stepNodeStarting, reasonNodeStarting, fmt.Sprintf("node %s: %s not allocatable yet", p.Spec.NodeName, pf.GPUResource))
		return false
	}
	t.done(stepNodeStarting, boundAt, "")
	return true
}

// nodeLaunched reports whether a node exists for a pod that is not bound
// yet — the pod names a node, or the nominated NodeClaim launched — with
// when it came and what the nodeStarting step says.
func nodeLaunched(pf podFacts, nominatedAt time.Time) (time.Time, string, bool) {
	if node := pf.Pod.Status.NominatedNodeName; node != "" {
		if nominatedAt.IsZero() {
			nominatedAt = pf.Now
		}
		return nominatedAt, "nominated to node " + node, true
	}
	at, ok := pf.Launch.launched()
	if !ok {
		return time.Time{}, "", false
	}
	message := fmt.Sprintf("NodeClaim %s launched an instance; the node is registering", pf.Launch.Claim)
	if node := pf.Launch.State.Node; node != "" {
		message = fmt.Sprintf("NodeClaim %s launched node %s; its GPU is not allocatable yet", pf.Launch.Claim, node)
	}
	return at, message, true
}

// schedulingReason is why a pod without a node waits: Karpenter's refusal to
// launch one (refusal then says what was refused and what would launch), the
// claim it nominated and is launching, else the scheduler's own words. A
// read of Karpenter's objects that failed is named in the message.
func schedulingReason(pf podFacts, nominated *corev1.Event) (reason, message string, refusal *capacityRefusal) {
	lf := pf.Launch
	if lf.Claim != "" {
		if all := lf.refusals(); len(all) > 0 {
			cr := lf.capacity(all)
			reason, message, refusal = reasonCapacityUnavailable, refusalMessage(all[len(all)-1], len(all), cr), &cr
		} else {
			reason, message = reasonNodeLaunching, fmt.Sprintf("Karpenter nominated NodeClaim %s; no instance has launched yet", lf.Claim)
			if lf.State == nil && lf.ClaimErr == nil {
				message = fmt.Sprintf("Karpenter nominated NodeClaim %s, which is gone, and no refusal is recorded for the pool; it retries", lf.Claim)
			}
		}
		if unread := lf.unread(); unread != "" {
			message += "; " + unread
		}
		return reason, message, refusal
	}
	if nominated != nil {
		return reasonNodeLaunching, nominated.Message, nil
	}
	reason, message = podPendingReason(pf.Pod)
	if reason == "" {
		if ev := lastEvent(pf.Events, eventFailedScheduling, ""); ev != nil {
			reason, message = ev.Reason, ev.Message
		}
	}
	return reason, message, nil
}

// download covers downloadingWeights: the storage-initializer init container.
func download(t *timeline, pf podFacts) bool {
	p := pf.Pod
	boundAt := conditionTime(p, corev1.PodScheduled, p.CreationTimestamp.Time)
	init := containerStatus(p.Status.InitContainerStatuses, initContainerName)
	if init == nil {
		t.done(stepWeights, boundAt, "")
		if len(p.Spec.InitContainers) == 0 {
			t.steps[stepWeights].Message = "no storage-initializer: the runtime fetches the weights itself"
		}
		return true
	}
	switch {
	case init.State.Terminated != nil:
		term := init.State.Terminated
		t.begin(stepWeights, term.StartedAt.Time)
		if term.ExitCode != 0 {
			t.fail(stepWeights, nonEmpty(term.Reason, "Error"), fmt.Sprintf("%s exited %d: %s", initContainerName, term.ExitCode, nonEmpty(term.Message, "no message")))
			return false
		}
		took := term.FinishedAt.Sub(term.StartedAt.Time)
		cached := took <= cachedWithin
		t.done(stepWeights, term.FinishedAt.Time, "")
		t.steps[stepWeights].Cached = &cached
		if cached {
			t.steps[stepWeights].Message = fmt.Sprintf("the cache claim already held the weights (%s finished in %s)", initContainerName, took.Round(100*time.Millisecond))
		}
		return true
	case init.State.Running != nil:
		startedAt := init.State.Running.StartedAt.Time
		t.begin(stepWeights, startedAt)
		running := pf.Now.Sub(startedAt)
		switch {
		case init.RestartCount > 0:
			t.fail(stepWeights, reasonDownloadStalled, fmt.Sprintf("%s restarted %d× (%s)", initContainerName, init.RestartCount, lastTermination(init)))
			return false
		case running > downloadStallAfter:
			t.fail(stepWeights, reasonDownloadStalled, fmt.Sprintf("%s running for %s without finishing", initContainerName, running.Round(time.Minute)))
			return false
		}
		t.note(stepWeights, reasonDownloadingWeights, fmt.Sprintf("%s downloading the weights into the cache claim", initContainerName))
		return false
	default:
		t.begin(stepWeights, boundAt)
		if w := init.State.Waiting; w != nil {
			if isPullFailure(w.Reason) {
				t.fail(stepWeights, w.Reason, w.Message)
				return false
			}
			t.note(stepWeights, w.Reason, w.Message)
		}
		return false
	}
}

// pull covers pullingImage: the runtime container's image.
func pull(t *timeline, pf podFacts) bool {
	p := pf.Pod
	if len(p.Status.ContainerStatuses) == 0 {
		t.begin(stepImage, stepEnd(t, stepWeights, pf.Now))
		return false
	}
	main := runtimeStatus(p)
	field := "spec.containers{" + main.Name + "}"
	// The image's first pull: a restarted container is Pulled again
	// (already present on the node), which is the crash loop, not the step.
	pulling := firstEvent(pf.Events, eventPulling, field)
	pulled := firstEvent(pf.Events, eventPulled, field)
	since := stepEnd(t, stepWeights, pf.Now)
	if at := eventTime(pulling, true); !at.IsZero() {
		since = at
	}
	t.begin(stepImage, since)
	if w := main.State.Waiting; w != nil {
		if isPullFailure(w.Reason) {
			t.fail(stepImage, w.Reason, w.Message)
			return false
		}
		if pulled == nil && !crashed(main) {
			message := w.Message
			if pulling != nil {
				message = pulling.Message
			}
			t.note(stepImage, nonEmpty(w.Reason, eventPulling), message)
			return false
		}
		// Pulled — or run before: a crashed runtime had its image, whether
		// or not the Pulled event still exists — but not Running: created,
		// or the kubelet backed off from restarting a runtime that keeps
		// dying, which fails the loading step.
		pulledAt, message := eventTime(pulled, true), ""
		if pulled != nil {
			message = pulled.Message
		}
		if pulledAt.IsZero() {
			pulledAt = since
		}
		t.done(stepImage, pulledAt, message)
		t.begin(stepLoading, pulledAt)
		switch {
		case isCrash(w.Reason):
			t.fail(stepLoading, w.Reason, crashMessage(main, pf.Crash, w.Message))
		case crashed(main):
			t.note(stepLoading, reasonCrashLoop, crashMessage(main, pf.Crash, w.Message))
		default:
			t.note(stepLoading, w.Reason, w.Message)
		}
		return false
	}
	pulledAt := eventTime(pulled, true)
	message := ""
	if pulled != nil {
		message = pulled.Message
	}
	if pulledAt.IsZero() {
		pulledAt = containerStart(main, since)
	}
	t.done(stepImage, pulledAt, message)
	return true
}

// load covers loading: the runtime container up to its probes passing. A
// runtime that died is named (CrashLoop, the crash count, the exit code, the
// crashed instance's last error line) and the step stays under way since the
// image was pulled while the kubelet restarts it; it fails when the kubelet
// will not (restartPolicy Never) — the back-off fails it in pull.
func load(t *timeline, pf podFacts) bool {
	p := pf.Pod
	main := runtimeStatus(p)
	since := stepEnd(t, stepImage, pf.Now)
	if crashed(main) {
		t.begin(stepLoading, since)
		message := crashMessage(main, pf.Crash, "")
		if term := main.State.Terminated; term != nil && p.Spec.RestartPolicy == corev1.RestartPolicyNever {
			t.fail(stepLoading, nonEmpty(term.Reason, "Error"), message)
			return false
		}
		t.note(stepLoading, reasonCrashLoop, message)
		return false
	}
	t.begin(stepLoading, containerStart(main, since))
	if !main.Ready {
		message := "the runtime is loading the model; the startup probe has not passed yet"
		if ev := lastEvent(pf.Events, eventUnhealthy, "spec.containers{"+main.Name+"}"); ev != nil {
			message = ev.Message
		}
		t.note(stepLoading, reasonLoadingModel, message)
		return false
	}
	t.done(stepLoading, conditionTime(p, corev1.ContainersReady, pf.Now), "")
	return true
}

// stepEnd is when step i finished, else fallback.
func stepEnd(t *timeline, i int, fallback time.Time) time.Time {
	if at := t.steps[i].FinishedAt; at != nil {
		return *at
	}
	return fallback
}

func containerStart(cs *corev1.ContainerStatus, fallback time.Time) time.Time {
	if r := cs.State.Running; r != nil && !r.StartedAt.IsZero() {
		return r.StartedAt.Time
	}
	return fallback
}

func containerStatus(list []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

// runtimeStatus is the runtime container's status — the pod's first
// container; nil before the kubelet reports one.
func runtimeStatus(p *corev1.Pod) *corev1.ContainerStatus {
	if len(p.Status.ContainerStatuses) == 0 {
		return nil
	}
	return &p.Status.ContainerStatuses[0]
}

// crashed reports whether an instance of the container died: the kubelet
// restarted it, recorded its termination, or reports it terminated now.
func crashed(cs *corev1.ContainerStatus) bool {
	return cs.RestartCount > 0 || cs.LastTerminationState.Terminated != nil || cs.State.Terminated != nil
}

// crashMessage names a runtime's crashes: how often it died, the exit code
// (and the reason unless the plain Error) of the last death, the last error
// line of the crashed instance's log — or why that line is missing, never
// silently — and the kubelet's own words when given (its back-off).
func crashMessage(cs *corev1.ContainerStatus, log crashLog, kubelet string) string {
	crashes := cs.RestartCount
	if cs.State.Running == nil {
		crashes++ // the latest instance is dead too
	}
	term := cs.State.Terminated
	if term == nil {
		term = cs.LastTerminationState.Terminated
	}
	exit := "exit code unknown"
	if term != nil {
		exit = fmt.Sprintf("exit %d", term.ExitCode)
		if r := term.Reason; r != "" && r != "Error" {
			exit += ", " + r
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "runtime crashed %d× (%s)", crashes, exit)
	switch {
	case log.Err != nil:
		fmt.Fprintf(&b, "; the crashed container's log could not be read (%v)", log.Err)
	case log.Line == "":
		fmt.Fprintf(&b, "; no error line in the last %d lines of the crashed container's log", crashLogTail)
	default:
		b.WriteString(": " + log.Line)
	}
	if kubelet = strings.TrimSpace(kubelet); kubelet != "" {
		b.WriteString("; " + kubelet)
	}
	return b.String()
}

// lastErrorLine is the last line of a log that names an error — from the
// exception on when the line carries one — bounded to crashLineMax
// characters; empty when no line does.
func lastErrorLine(log string) string {
	line := ""
	for _, l := range strings.Split(log, "\n") {
		if errorLine.MatchString(l) {
			line = strings.TrimSpace(l)
		}
	}
	if at := exceptionAt.FindStringIndex(line); at != nil {
		line = line[at[0]:]
	}
	if r := []rune(line); len(r) > crashLineMax {
		line = string(r[:crashLineMax-1]) + "…"
	}
	return line
}

func lastTermination(cs *corev1.ContainerStatus) string {
	term := cs.LastTerminationState.Terminated
	if term == nil {
		return "no termination recorded"
	}
	return fmt.Sprintf("last exit %d %s %s", term.ExitCode, term.Reason, strings.TrimSpace(term.Message))
}

func isPullFailure(reason string) bool {
	switch reason {
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "ErrImageNeverPull", "CreateContainerConfigError", "CreateContainerError":
		return true
	}
	return false
}

func isCrash(reason string) bool {
	return reason == "CrashLoopBackOff" || reason == "RunContainerError"
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func requestsGPU(p *corev1.Pod, gpuResource string) bool {
	for _, c := range p.Spec.Containers {
		if q, ok := c.Resources.Requests[corev1.ResourceName(gpuResource)]; ok && !q.IsZero() {
			return true
		}
		if q, ok := c.Resources.Limits[corev1.ResourceName(gpuResource)]; ok && !q.IsZero() {
			return true
		}
	}
	return false
}

func conditionIs(p *corev1.Pod, kind corev1.PodConditionType, status corev1.ConditionStatus) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == kind {
			return c.Status == status
		}
	}
	return false
}

// conditionTime is the condition's lastTransitionTime, else fallback.
func conditionTime(p *corev1.Pod, kind corev1.PodConditionType, fallback time.Time) time.Time {
	for _, c := range p.Status.Conditions {
		if c.Type == kind && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time
		}
	}
	return fallback
}

// lastEvent is the latest event with the reason, for the object's fieldPath
// (a container) when given; nil without one.
func lastEvent(events []corev1.Event, reason, fieldPath string) *corev1.Event {
	return pickEvent(events, reason, fieldPath, func(e, best *corev1.Event) bool {
		return eventTime(e, false).After(eventTime(best, false))
	})
}

// firstEvent is the earliest event with the reason (the image's first pull,
// not a restarted container's), for the fieldPath when given.
func firstEvent(events []corev1.Event, reason, fieldPath string) *corev1.Event {
	return pickEvent(events, reason, fieldPath, func(e, best *corev1.Event) bool {
		return eventTime(e, true).Before(eventTime(best, true))
	})
}

func pickEvent(events []corev1.Event, reason, fieldPath string, better func(e, best *corev1.Event) bool) *corev1.Event {
	var best *corev1.Event
	for i := range events {
		e := &events[i]
		if e.Reason != reason || (fieldPath != "" && e.InvolvedObject.FieldPath != fieldPath) {
			continue
		}
		if best == nil || better(e, best) {
			best = e
		}
	}
	return best
}

// eventTime is when an event (last) happened — or first, for the start of a
// step; zero for nil.
func eventTime(e *corev1.Event, first bool) time.Time {
	if e == nil {
		return time.Time{}
	}
	if first && !e.FirstTimestamp.IsZero() {
		return e.FirstTimestamp.Time
	}
	for _, t := range []time.Time{e.LastTimestamp.Time, e.EventTime.Time, e.FirstTimestamp.Time} {
		if !t.IsZero() {
			return t
		}
	}
	return e.CreationTimestamp.Time
}

// podEvents lists the Events of one pod, bounded by eventsTimeout; none when
// the read fails (the phase is then computed from the pod alone).
func (b *Backend) podEvents(ctx context.Context, p *corev1.Pod) []corev1.Event {
	ctx, cancel := context.WithTimeout(ctx, eventsTimeout)
	defer cancel()
	list, err := b.k8s(ctx).CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.uid=" + string(p.UID)})
	if err != nil {
		b.log.Warn("listing the predictor pod's events failed; phases are read from the pod alone", "pod", p.Name, "error", err)
		return nil
	}
	out := make([]corev1.Event, 0, len(list.Items))
	for _, e := range list.Items {
		if e.InvolvedObject.UID == p.UID {
			out = append(out, e)
		}
	}
	return out
}

// crashLog reads the log of the runtime container instance that died — the
// previous instance while the kubelet restarts the container, the current
// one while it lies terminated — as the caller, bounded like the Events, and
// keeps its last error line. A read that fails keeps why, so the step says
// it instead of dropping the line (a caller who may not read pods/log).
func (b *Backend) crashLog(ctx context.Context, p *corev1.Pod, cs *corev1.ContainerStatus) crashLog {
	ctx, cancel := context.WithTimeout(ctx, eventsTimeout)
	defer cancel()
	opts := corev1.PodLogOptions{Container: cs.Name, TailLines: ptr.To(int64(crashLogTail)), Previous: cs.State.Terminated == nil}
	out, err := b.podLog(ctx, p.Namespace, p.Name, opts)
	if err != nil {
		return crashLog{Err: err}
	}
	return crashLog{Line: lastErrorLine(out)}
}

// nodeGPUs maps every node to its allocatable accelerator count; nil when
// nodes cannot be read.
func (b *Backend) nodeGPUs(ctx context.Context, gpuResource string) map[string]int64 {
	nodes, err := b.k8s(ctx).CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		b.log.Warn("listing nodes for the served models' phases failed", "error", err)
		return nil
	}
	out := make(map[string]int64, len(nodes.Items))
	for _, n := range nodes.Items {
		q := n.Status.Allocatable[corev1.ResourceName(gpuResource)]
		out[n.Name] = q.Value()
	}
	return out
}

// weightsBytes fills the weights step with the download's size — the
// preset's weights as total and, while the initializer runs in cache-agent
// mode, the cache directory's current size from a bounded scan of the node.
func (b *Backend) weightsBytes(ctx context.Context, sv *served, total int64) {
	for i := range sv.Steps {
		s := &sv.Steps[i]
		if s.Name != backend.PhaseDownloadingWeights {
			continue
		}
		s.BytesTotal = total
		if s.State != backend.StepInProgress || !b.liveCache || sv.Node == "" {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, eventsTimeout)
		defer cancel()
		snap := b.inv.snapshot(ctx, sv.Node, 0, true, b.scan)
		for _, e := range snap.Entries {
			if e.Dir == sv.Name {
				s.BytesCompleted = e.Bytes
				if total > 0 {
					s.Message = fmt.Sprintf("%s downloading the weights into the cache claim: %s of %s", initContainerName, humanBytes(e.Bytes), humanBytes(total))
				}
			}
		}
		return
	}
}
