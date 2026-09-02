package kserve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/giantswarm/model-manager/internal/cacheagent"
)

// The cache layout the modelServing contract defines: one subdirectory per
// InferenceService under the claim root, mounted at cache.mountPath in the
// predictor. model-manager adds a hidden markers directory that records which
// repository a pre-warm download put into which directory.
const (
	cacheMount  = "/cache"
	cacheVolume = "cache"
	markersDir  = cacheagent.MarkersDir
	lineDir     = "DIR"
	lineMarker  = "MARKER"
	lineEnd     = "END"
	scriptShell = "sh"
)

// scanScript walks the cache root and prints one DIR line per subdirectory
// (name, apparent bytes, file count, mtime) and one MARKER line per marker
// file. busybox-compatible: find -exec stat -c, awk, wc.
const scanScript = `set -u
cd "${MM_CACHE_ROOT:-` + cacheMount + `}" || exit 1
for d in */ ; do
  d="${d%/}"
  [ -d "$d" ] || continue
  case "$d" in .*|'*') continue ;; esac
  bytes=$(find "$d" -type f -exec stat -c %s {} + 2>/dev/null | awk '{s+=$1} END {printf "%d", s}')
  files=$(find "$d" -type f 2>/dev/null | wc -l | tr -d ' ')
  mtime=$(stat -c %Y "$d" 2>/dev/null || echo 0)
  printf '` + lineDir + `\t%s\t%s\t%s\t%s\n' "$d" "${bytes:-0}" "${files:-0}" "${mtime:-0}"
done
if [ -d ` + markersDir + ` ]; then
  for m in ` + markersDir + `/*.json ; do
    [ -f "$m" ] || continue
    n="${m#` + markersDir + `/}"; n="${n%.json}"
    printf '` + lineMarker + `\t%s\t%s\n' "$n" "$(tr -d '\n' < "$m")"
  done
fi
echo ` + lineEnd

// marker is what a pre-warm download records about its directory; the
// cache-agent reads the same files.
type marker = cacheagent.Marker

// cacheEntry is one directory of the cache on one node.
type cacheEntry struct {
	Node   string
	Dir    string
	Bytes  int64
	Files  int
	MTime  time.Time
	Marker *marker
}

// cacheSnapshot is one node's scan.
type cacheSnapshot struct {
	Node      string
	Entries   []cacheEntry
	ScannedAt time.Time
	Err       error
}

// inventory caches scans per node.
type inventory struct {
	mu    sync.Mutex
	snaps map[string]*cacheSnapshot
	// inflight de-duplicates concurrent scans of one node.
	inflight map[string]chan struct{}
}

func newInventory() *inventory {
	return &inventory{snaps: map[string]*cacheSnapshot{}, inflight: map[string]chan struct{}{}}
}

// scanner runs one cache scan on a node; replaced in tests.
type scanner func(ctx context.Context, node string) ([]cacheEntry, string, error)

// snapshot returns the node's scan, reusing one younger than ttl. On failure
// the previous entries are kept and Err reports the failure.
func (inv *inventory) snapshot(ctx context.Context, node string, ttl time.Duration, force bool, scan scanner) *cacheSnapshot {
	for {
		inv.mu.Lock()
		if s, ok := inv.snaps[node]; ok && !force && time.Since(s.ScannedAt) < ttl {
			inv.mu.Unlock()
			return s
		}
		if ch, busy := inv.inflight[node]; busy {
			inv.mu.Unlock()
			select {
			case <-ch:
				force = false
				continue
			case <-ctx.Done():
				return &cacheSnapshot{Node: node, Err: ctx.Err()}
			}
		}
		ch := make(chan struct{})
		inv.inflight[node] = ch
		inv.mu.Unlock()

		entries, actualNode, err := scan(ctx, node)
		inv.mu.Lock()
		prev := inv.snaps[node]
		snap := &cacheSnapshot{Node: node, ScannedAt: time.Now()}
		if actualNode != "" {
			snap.Node = actualNode
		}
		if err != nil {
			snap.Err = err
			if prev != nil {
				snap.Entries = prev.Entries
				snap.ScannedAt = prev.ScannedAt
			}
		} else {
			snap.Entries = entries
		}
		inv.snaps[node] = snap
		delete(inv.inflight, node)
		close(ch)
		inv.mu.Unlock()
		return snap
	}
}

// invalidate drops cached scans so the next read rescans.
func (inv *inventory) invalidate() {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.snaps = map[string]*cacheSnapshot{}
}

