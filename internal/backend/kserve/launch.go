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
	"k8s.io/utils/ptr"
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
//
// A capacity refusal is read whole (giantswarm/model-manager#125). Karpenter's
// event holds the first 300 characters of the cloud's answer — the first size
// and the zone, the rest cut off — so what the claim asked for comes from its
// requirements while it stands, else from the pool's template (the same
// constraint, standing after the claim is gone); the sizes and zones the
// cloud names come from every refusal text, the zones with capacity where
// the text is whole; and whether the zone is the model cache claim's — the
// pool's pin, cluster-manager's `pool.zones` — from the claim's volume.
const (
	// karpenterEventsNamespace is where the events of Karpenter's
	// cluster-scoped NodeClaims land.
	karpenterEventsNamespace = metav1.NamespaceDefault
	kindNodeClaim            = "NodeClaim"
	conditionLaunched        = "Launched"
	// nodeClaimSuffix is the random suffix Karpenter appends to the NodePool's
	// name when it names a claim (`<nodepool>-<five characters>`).
	nodeClaimSuffix = 5
	// refusalMessageMax bounds Karpenter's message where it is quoted as is.
	refusalMessageMax = 320

	// reasonNodeLaunching: a NodeClaim is nominated, no instance yet.
	reasonNodeLaunching = "NodeLaunching"
	// reasonCapacityUnavailable: Karpenter could not launch the node.
	reasonCapacityUnavailable = "CapacityUnavailable"

	// The requirement keys that say what a claim asks the cloud for: the
	// instance types Karpenter resolved on the NodeClaim; the family and
	// the sizes a NodePool's template names instead (the pool chart's
	// shape); the zone either constrains the node to (labelZone).
	requirementInstanceType   = "node.kubernetes.io/instance-type"
	requirementInstanceFamily = "karpenter.k8s.aws/instance-family"
	requirementInstanceSize   = "karpenter.k8s.aws/instance-size"
)

// Karpenter's NodeClaim and NodePool (cluster-scoped); a claim is named
// after its pool.
var (
	nodeClaimGVR = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodeclaims"}
	nodePoolGVR  = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}
)

// launchRefusalEvents are Karpenter's event reasons for a claim it gave up
// on and deleted at once.
var launchRefusalEvents = map[string]bool{"InsufficientCapacityError": true, "NodeClassNotReady": true}

// nominatedClaim finds the NodeClaim in Karpenter's Nominated event.
var nominatedClaim = regexp.MustCompile(`nodeclaim/([a-z0-9][-a-z0-9.]*)`)

// The cloud's account of a capacity refusal, as EC2's CreateFleet words it
// per size and Karpenter quotes it: `InsufficientInstanceCapacity: We
// currently do not have sufficient g6e.2xlarge capacity in the Availability
// Zone you requested (eu-central-1b). Our system will be working on
// provisioning additional capacity. You can currently get g6e.2xlarge
// capacity by not specifying an Availability Zone in your request or
// choosing eu-central-1a, eu-central-1c.` — read for the size, the zone and
// the zones with capacity.
var (
	refusedSize       = regexp.MustCompile(`do not have sufficient ([a-z0-9-]+\.[a-z0-9]+) capacity`)
	refusedZone       = regexp.MustCompile(`Availability Zone you requested \(([a-z][a-z0-9-]*\d[a-z])\)`)
	zonesWithCapacity = regexp.MustCompile(`choosing ([a-z][a-z0-9-]*\d[a-z](?:, [a-z][a-z0-9-]*\d[a-z])*)`)
)

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
	// Pool is what the pool's template asks the cloud for — the claim's
	// constraint, standing after the claim is gone — read when a refusal
	// stands; PoolErr says why it could not be read.
	Pool    constraints
	PoolErr error
	// Cache is where the serving namespace's cache claim lives — its zones
	// decide whether the pool's zone is the claim's — read when a refusal
	// stands; CacheErr says why it could not be read.
	Cache    cacheLocation
	CacheErr error
}

