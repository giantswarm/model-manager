package kserve

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// cacheUID is the uid of every process model-manager runs against the cache
// claim: the KServe storage-initializer's (its image's USER, what a predictor's
// download writes as), the pre-warm download's, the scan's and the removal's.
// Every such pod sets it as its fsGroup too, so the kubelet makes the claim's
// contents group-owned and group-writable when it mounts the claim read-write
// (a CSI driver whose fsGroupPolicy allows it; the read-only scan mount is
// left as it is and reads what the last read-write mount prepared). That is
// what lets the pods stay within the restricted Pod Security Standard, which
// Giant Swarm clusters enforce through Kyverno on every namespace: non-root,
// no capability added, a seccomp profile — root and DAC_OVERRIDE/FOWNER, the
// Pod Security baseline's allowance, are denied there.
const cacheUID = int64(1000)

// cachePodSecurityContext is the pod-level security context of every pod
// mounting the cache claim: the restricted profile's runAsNonRoot and seccomp
// profile, and the cache uid as user, group and fsGroup.
func cachePodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		RunAsUser:      ptr.To(cacheUID),
		RunAsGroup:     ptr.To(cacheUID),
		FSGroup:        ptr.To(cacheUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// cacheToolSecurityContext is the security context of the shell containers
// that scan, prepare and clean the cache (the init image): the cache uid, no
// privilege escalation, a read-only root filesystem and every capability
// dropped, none added.
func cacheToolSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                ptr.To(cacheUID),
		RunAsNonRoot:             ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}
