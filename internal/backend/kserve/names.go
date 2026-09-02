package kserve

import (
	"fmt"
	"hash/fnv"
	"strings"
)

const (
	maxNameLength = 63
	// jobPrefix / scanPrefix / rmPrefix name the pods the driver creates.
	jobPrefix  = "mm-pull-"
	scanPrefix = "mm-scan-"
	rmPrefix   = "mm-rm-"
)

// dnsLabel turns a free-form string (a Hugging Face repository id such as
// "Qwen/Qwen3-14B") into a DNS-1123 label: lower case, runs of other
// characters collapsed to one dash, at most 63 characters with a short hash
// suffix when truncated so distinct inputs never collide.
func dnsLabel(s string) string {
	raw := strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := true
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	name := strings.TrimRight(b.String(), "-")
	if name == "" {
		name = "model"
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "m-" + name
	}
	return truncateLabel(name, raw)
}

// truncateLabel shortens name to the DNS label limit, keeping it unique per
// original input.
func truncateLabel(name, original string) string {
	if len(name) <= maxNameLength {
		return name
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(original))
	suffix := fmt.Sprintf("-%08x", h.Sum32())
	return strings.TrimRight(name[:maxNameLength-len(suffix)], "-") + suffix
}

// prefixed builds "<prefix><name>" within the label limit.
func prefixed(prefix, name string) string {
	return truncateLabel(prefix+name, prefix+name)
}

// isRepoID reports whether s looks like a Hugging Face repository id
// (owner/name, no further slashes).
func isRepoID(s string) bool {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, " \t:") {
			return false
		}
	}
	return true
}

// splitRevision separates "owner/name:revision" (also "owner/name@revision").
func splitRevision(ref string) (repo, revision string) {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "hf://")
	if i := strings.LastIndexAny(ref, ":@"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}