// claimState is the part of a NodeClaim that decides the step: whether an
// instance launched, the refusal when none could, and what was asked for.
type claimState struct {
	Created time.Time
	// Launched is the Launched condition's status; empty before Karpenter
	// set one. Since is its transition; Reason and Message its words.
	Launched        corev1.ConditionStatus
	Since           time.Time
	Reason, Message string
	// Node is the node the claim registered, once it did.
	Node string
	// Asked is what the claim asks the cloud for (its requirements).
	Asked constraints
}

// constraints are what a claim asks the cloud for: the instance types and
// the zones of its In-requirements, in the order given. A NodePool's
// template names the family and the sizes instead of the types; they are
// composed (`g6e` × `2xlarge` = `g6e.2xlarge`).
type constraints struct {
	InstanceTypes, Zones []string
}

func (c constraints) empty() bool { return len(c.InstanceTypes) == 0 && len(c.Zones) == 0 }

// parseRequirements reads the requirements of a NodeClaim's spec or a
// NodePool's template.
func parseRequirements(reqs []any) constraints {
	var c constraints
	var families, sizes []string
	for _, r := range reqs {
		req, ok := r.(map[string]any)
		if !ok || req["operator"] != string(corev1.NodeSelectorOpIn) {
			continue
		}
		values, _, _ := unstructured.NestedStringSlice(req, "values")
		switch req["key"] {
		case requirementInstanceType:
			c.InstanceTypes = append(c.InstanceTypes, values...)
		case requirementInstanceFamily:
			families = append(families, values...)
		case requirementInstanceSize:
			sizes = append(sizes, values...)
		case labelZone:
			c.Zones = append(c.Zones, values...)
		}
	}
	if len(c.InstanceTypes) == 0 {
		for _, f := range families {
			for _, s := range sizes {
				c.InstanceTypes = append(c.InstanceTypes, f+"."+s)
			}
		}
	}
	return c
}

// launchRefusal is one NodeClaim Karpenter could not launch: its name,
// Karpenter's reason and message and when it said so.
type launchRefusal struct {
	Claim           string
	Reason, Message string
	At              time.Time
}

// refusals are the refusals standing against the pod, oldest first: the
// refusal events newer than the nominated claim (a claim created after a
// refusal is Karpenter's retry: the claim speaks for itself), every event
// when the claim is gone, and the nominated claim's own Launched=False.
func (lf launchFacts) refusals() []launchRefusal {
	all := make([]launchRefusal, 0, len(lf.Refusals)+1)
	for _, r := range lf.Refusals {
		if lf.State == nil || r.At.After(lf.State.Created) {
			all = append(all, r)
		}
	}
	if s := lf.State; s != nil && s.Launched == corev1.ConditionFalse {
		all = append(all, launchRefusal{Claim: lf.Claim, Reason: s.Reason, Message: s.Message, At: s.Since})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].At.Before(all[j].At) })
	return all
}

// launched reports when the nominated claim launched an instance, if it has.
func (lf launchFacts) launched() (time.Time, bool) {
	if s := lf.State; s != nil && s.Launched == corev1.ConditionTrue {
		return s.Since, true
	}
	return time.Time{}, false
}

// asked is what the refused claim asked the cloud for: its own requirements
// while it stands, else the pool's.
func (lf launchFacts) asked() constraints {
	if s := lf.State; s != nil && !s.Asked.empty() {
		return s.Asked
	}
	return lf.Pool
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
	if lf.PoolErr != nil {
		parts = append(parts, fmt.Sprintf("what NodePool %s asks for could not be read (%v)", nodeClaimPool(lf.Claim), lf.PoolErr))
	}
	if lf.CacheErr != nil {
		parts = append(parts, fmt.Sprintf("where the cache claim lives could not be read (%v)", lf.CacheErr))
	}
	return strings.Join(parts, "; ")
}