// parseScan turns the scan output into entries.
func parseScan(node string, r io.Reader) ([]cacheEntry, error) {
	byDir := map[string]*cacheEntry{}
	var order []string
	markers := map[string]*marker{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	ended := false
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		switch fields[0] {
		case lineDir:
			if len(fields) < 5 {
				continue
			}
			bytes, _ := strconv.ParseInt(fields[2], 10, 64)
			files, _ := strconv.Atoi(fields[3])
			mtime, _ := strconv.ParseInt(fields[4], 10, 64)
			e := &cacheEntry{Node: node, Dir: fields[1], Bytes: bytes, Files: files}
			if mtime > 0 {
				e.MTime = time.Unix(mtime, 0).UTC()
			}
			byDir[e.Dir] = e
			order = append(order, e.Dir)
		case lineMarker:
			if len(fields) < 3 {
				continue
			}
			var m marker
			if err := json.Unmarshal([]byte(fields[2]), &m); err == nil {
				if m.Dir == "" {
					m.Dir = fields[1]
				}
				markers[fields[1]] = &m
			}
		case lineEnd:
			ended = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read scan output: %w", err)
	}
	if !ended {
		return nil, fmt.Errorf("scan output truncated (no %s line)", lineEnd)
	}
	out := make([]cacheEntry, 0, len(order))
	for _, d := range order {
		e := byDir[d]
		e.Marker = markers[d]
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// scanNode runs the scan script in a short-lived pod that mounts the cache
// claim read-only, pinned to node when given. Returns the entries and the node
// the pod actually ran on.
func (b *Backend) scanNode(ctx context.Context, node string) ([]cacheEntry, string, error) {
	s := b.cfg.settings(ctx)
	ctx, cancel := context.WithTimeout(ctx, b.opts.InventoryTimeout)
	defer cancel()
	pod := b.cachePod(prefixed(scanPrefix, node+"-"+shortID()), s, node, scanScript, true)
	logs, ranOn, err := b.runPod(ctx, pod)
	if err != nil {
		return nil, ranOn, fmt.Errorf("scan cache on %s: %w", nodeOrAny(node), err)
	}
	entries, err := parseScan(ranOn, strings.NewReader(logs))
	if err != nil {
		return nil, ranOn, err
	}
	return entries, ranOn, nil
}

// scanAgent reads the cache through the cache-agent pod on node (daemonset
// inventory mode): no pod is created. With an empty node (shared storage) any
// ready agent answers; the node it runs on is returned.
func (b *Backend) scanAgent(ctx context.Context, node string) ([]cacheEntry, string, error) {
	s := b.cfg.settings(ctx)
	pod, err := b.agentPod(ctx, s.Namespace, node)
	if err != nil {
		return nil, node, err
	}
	ctx, cancel := context.WithTimeout(ctx, b.opts.InventoryTimeout)
	defer cancel()
	url := "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(b.opts.InventoryAgentPort)) + cacheagent.InventoryPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, pod.Spec.NodeName, err
	}
	resp, err := b.agentHTTP.Do(req)
	if err != nil {
		return nil, pod.Spec.NodeName, fmt.Errorf("cache-agent %s on %s: %w", pod.Name, pod.Spec.NodeName, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, pod.Spec.NodeName, fmt.Errorf("cache-agent %s on %s: HTTP %d: %s", pod.Name, pod.Spec.NodeName, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var inv cacheagent.Inventory
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&inv); err != nil {
		return nil, pod.Spec.NodeName, fmt.Errorf("cache-agent %s on %s: decode inventory: %w", pod.Name, pod.Spec.NodeName, err)
	}
	for _, w := range inv.Warnings {
		b.log.Warn("cache-agent scan warning", "node", pod.Spec.NodeName, "detail", w)
	}
	entries := make([]cacheEntry, 0, len(inv.Entries))
	for _, e := range inv.Entries {
		entries = append(entries, cacheEntry{Node: pod.Spec.NodeName, Dir: e.Dir, Bytes: e.Bytes, Files: e.Files, MTime: e.MTime, Marker: e.Marker})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Dir < entries[j].Dir })
	return entries, pod.Spec.NodeName, nil
}

// agentPod finds a ready cache-agent pod on node (any node when empty).
func (b *Backend) agentPod(ctx context.Context, namespace, node string) (*corev1.Pod, error) {
	opts := metav1.ListOptions{LabelSelector: b.opts.InventoryAgentSelector}
	if node != "" {
		opts.FieldSelector = "spec.nodeName=" + node
	}
	pods, err := b.cs.CoreV1().Pods(namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list cache-agent pods in %s: %w", namespace, err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" || !podReady(p) {
			continue
		}
		if node != "" && p.Spec.NodeName != node {
			continue
		}
		return p, nil
	}
	return nil, fmt.Errorf("no ready cache-agent pod on %s (pods matching %q in %s): kserve.inventory.mode is %s — is the cache-agent DaemonSet scheduled there and mounting the claim?",
		nodeOrAny(node), b.opts.InventoryAgentSelector, namespace, InventoryModeDaemonSet)
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// removeDir deletes one cache directory (and its marker) on a node.
func (b *Backend) removeDir(ctx context.Context, node, dir string) error {
	s := b.cfg.settings(ctx)
	if dir == "" || strings.ContainsAny(dir, "/ \t\n") || strings.HasPrefix(dir, ".") {
		return fmt.Errorf("refusing to remove cache directory %q", dir)
	}
	ctx, cancel := context.WithTimeout(ctx, b.opts.InventoryTimeout)
	defer cancel()
	script := fmt.Sprintf("set -eu\ncd %s\nrm -rf -- %q %q\necho removed", cacheMount, dir, markersDir+"/"+dir+".json")
	pod := b.cachePod(prefixed(rmPrefix, dir+"-"+shortID()), s, node, script, false)
	if _, _, err := b.runPod(ctx, pod); err != nil {
		return fmt.Errorf("remove %s on %s: %w", dir, nodeOrAny(node), err)
	}
	return nil
}

// cachePod builds a one-shot pod mounting the cache claim. It runs as root:
// the claim root is root-owned and directories are created by the
// storage-initializer's uid, so a fixed non-root uid could not read or remove
// everything.
func (b *Backend) cachePod(name string, s settings, node, script string, readOnly bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.Namespace,
			Labels:    b.labels(map[string]string{ComponentLabel: "cache-tool"}),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  ptr.To(false),
			NodeName:                      node,
			TerminationGracePeriodSeconds: ptr.To[int64](5),
			Containers: []corev1.Container{{
				Name:    "tool",
				Image:   b.opts.InitImage,
				Command: []string{scriptShell, "-c", script},
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                ptr.To[int64](0),
					RunAsNonRoot:             ptr.To(false),
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
						Add:  []corev1.Capability{"DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER"},
					},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: cacheVolume, MountPath: cacheMount, ReadOnly: readOnly}},
			}},
			Volumes: []corev1.Volume{{
				Name: cacheVolume,
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: s.CacheClaim,
					ReadOnly:  readOnly,
				}},
			}},
		},
	}
	return pod
}

