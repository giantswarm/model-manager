package kserve

import (
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The GPU node pool's scheduling: a pool created through the platform is
// tainted (nvidia.com/gpu NoSchedule) so only accelerator work lands there,
// and labelled (giantswarm.io/machine-pool=<cluster>-<pool>). settings.GPUPool
// carries both — from the discovery ConfigMap's spec.gpuPool, the registered
// document's spec.kserve.gpuPool replacing it — and everything the backend
// schedules onto the pool gets them here: the toleration always, the node
// selector wherever the pod is not pinned to a node already.

// poolToleration is the toleration of the pool taint; false without one.
func (s settings) poolToleration() (corev1.Toleration, bool) {
	t := s.GPUPool.Taint
	if t == nil || t.Key == "" {
		return corev1.Toleration{}, false
	}
	tol := corev1.Toleration{Key: t.Key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffect(t.Effect)}
	if t.Value != "" {
		tol.Operator = corev1.TolerationOpEqual
		tol.Value = t.Value
	}
	return tol, true
}

// poolTolerations is the toleration as a list; nil without one.
func (s settings) poolTolerations() []corev1.Toleration {
	if tol, ok := s.poolToleration(); ok {
		return []corev1.Toleration{tol}
	}
	return nil
}

// schedule puts the pool's scheduling on a pod model-manager creates itself
// (a scan pod, a download Job's pod): the toleration, and the node selector
// unless the pod is pinned to a node — the pin already names the node (the
// cache node, which may live outside the pool) and a selector it does not
// match would fail the kubelet's admission.
func (s settings) schedule(spec *corev1.PodSpec) {
	spec.Tolerations = append(spec.Tolerations, s.poolTolerations()...)
	if spec.NodeName != "" || len(s.GPUPool.NodeSelector) == 0 {
		return
	}
	if spec.NodeSelector == nil {
		spec.NodeSelector = map[string]string{}
	}
	for k, v := range s.GPUPool.NodeSelector {
		spec.NodeSelector[k] = v
	}
}

// tolerations are a composed predictor's: the pool toleration first, the
// preset's scheduling.tolerations after it, an entry the preset repeats
// verbatim once. Empty when there is nothing.
func (p *servingPreset) tolerations(s settings) []any {
	var out []any
	if tol, ok := s.poolToleration(); ok {
		out = append(out, tolerationMap(tol))
	}
	for _, t := range p.Spec.Scheduling.Tolerations {
		m := map[string]any(t)
		if !containsMap(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// tolerationMap renders a toleration the way a preset writes one, so the
// pool's and a preset's identical entries compare equal.
func tolerationMap(t corev1.Toleration) map[string]any {
	m := map[string]any{"key": t.Key, "operator": string(t.Operator)}
	if t.Value != "" {
		m["value"] = t.Value
	}
	if t.Effect != "" {
		m["effect"] = string(t.Effect)
	}
	return m
}

func containsMap(list []any, m map[string]any) bool {
	for _, e := range list {
		if reflect.DeepEqual(e, m) {
			return true
		}
	}
	return false
}

// untolerated lists the node's hard taints (NoSchedule, NoExecute) the
// pool toleration does not cover — the taints that keep a predictor off the
// node — as key[=value]:effect in key order.
func (s settings) untolerated(taints []corev1.Taint) []string {
	tols := s.poolTolerations()
	var out []string
	for _, taint := range taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if toleratesTaint(tols, taint) {
			continue
		}
		out = append(out, formatTaint(taint))
	}
	sort.Strings(out)
	return out
}

// toleratesTaint applies the Kubernetes matching rules: an empty toleration
// effect matches every effect, an empty key with Exists every key, Exists
// ignores the value, Equal compares it.
func toleratesTaint(tols []corev1.Toleration, taint corev1.Taint) bool {
	for _, tol := range tols {
		if tol.Effect != "" && tol.Effect != taint.Effect {
			continue
		}
		if tol.Key != "" && tol.Key != taint.Key {
			continue
		}
		switch tol.Operator {
		case corev1.TolerationOpExists:
			return true
		case corev1.TolerationOpEqual, "":
			if tol.Value == taint.Value {
				return true
			}
		}
	}
	return false
}

func formatTaint(t corev1.Taint) string {
	var b strings.Builder
	b.WriteString(t.Key)
	if t.Value != "" {
		b.WriteString("=" + t.Value)
	}
	b.WriteString(":" + string(t.Effect))
	return b.String()
}

// gpuPoolReport is the pool scheduling for the backend descriptor; nil
// when none is configured.
func (s settings) gpuPoolReport() *backend.GPUPool {
	if s.GPUPool.Taint == nil && len(s.GPUPool.NodeSelector) == 0 {
		return nil
	}
	p := s.GPUPool
	return &p
}