// capacityRefusal is what stands against the pod once the refusals are
// read: the sizes the cloud refused, the zones the claim asked for, the
// zones the cloud named as having capacity, whether the cache claim pins
// the pool to the zone — the scheduling step's fields — and whose
// constraint the zone is, for the step's message. Empty when no refusal
// carries the cloud's capacity wording.
type capacityRefusal struct {
	RefusedInstanceTypes []string
	RequestedZones       []string
	AvailableZones       []string
	// PinnedByCache is true when the cache claim's volume lies in a
	// requested zone (the pool follows the cache), false when the claim
	// pins nothing there, nil when that could not be read.
	PinnedByCache *bool
	// CacheClaim names the claim that pins.
	CacheClaim string
	// PoolZones says the requested zones are the pool's own constraint.
	PoolZones bool
}

// capacity reads the refusals for what the cloud refused and what would
// launch: every size the claim asked for (a fleet that launched nothing was
// refused every size) and every size the cloud names, the zones asked for
// and the zone the cloud names, and the zones it names as having capacity
// where its answer is whole.
func (lf launchFacts) capacity(refusals []launchRefusal) capacityRefusal {
	asked := lf.asked()
	cr := capacityRefusal{
		RefusedInstanceTypes: append([]string(nil), asked.InstanceTypes...),
		RequestedZones:       append([]string(nil), asked.Zones...),
	}
	named := false
	for _, r := range refusals {
		for _, m := range refusedSize.FindAllStringSubmatch(r.Message, -1) {
			cr.RefusedInstanceTypes, named = appendMissing(cr.RefusedInstanceTypes, m[1]), true
		}
		for _, m := range refusedZone.FindAllStringSubmatch(r.Message, -1) {
			cr.RequestedZones = appendMissing(cr.RequestedZones, m[1])
		}
		for _, m := range zonesWithCapacity.FindAllStringSubmatch(r.Message, -1) {
			for _, z := range strings.Split(m[1], ", ") {
				cr.AvailableZones = appendMissing(cr.AvailableZones, z)
			}
		}
	}
	if !named {
		return capacityRefusal{}
	}
	cr.PoolZones = len(lf.Pool.Zones) > 0 && subset(cr.RequestedZones, lf.Pool.Zones)
	cr.PinnedByCache, cr.CacheClaim = lf.pinnedByCache(cr.RequestedZones)
	return cr
}

// pinnedByCache says whether the serving namespace's cache claim pins the
// pool to one of the zones: its volume's node affinity names one of them
// (cluster-manager pins a pool created while the claim is Bound to its
// zone). False when nothing pins — no claim, no volume yet, a volume naming
// no zone or another one; nil when the claim or its volume could not be
// read, or the zones are unknown.
func (lf launchFacts) pinnedByCache(zones []string) (*bool, string) {
	c := lf.Cache
	if lf.CacheErr != nil || c.Claim == "" || len(zones) == 0 {
		return nil, c.Claim
	}
	if c.Missing || !c.Bound {
		return ptr.To(false), c.Claim
	}
	if !c.VolumeRead {
		return nil, c.Claim
	}
	for _, z := range c.Zones {
		if containsString(zones, z) {
			return ptr.To(true), c.Claim
		}
	}
	return ptr.To(false), c.Claim
}

