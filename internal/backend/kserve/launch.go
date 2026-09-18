package kserve

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Whether the node a Pending predictor waits for exists, by Karpenter's own
// account (giantswarm/model-manager#121). Karpenter's Nominated event on the
// pod ("Pod should schedule on: nodeclaim/<name>") says a NodeClaim was
// created for it, not that an instance came: a claim the cloud refuses for
// capacity (InsufficientInstanceCapacity) is deleted within seconds and
// another one nominated, for as long as Karpenter retries. So the scheduling
// step ends when the nominated claim's Launched condition is True — or the
// pod names a node — and a refusal is read from the claim's Launched=False
// condition while the claim stands, else from the Warning event Karpenter
// publishes on the claim before deleting it: in the default namespace, where
// the events of a cluster-scoped object go, outliving the claim by the API
// server's event TTL (an hour). Both are read as the caller; a read that
// fails is named in the step, never silent.
const (
	// karpenterEventsNamespace is where the events of Karpenter's
	// cluster-scoped NodeClaims land.
	karpenterEventsNamespace = metav1.NamespaceDefault
	kindNodeClaim            = "NodeClaim"
	conditionLaunched        = "Launched"
	// nodeClaimSuffix is the random suffix Karpenter appends to the NodePool's
	// name when it names a claim (`<nodepool>-<five characters>`).
	nodeClaimSuffix = 5
	// refusalMessageMax bounds Karpenter's message as quoted in the step.
	refusalMessageMax = 320

	// reasonNodeLaunching: a NodeClaim is nominated, no instance yet.
	reasonNodeLaunching = "NodeLaunching"
	// reasonCapacityUnavailable: Karpenter could not launch the node.
	reasonCapacityUnavailable = "CapacityUnavailable"
)

// nodeClaimGVR is Karpenter's NodeClaim (cluster-scoped).
var nodeClaimGVR = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodeclaims"}

// launchRefusalEvents are Karpenter's event reasons for a claim it gave up
// on and deleted at once.
var launchRefusalEvents = map[string]bool{"InsufficientCapacityError": true, "NodeClassNotReady": true}

// nominatedClaim finds the NodeClaim in Karpenter's Nominated event.
var nominatedClaim = regexp.MustCompile(`nodeclaim/([a-z0-9][-a-z0-9.]*)`)

// launchFacts is what Karpenter says about the node a pod without one waits
// for.
type launchFacts struct {
	// Claim names the NodeClaim Karpenter nominated for the pod (its
	// Nominated event); empty when it nominated none.
	Claim string
	// State is the nominated claim as read; nil when the claim is gone (a
	// refused claim is deleted at once) or could not be read.
	State *claimState
	// ClaimErr says why the claim could not be read (a caller who may not
	// get nodeclaims.karpenter.sh); nil when it was read or is gone.
	ClaimErr error
	// Refusals are Karpenter's refusals to launch a claim of the pool that
	// its Warning events still carry, oldest first; RefusalsErr says why the
	// events could not be read.
	Refusals    []launchRefusal
	RefusalsErr error
}

// claimState is the part of a NodeClaim that decides the step: whether an
// instance launched, and the refusal when none could.
type claimState struct {
	Created time.Time
	// Launched is the Launched condition's status; empty before Karpenter
	// set one. Since is its transition; Reason and Message its words.
	Launched        corev1.ConditionStatus
	Since           time.Time
	Reason, Message string
	// Node is the node the claim registered, once it did.
	Node string
}

// launchRefusal is one NodeClaim Karpenter could not launch: its name,
// Karpenter's reason and message and when it said so.
type launchRefusal struct {
	Claim           string
	Reason, Message string
	At              time.Time
}

// refusal is the refusal standing against the pod: the nominated claim's own
// Launched=False, else the latest refusal event newer than the nominated
// claim (a claim created after a refusal is Karpenter's retry: the claim
// speaks for itself), else the latest event when the claim is gone. The
// count is how many refusals the events and the claim carry.
func (lf launchFacts) refusal() (launchRefusal, int, bool) {
	all := make([]launchRefusal, 0, len(lf.Refusals)+1)
	for _, r := range lf.Refusals {
		if lf.State == nil || r.At.After(lf.State.Created) {
			all = append(all, r)
		}
	}
	if s := lf.State; s != nil && s.Launched == corev1.ConditionFalse {
		all = append(all, launchRefusal{Claim: lf.Claim, Reason: s.Reason, Message: s.Message, At: s.Since})
	}
	if len(all) == 0 {
		return launchRefusal{}, 0, false
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].At.Before(all[j].At) })
	return all[len(all)-1], len(all), true
}

// launched reports when the nominated claim launched an instance, if it has.
func (lf launchFacts) launched() (time.Time, bool) {
	if s := lf.State; s != nil && s.Launched == corev1.ConditionTrue {
		return s.Since, true
	}
	return time.Time{}, false
}