// runPod creates the pod, waits for it to finish, returns its logs and the
// node it ran on, and deletes it.
func (b *Backend) runPod(ctx context.Context, pod *corev1.Pod) (logs, node string, err error) {
	pods := b.cs.CoreV1().Pods(pod.Namespace)
	created, err := pods.Create(ctx, pod, metav1.CreateOptions{FieldManager: ManagedByValue})
	if err != nil {
		return "", "", fmt.Errorf("create pod %s: %w", pod.Name, err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = pods.Delete(dctx, created.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		p, err := pods.Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return "", "", fmt.Errorf("get pod %s: %w", created.Name, err)
		}
		if p != nil {
			node = p.Spec.NodeName
			switch p.Status.Phase {
			case corev1.PodSucceeded:
				out, err := b.podLogs(ctx, p.Namespace, p.Name, 0)
				return out, node, err
			case corev1.PodFailed:
				out, _ := b.podLogs(ctx, p.Namespace, p.Name, 20)
				return "", node, fmt.Errorf("pod failed: %s %s", podReason(p), strings.TrimSpace(out))
			}
			if reason := podStuckReason(p); reason != "" {
				return "", node, fmt.Errorf("pod cannot start: %s", reason)
			}
		}
		select {
		case <-ctx.Done():
			return "", node, fmt.Errorf("waiting for pod %s: %w", created.Name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// podLogs reads a pod's log (the whole log with tail 0).
func (b *Backend) podLogs(ctx context.Context, namespace, name string, tail int64) (string, error) {
	if b.logs != nil {
		return b.logs(ctx, namespace, name, tail)
	}
	opts := &corev1.PodLogOptions{}
	if tail > 0 {
		opts.TailLines = ptr.To(tail)
	}
	rc, err := b.cs.CoreV1().Pods(namespace).GetLogs(name, opts).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("read logs of %s: %w", name, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, 8<<20))
	if err != nil {
		return "", fmt.Errorf("read logs of %s: %w", name, err)
	}
	return string(data), nil
}

func podReason(p *corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return fmt.Sprintf("%s (exit %d) %s", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode, cs.State.Terminated.Message)
		}
	}
	return p.Status.Reason + " " + p.Status.Message
}

// podStuckReason reports an unrecoverable pending state (bad image, missing
// volume) so callers fail fast instead of waiting for the timeout.
func podStuckReason(p *corev1.Pod) string {
	for _, cs := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
				return w.Reason + ": " + w.Message
			}
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable && strings.Contains(c.Message, "persistentvolumeclaim") && strings.Contains(c.Message, "not found") {
			return c.Message
		}
	}
	return ""
}

func nodeOrAny(node string) string {
	if node == "" {
		return "any node"
	}
	return node
}

func shortID() string {
	return strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 36)
}