// refusalMessage words a refusal: how many claims Karpenter could not
// launch, the last one and when, its reason — and, for a capacity refusal
// the cloud's answer was read from, every size refused, the zone and whose
// constraint it is, the zones with capacity when named, and the way out; a
// refusal read no further keeps Karpenter's words, bounded.
func refusalMessage(last launchRefusal, count int, cr capacityRefusal) string {
	var b strings.Builder
	claims := "NodeClaim"
	if count > 1 {
		claims = "NodeClaims"
	}
	fmt.Fprintf(&b, "Karpenter could not launch a node: %d %s refused, the last (%s) at %s — %s: ", count, claims, last.Claim, last.At.UTC().Format(time.RFC3339), last.Reason)
	if len(cr.RefusedInstanceTypes) == 0 {
		b.WriteString(boundMessage(last.Message))
		b.WriteString("; Karpenter retries while the pod waits")
		return b.String()
	}
	fmt.Fprintf(&b, "the cloud has no %s capacity", joinList(cr.RefusedInstanceTypes, "or"))
	pinned := cr.PinnedByCache != nil && *cr.PinnedByCache
	if len(cr.RequestedZones) > 0 {
		zone := "zone"
		if len(cr.RequestedZones) > 1 {
			zone = "zones"
		}
		fmt.Fprintf(&b, " in %s, the %s ", joinList(cr.RequestedZones, "and"), zone)
		switch {
		case pinned:
			fmt.Fprintf(&b, "the pool is pinned to by the cache claim %s (its volume lives there)", cr.CacheClaim)
		case cr.PoolZones:
			b.WriteString("the pool constrains its nodes to")
		default:
			b.WriteString("the claim was constrained to")
		}
	}
	if len(cr.AvailableZones) > 0 {
		fmt.Fprintf(&b, "; it has capacity in %s", joinList(cr.AvailableZones, "and"))
	}
	b.WriteString(". The way out is a pool in ")
	if len(cr.AvailableZones) > 0 {
		b.WriteString(joinList(cr.AvailableZones, "or"))
	} else {
		b.WriteString("another zone")
	}
	if pinned {
		b.WriteString(" (create_node_pool with zones naming it and cache: false), or removing the cache claim so the next pool is not pinned")
	} else {
		b.WriteString(" (create_node_pool with zones naming it)")
	}
	b.WriteString("; Karpenter retries while the pod waits")
	return b.String()
}

// claimRefusalMessage collapses Karpenter's message to one line and drops
// the event's prefix naming the claim.
func claimRefusalMessage(claim, message string) string {
	return strings.TrimPrefix(strings.Join(strings.Fields(message), " "), fmt.Sprintf("NodeClaim %s event: ", claim))
}

// boundMessage bounds a message quoted as is.
func boundMessage(message string) string {
	if r := []rune(message); len(r) > refusalMessageMax {
		return string(r[:refusalMessageMax-1]) + "…"
	}
	return message
}

// joinList words a list: `a`, `a and b`, `a, b and c` — with the
// conjunction given.
func joinList(items []string, conjunction string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + conjunction + " " + items[len(items)-1]
}

func appendMissing(list []string, s string) []string {
	if containsString(list, s) {
		return list
	}
	return append(list, s)
}

// subset reports whether every item of of is in in.
func subset(of, in []string) bool {
	for _, s := range of {
		if !containsString(in, s) {
			return false
		}
	}
	return true
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
// and — unless it launched — the refusal events of the pool's claims; when
// a refusal stands, what the pool asks for and where the cache claim lives,
// so the step says what would launch.
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
	pool := nodeClaimPool(lf.Claim)
	lf.Refusals, lf.RefusalsErr = b.launchRefusals(ctx, pool)
	if len(lf.refusals()) == 0 {
		return lf
	}
	lf.Pool, lf.PoolErr = b.poolConstraints(ctx, pool)
	lf.Cache, lf.CacheErr = b.cacheNodes(ctx)
	return lf
}

// parseClaim reads a NodeClaim's launch state and requirements.
func parseClaim(obj *unstructured.Unstructured) *claimState {
	s := &claimState{Created: obj.GetCreationTimestamp().UTC()}
	s.Node, _, _ = unstructured.NestedString(obj.Object, "status", "nodeName")
	reqs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "requirements")
	s.Asked = parseRequirements(reqs)
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

// poolConstraints reads what a NodePool's template asks the cloud for.
func (b *Backend) poolConstraints(ctx context.Context, pool string) (constraints, error) {
	if pool == "" {
		return constraints{}, nil
	}
	obj, err := b.dynamic(ctx).Resource(nodePoolGVR).Get(ctx, pool, metav1.GetOptions{})
	if err != nil {
		return constraints{}, err
	}
	reqs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "requirements")
	return parseRequirements(reqs), nil
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