// unread names the reads that failed, for the step's message.
func (lf launchFacts) unread() string {
	var parts []string
	if lf.ClaimErr != nil {
		parts = append(parts, fmt.Sprintf("whether NodeClaim %s launched could not be read (%v)", lf.Claim, lf.ClaimErr))
	}
	if lf.RefusalsErr != nil {
		parts = append(parts, fmt.Sprintf("Karpenter's events in %s could not be read (%v)", karpenterEventsNamespace, lf.RefusalsErr))
	}
	return strings.Join(parts, "; ")
}

// refusalMessage words a refusal: how many claims Karpenter could not
// launch, the last one and when, its reason and its message.
func refusalMessage(last launchRefusal, count int) string {
	claims := "NodeClaim"
	if count > 1 {
		claims = "NodeClaims"
	}
	return fmt.Sprintf("Karpenter could not launch a node: %d %s refused, the last (%s) at %s — %s: %s; it retries while the pod waits",
		count, claims, last.Claim, last.At.UTC().Format(time.RFC3339), last.Reason, last.Message)
}

// claimRefusalMessage collapses Karpenter's message to one line, drops the
// event's prefix naming the claim and bounds it.
func claimRefusalMessage(claim, message string) string {
	message = strings.TrimPrefix(strings.Join(strings.Fields(message), " "), fmt.Sprintf("NodeClaim %s event: ", claim))
	if r := []rune(message); len(r) > refusalMessageMax {
		message = string(r[:refusalMessageMax-1]) + "…"
	}
	return message
}

// nodeClaimPool is the NodePool a claim is named after (`<nodepool>-<five
// characters>`); empty when the name has no such suffix.
func nodeClaimPool(claim string) string {
	if i := strings.LastIndex(claim, "-"); i > 0 && len(claim)-i-1 == nodeClaimSuffix {
		return claim[:i]
	}
	return ""
}

// launchFacts reads what Karpenter says about the node a pod without one
// waits for: the nominated claim, as the caller, bounded like the Events,
// and — unless it launched — the refusal events of the pool's claims.
func (b *Backend) launchFacts(ctx context.Context, events []corev1.Event) launchFacts {
	var lf launchFacts
	nominated := lastEvent(events, eventNominated, "")
	if nominated == nil {
		return lf
	}
	m := nominatedClaim.FindStringSubmatch(nominated.Message)
	if m == nil {
		return lf
	}
	lf.Claim = m[1]
	ctx, cancel := context.WithTimeout(ctx, eventsTimeout)
	defer cancel()
	obj, err := b.dynamic(ctx).Resource(nodeClaimGVR).Get(ctx, lf.Claim, metav1.GetOptions{})
	switch {
	case err == nil:
		lf.State = parseClaim(obj)
	case apierrors.IsNotFound(err):
	default:
		lf.ClaimErr = err
	}
	if _, ok := lf.launched(); ok {
		return lf
	}
	lf.Refusals, lf.RefusalsErr = b.launchRefusals(ctx, nodeClaimPool(lf.Claim))
	return lf
}

// parseClaim reads a NodeClaim's launch state.
func parseClaim(obj *unstructured.Unstructured) *claimState {
	s := &claimState{Created: obj.GetCreationTimestamp().UTC()}
	s.Node, _, _ = unstructured.NestedString(obj.Object, "status", "nodeName")
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		cond, ok := c.(map[string]any)
		if !ok || cond["type"] != conditionLaunched {
			continue
		}
		s.Launched = corev1.ConditionStatus(fmt.Sprint(cond["status"]))
		s.Reason, _ = cond["reason"].(string)
		if msg, _ := cond["message"].(string); msg != "" {
			s.Message = claimRefusalMessage(obj.GetName(), msg)
		}
		if at, _ := cond["lastTransitionTime"].(string); at != "" {
			if parsed, err := time.Parse(time.RFC3339, at); err == nil {
				s.Since = parsed.UTC()
			}
		}
	}
	return s
}

// launchRefusals lists Karpenter's refusal events on the pool's claims,
// oldest first: a Warning of a refusal reason on a NodeClaim named after
// the pool. The API server's field selectors narrow the list; the filter is
// applied again here (the fake client applies none).
func (b *Backend) launchRefusals(ctx context.Context, pool string) ([]launchRefusal, error) {
	if pool == "" {
		return nil, nil
	}
	list, err := b.k8s(ctx).CoreV1().Events(karpenterEventsNamespace).List(ctx, metav1.ListOptions{FieldSelector: "type=" + corev1.EventTypeWarning + ",involvedObject.kind=" + kindNodeClaim})
	if err != nil {
		return nil, err
	}
	var out []launchRefusal
	for i := range list.Items {
		e := &list.Items[i]
		claim := e.InvolvedObject.Name
		if e.Type != corev1.EventTypeWarning || e.InvolvedObject.Kind != kindNodeClaim || !launchRefusalEvents[e.Reason] || nodeClaimPool(claim) != pool {
			continue
		}
		out = append(out, launchRefusal{Claim: claim, Reason: e.Reason, Message: claimRefusalMessage(claim, e.Message), At: eventTime(e, false)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}
