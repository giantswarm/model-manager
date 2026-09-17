package kserve

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
)

func TestDNSLabelAndRevisions(t *testing.T) {
	assert.Equal(t, "qwen-qwen3-14b", dnsLabel("Qwen/Qwen3-14B"))
	assert.Equal(t, "hf-internal-testing-tiny-random-gpt2", dnsLabel("hf-internal-testing/tiny-random-gpt2"))
	assert.Equal(t, "m-123", dnsLabel("123"))
	assert.Equal(t, "model", dnsLabel("///"))
	long := dnsLabel(strings.Repeat("a", 70) + "/" + strings.Repeat("b", 30))
	assert.LessOrEqual(t, len(long), maxNameLength)
	assert.NotEqual(t, long, dnsLabel(strings.Repeat("a", 70)+"/"+strings.Repeat("c", 30)), "truncated names stay distinct")
	assert.Equal(t, "mm-pull-qwen-qwen3-14b", prefixed(jobPrefix, "qwen-qwen3-14b"))

	repo, rev := splitRevision("Qwen/Qwen3-14B:abc123")
	assert.Equal(t, "Qwen/Qwen3-14B", repo)
	assert.Equal(t, "abc123", rev)
	repo, rev = splitRevision("hf://org/name")
	assert.Equal(t, "org/name", repo)
	assert.Empty(t, rev)
	assert.True(t, isRepoID("org/name"))
	assert.False(t, isRepoID("name"))
	assert.False(t, isRepoID("a/b/c"))
	assert.False(t, isRepoID("org/na me"))
}

func TestParsePresetAndResolve(t *testing.T) {
	p, err := parsePreset([]byte(presetDoc("tiny", tinyRepo, 0.5, "")), "shipped")
	require.NoError(t, err)
	assert.Equal(t, "tiny", p.name())
	assert.Equal(t, tinyRepo, p.Spec.Model.ID)
	assert.EqualValues(t, 1, p.gpus())
	assert.Equal(t, gib/2, p.weightsBytes())
	assert.Equal(t, gib, p.overheadBytes(30))
	v := p.view(30)
	assert.Equal(t, gib/2+gib, v.RequiredBytes)
	assert.Equal(t, "shipped", v.Source)

	_, err = parsePreset([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n"), "")
	assert.Error(t, err)
	_, err = parsePreset([]byte("apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ServingPreset\nmetadata: {name: x}\nspec: {displayName: x}\n"), "")
	assert.ErrorContains(t, err, "spec.model.id")

	// Defaults filled when the published form omits them.
	d, err := parsePreset([]byte("apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ServingPreset\nmetadata: {name: d}\nspec: {displayName: d, model: {id: o/m}, requirements: {weightsGiB: 2}}\n"), "values")
	require.NoError(t, err)
	assert.Equal(t, "hf://o/m", d.Spec.Model.StorageURI)
	assert.Equal(t, "vLLM", d.Spec.Model.Format)
	assert.Equal(t, 30*gib, d.overheadBytes(30))

	other, _ := parsePreset([]byte(presetDoc("tiny-alt", tinyRepo, 0.5, "")), "values")
	idx := indexPresets([]*servingPreset{p, other, d})
	got, err := idx.resolve("", "tiny")
	require.NoError(t, err)
	assert.Equal(t, "tiny", got.name())
	_, err = idx.resolve(tinyRepo, "")
	assert.ErrorContains(t, err, "2 presets serve")
	got, err = idx.resolve("o/m", "")
	require.NoError(t, err)
	assert.Equal(t, "d", got.name())
	got, err = idx.resolve("d", "")
	require.NoError(t, err)
	assert.Equal(t, "d", got.name(), "a preset name resolves too")
	_, err = idx.resolve("o/m", "tiny")
	assert.ErrorContains(t, err, "serves org/tiny, not o/m")
	_, err = idx.resolve("o/m", "nope")
	assert.ErrorContains(t, err, "not found")
	got, err = idx.resolve("x/unknown", "")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestParseScanAndScript(t *testing.T) {
	out := strings.Join([]string{
		"DIR\ttiny\t453864\t2\t1700000000\t1",
		"DIR\tqwen3-14b\t0\t0\t1700000100\t0",
		"DIR\tlegacy\t5\t1\t1700000200",
		"MARKER\ttiny\t{\"model\":\"org/tiny\",\"dir\":\"tiny\",\"bytesExpected\":453864}",
		"junk line",
		"END",
	}, "\n")
	entries, err := parseScan("n1", strings.NewReader(out))
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, "legacy", entries[0].Dir)
	assert.True(t, entries[0].HasModel, "a DIR line without the model field (an older script) is a model")
	assert.Equal(t, "qwen3-14b", entries[1].Dir)
	assert.Nil(t, entries[1].Marker)
	assert.False(t, entries[1].HasModel)
	assert.False(t, entries[1].holdsModel())
	assert.Equal(t, "tiny", entries[2].Dir)
	assert.EqualValues(t, 453864, entries[2].Bytes)
	assert.Equal(t, 2, entries[2].Files)
	assert.True(t, entries[2].HasModel)
	assert.Equal(t, "org/tiny", entries[2].Marker.Model)
	assert.Equal(t, time.Unix(1700000000, 0).UTC(), entries[2].MTime)
	assert.True(t, cacheEntry{Marker: &marker{Model: "org/x"}}.holdsModel(), "a completed pre-warm download holds a model whatever its layout")
	assert.False(t, cacheEntry{Marker: &marker{}}.holdsModel())

	_, err = parseScan("n1", strings.NewReader("DIR\tx\t1\t1\t1\t1\n"))
	assert.ErrorContains(t, err, "truncated")

	// The real script against a temporary cache root: tiny has a config.json,
	// mistral only consolidated weights (Mistral layout, no config.json),
	// hf-home and xet are Hugging Face client internals, empty is empty.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "tiny", "sub"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "tiny", "config.json"), make([]byte, 100), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "tiny", "sub", "model.safetensors"), make([]byte, 2000), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "mistral"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "mistral", "consolidated-00001-of-00002.safetensors"), make([]byte, 300), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "mistral", "params.json"), make([]byte, 30), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "hf-home", "hub", "models--org--tiny", "refs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "hf-home", "hub", "models--org--tiny", "refs", "main"), []byte("abc"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "hf-home", "CACHEDIR.TAG"), []byte("Signature: 8a477f597d28d172789f06886806bc55"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "xet", "chunk-cache"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "xet", "chunk-cache", "c1"), make([]byte, 7), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "empty"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, markersDir), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, markersDir, "tiny.json"), []byte(`{"model":"org/tiny","dir":"tiny"}`+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "stray-file"), []byte("x"), 0o600))
	// Model weights are larger than 2 GiB: a sparse file with the apparent
	// size of a real checkpoint (no disk is used) guards the byte sum against
	// 32-bit formatting in the scan pod's awk.
	const bigBytes = int64(29_552_615_805) // Qwen/Qwen3-14B on disk
	require.NoError(t, os.MkdirAll(filepath.Join(root, "big"), 0o750))
	bigFile, err := os.Create(filepath.Join(root, "big", "model-00001-of-00008.safetensors")) // #nosec G304 -- test fixture under t.TempDir()
	require.NoError(t, err)
	require.NoError(t, bigFile.Truncate(bigBytes-1))
	require.NoError(t, bigFile.Close())
	require.NoError(t, os.WriteFile(filepath.Join(root, "big", "config.json"), []byte("{"), 0o600))
	// busybox is what the scan pod's alpine image ships; prefer it when the
	// machine has one so the test exercises the same awk.
	cmd := exec.Command("sh", "-c", scanScript)
	if busybox, err := exec.LookPath("busybox"); err == nil {
		cmd = exec.Command(busybox, "sh", "-c", scanScript) // #nosec G204 -- fixed script; busybox resolved from PATH
	}
	cmd.Env = append(os.Environ(), "MM_CACHE_ROOT="+root)
	raw, err := cmd.CombinedOutput()
	require.NoError(t, err, string(raw))
	entries, err = parseScan("n1", strings.NewReader(string(raw)))
	require.NoError(t, err, string(raw))
	byDir := map[string]cacheEntry{}
	for _, e := range entries {
		byDir[e.Dir] = e
	}
	require.Len(t, byDir, 6, string(raw))
	big := byDir["big"]
	assert.Equal(t, bigBytes, big.Bytes, string(raw))
	assert.Equal(t, 2, big.Files)
	assert.True(t, big.HasModel)
	assert.EqualValues(t, 0, byDir["empty"].Bytes)
	assert.False(t, byDir["empty"].HasModel)
	tiny := byDir["tiny"]
	assert.EqualValues(t, 2100, tiny.Bytes)
	assert.Equal(t, 2, tiny.Files)
	assert.True(t, tiny.HasModel, string(raw))
	require.NotNil(t, tiny.Marker)
	assert.Equal(t, "org/tiny", tiny.Marker.Model)
	assert.True(t, byDir["mistral"].HasModel, "weights at the top level count without a config.json: %s", raw)
	assert.EqualValues(t, 330, byDir["mistral"].Bytes)
	assert.False(t, byDir["hf-home"].HasModel, "the Hugging Face hub cache is not a model: %s", raw)
	assert.EqualValues(t, 3+len("Signature: 8a477f597d28d172789f06886806bc55"), byDir["hf-home"].Bytes, "but its bytes are counted")
	assert.False(t, byDir["xet"].HasModel, "the xet chunk cache is not a model: %s", raw)
	assert.Equal(t, 3, countModels(entries), "big, tiny and mistral, nothing else")
}

func TestParseProgressAndJobHelpers(t *testing.T) {
	n, ok := parseProgress("INFO downloading\nPROGRESS 10\nsome text\nPROGRESS 2048\n")
	assert.True(t, ok)
	assert.EqualValues(t, 2048, n)
	_, ok = parseProgress("nothing here")
	assert.False(t, ok)
	assert.Equal(t, "b | c", lastLines("a\nb\nc\n", 2))
}

func TestServedStatus(t *testing.T) {
	obj := func(status map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"status": status}}
	}
	s, reason, msg, ready := servedStatus(obj(map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}))
	assert.Equal(t, statusReady, s)
	assert.True(t, ready)
	assert.Empty(t, reason)
	assert.Empty(t, msg)

	s, reason, msg, ready = servedStatus(obj(map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "PredictorNotReady", "message": "waiting"}}}))
	assert.Equal(t, statusNotReady, s)
	assert.False(t, ready)
	assert.Equal(t, "PredictorNotReady", reason)
	assert.Equal(t, "PredictorNotReady waiting", msg)

	s, reason, msg, _ = servedStatus(obj(map[string]any{"modelStatus": map[string]any{"transitionStatus": "BlockedByFailedLoad", "lastFailureInfo": map[string]any{"reason": "ModelLoadFailed", "message": "OOM"}}}))
	assert.Equal(t, statusNotReady, s)
	assert.Equal(t, "ModelLoadFailed", reason)
	assert.Equal(t, "ModelLoadFailed OOM", msg)

	s, _, _, _ = servedStatus(obj(map[string]any{}))
	assert.Equal(t, statusPending, s)
	s, reason, _, _ = servedStatus(obj(map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "Unknown", "reason": "Scheduling"}}}))
	assert.Equal(t, statusPending, s)
	assert.Equal(t, "Scheduling", reason)
}

func TestHubHelpers(t *testing.T) {
	files := []hubFile{
		{Type: "file", Path: "config.json", Size: 100},
		{Type: "file", Path: "model-00001.safetensors", Size: 5, LFS: &struct {
			Size int64 `json:"size"`
		}{Size: 1000}},
		{Type: "file", Path: "pytorch_model.bin", Size: 3000},
		{Type: "file", Path: ".gitattributes", Size: 10},
	}
	assert.EqualValues(t, 1000, weightsFromTree(files), "safetensors win over legacy formats")
	assert.EqualValues(t, 3000, weightsFromTree(files[2:3]), "legacy formats when no safetensors")
	assert.EqualValues(t, 4110, downloadTotal(files, nil))
	assert.EqualValues(t, 1110, downloadTotal(files, []string{"*.bin"}))
	assert.EqualValues(t, 1100, downloadTotal(files, []string{"*.bin", ".git*"}))

	m := hubModel{Gated: "auto"}
	assert.True(t, m.isGated())
	m.Gated = false
	assert.False(t, m.isGated())
	assert.Equal(t, "org/na%20me", escapeRepo("org/na me"))
	assert.Equal(t, "1.0 GiB", humanBytes(gib))
	assert.Equal(t, "512 B", humanBytes(512))
}

func TestNormalizePredictorURL(t *testing.T) {
	// KServe raw deployment publishes status.address.url with the ingress
	// urlScheme; the predictor Service is plain HTTP on port 80 regardless.
	assert.Equal(t, "http://m-predictor.serving.svc.cluster.local", normalizePredictorURL("https://m-predictor.serving.svc.cluster.local/"))
	assert.Equal(t, "http://m-predictor.serving.svc", normalizePredictorURL("https://m-predictor.serving.svc"))
	assert.Equal(t, "http://m-predictor.serving.svc.cluster.local", normalizePredictorURL("http://m-predictor.serving.svc.cluster.local/"))
	assert.Equal(t, "https://m-serving.example.com", normalizePredictorURL("https://m-serving.example.com/"), "external hosts keep their scheme")
	assert.Equal(t, "https://m-predictor.serving.svc.cluster.local:8443", normalizePredictorURL("https://m-predictor.serving.svc.cluster.local:8443"), "an explicit port is deliberate")
	assert.Equal(t, "not a url", normalizePredictorURL("not a url"))
	assert.Equal(t, "", normalizePredictorURL(""))
}

func TestBudgetOfAnnotationWinsOverEverySource(t *testing.T) {
	n := node("dgx", "120Gi", map[string]string{labelGPUCount: "1", labelGPUMemory: "131072"})
	for _, source := range []string{budgetSourceAuto, budgetSourceGPULabels, budgetSourceAllocatable} {
		nb := budgetOf(n, DefaultGPUResourceName, source)
		assert.NotEqual(t, budgetSourceAnnotation, nb.BudgetSource, source)
		assert.Empty(t, nb.Message)
	}
	n.Annotations = map[string]string{BudgetAnnotation: " 96 "}
	for _, source := range []string{budgetSourceAuto, budgetSourceGPULabels, budgetSourceAllocatable} {
		nb := budgetOf(n, DefaultGPUResourceName, source)
		assert.Equal(t, budgetSourceAnnotation, nb.BudgetSource, source)
		assert.Equal(t, 96*gib, nb.Budget, source)
		assert.EqualValues(t, 128*gib, nb.GPUMemory, "the labels are still reported")
	}
	n.Annotations[BudgetAnnotation] = "1e400"
	nb := budgetOf(n, DefaultGPUResourceName, budgetSourceAllocatable)
	assert.Equal(t, budgetSourceAllocatable, nb.BudgetSource, "infinite values are ignored")
	assert.Contains(t, nb.Message, "1e400")
}

// pssBaselineCapabilities is the Pod Security "baseline" allow-list of added
// Every pod the kserve driver runs against the cache claim — the scan and
// removal pods, the download Job — satisfies the restricted Pod Security
// Standard a Giant Swarm cluster enforces through Kyverno: runAsNonRoot,
// runAsUser > 0, no capability added and ALL dropped, no privilege
// escalation, seccomp RuntimeDefault (the four rules that denied the scan pod:
// require-run-as-nonroot, require-run-as-non-root-user,
// disallow-capabilities-strict, restrict-seccomp-strict). They run as the
// cache uid with the same fsGroup, so the claim's contents are theirs to read
// and write without a capability.
func TestCachePodsSatisfyRestrictedPodSecurity(t *testing.T) {
	b := &Backend{opts: backend.KServeOptions{InitImage: "alpine:3", DownloadImage: "storage-initializer:test"}}
	s := settings{Namespace: "serving", CacheClaim: "hf-cache"}
	specs := map[string]corev1.PodSpec{
		"scan":     b.cachePod("scan", s, "node-a", "true", true).Spec,
		"remove":   b.cachePod("rm", s, "node-a", "true", false).Spec,
		"download": b.buildJob(downloadPlan{Dir: "tiny", Repo: tinyRepo, Node: "node-a"}, s).Spec.Template.Spec,
	}
	for name, spec := range specs {
		t.Run(name, func(t *testing.T) {
			assertRestrictedPodSecurity(t, spec)
			require.NotNil(t, spec.SecurityContext.FSGroup, "the claim's fsGroup is set")
			assert.EqualValues(t, cacheUID, *spec.SecurityContext.FSGroup, "fsGroup is the cache uid")
			for _, c := range append(spec.InitContainers, spec.Containers...) {
				assert.EqualValues(t, cacheUID, *c.SecurityContext.RunAsUser, "%s runs as the cache uid", c.Name)
			}
		})
	}
}

// The download Job downloads without Xet and gives up on a stalled download
// with a reason (giantswarm/model-manager#106): hf_xet connects to CDN
// addresses a Cilium toFQDNs DNS proxy never resolved and hangs on the dropped
// SYNs, hf_transfer resolves per connection through the system resolver. A
// download that writes nothing for the configured window exits
// stalledExitCode, which the Job's podFailurePolicy turns into a failed Job at
// once (a pod its node's disruption took is no attempt), and the stall line
// is the failure's reason rather than the log tail.
func TestDownloadJobDisablesXetAndFailsAStall(t *testing.T) {
	b := &Backend{opts: backend.KServeOptions{InitImage: "alpine:3", DownloadImage: "storage-initializer:test", DownloadStallTimeout: 7 * time.Minute}}
	job := b.buildJob(downloadPlan{Dir: "tiny", Repo: tinyRepo, Node: "node-a"}, settings{Namespace: "serving", CacheClaim: "hf-cache"})
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	c := job.Spec.Template.Spec.Containers[0]
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t, "1", env["HF_HUB_DISABLE_XET"], "the Xet path is off")
	assert.Equal(t, "1", env["HF_HUB_ENABLE_HF_TRANSFER"], "hf_transfer stays on")
	assert.Equal(t, "420", env["MM_STALL_SECONDS"], "the stall window in seconds")

	script := c.Command[2]
	assert.Contains(t, script, `"$idle" -ge "${MM_STALL_SECONDS:-600}"`, "the script's stall clock reads the window")
	assert.Contains(t, script, "allocated() { du -s -B1", "the clock reads bytes allocated on disk")
	assert.Contains(t, script, stalledMarker)
	assert.Contains(t, script, "exit "+strconv.Itoa(stalledExitCode))

	require.NotNil(t, job.Spec.PodFailurePolicy)
	require.Len(t, job.Spec.PodFailurePolicy.Rules, 2)
	stalled := job.Spec.PodFailurePolicy.Rules[0]
	assert.Equal(t, batchv1.PodFailurePolicyActionFailJob, stalled.Action, "a stall fails the Job, no retry")
	require.NotNil(t, stalled.OnExitCodes)
	assert.Equal(t, c.Name, *stalled.OnExitCodes.ContainerName)
	assert.Equal(t, batchv1.PodFailurePolicyOnExitCodesOpIn, stalled.OnExitCodes.Operator)
	assert.Equal(t, []int32{stalledExitCode}, stalled.OnExitCodes.Values)
	disrupted := job.Spec.PodFailurePolicy.Rules[1]
	assert.Equal(t, batchv1.PodFailurePolicyActionIgnore, disrupted.Action, "a disrupted pod is not a failed attempt")
	require.Len(t, disrupted.OnPodConditions, 1)
	assert.Equal(t, corev1.DisruptionTarget, disrupted.OnPodConditions[0].Type)
	assert.EqualValues(t, 2, *job.Spec.BackoffLimit, "other failures keep their retries")

	stall := "DOWNLOAD STALLED: no bytes written to /cache/tiny for 420s (100 bytes on disk, HF_HUB_DISABLE_XET=1 HF_HUB_ENABLE_HF_TRANSFER=1); partial files stay for a retry"
	assert.Equal(t, stall, failureDetail("PROGRESS 100\nPROGRESS 100\n"+stall+"\n"), "the stall line is the reason")
	assert.Equal(t, "a | b", failureDetail("a\nb\n"), "else the tail")
}

// assertRestrictedPodSecurity checks a pod spec against the restricted Pod
// Security Standard's rules on the security context, the way the Kyverno
// policies read them: a pod-level field covers every container that does not
// set its own.
func assertRestrictedPodSecurity(t *testing.T, spec corev1.PodSpec) {
	t.Helper()
	require.NotNil(t, spec.SecurityContext, "pod-level securityContext")
	podNonRoot := spec.SecurityContext.RunAsNonRoot != nil && *spec.SecurityContext.RunAsNonRoot
	podSeccomp := spec.SecurityContext.SeccompProfile != nil && spec.SecurityContext.SeccompProfile.Type == corev1.SeccompProfileTypeRuntimeDefault
	if spec.SecurityContext.RunAsUser != nil {
		assert.Greater(t, *spec.SecurityContext.RunAsUser, int64(0), "pod runAsUser must be > 0")
	}
	for _, c := range append(spec.InitContainers, spec.Containers...) {
		sc := c.SecurityContext
		require.NotNil(t, sc, "%s: securityContext", c.Name)
		if sc.RunAsNonRoot != nil {
			assert.True(t, *sc.RunAsNonRoot, "%s: runAsNonRoot must be true when set", c.Name)
		} else {
			assert.True(t, podNonRoot, "%s: runAsNonRoot neither on the container nor on the pod", c.Name)
		}
		if sc.RunAsUser != nil {
			assert.Greater(t, *sc.RunAsUser, int64(0), "%s: runAsUser must be > 0", c.Name)
		}
		require.NotNil(t, sc.AllowPrivilegeEscalation, "%s: allowPrivilegeEscalation", c.Name)
		assert.False(t, *sc.AllowPrivilegeEscalation, "%s: allowPrivilegeEscalation must be false", c.Name)
		require.NotNil(t, sc.Capabilities, "%s: capabilities", c.Name)
		assert.Contains(t, sc.Capabilities.Drop, corev1.Capability("ALL"), "%s: capabilities must drop ALL", c.Name)
		assert.Empty(t, sc.Capabilities.Add, "%s: no capability may be added", c.Name)
		if sc.SeccompProfile != nil {
			assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type, "%s: seccompProfile.type", c.Name)
		} else {
			assert.True(t, podSeccomp, "%s: seccompProfile neither on the container nor on the pod", c.Name)
		}
	}
}

// The init container leaves a directory that exists alone (mkdir -m, no chmod
// of someone else's directory) and names the fsGroup when the root is not
// writable.
func TestCacheInitScript(t *testing.T) {
	script := cacheInitScript("/cache/tiny", "/cache/.model-manager")
	assert.Contains(t, script, `for d in "/cache/tiny" "/cache/.model-manager"; do`)
	assert.Contains(t, script, `mkdir -m 0777 -p "$d" && [ -w "$d" ] ||`)
	assert.NotContains(t, script, "chmod")
	assert.Contains(t, script, "did not apply the pod's fsGroup")
}

func TestIsAcceleratorAndEligibility(t *testing.T) {
	// The configured GPU resource (capacity or allocatable) or a
	// gpu-feature-discovery label makes an accelerator node; nothing else does.
	assert.False(t, isAccelerator(node("cpu", "8Gi", nil), DefaultGPUResourceName))
	assert.True(t, isAccelerator(withGPUs(node("res", "8Gi", nil), 1), DefaultGPUResourceName))
	amd := node("amd", "8Gi", nil)
	amd.Status.Capacity = corev1.ResourceList{"amd.com/gpu": resource.MustParse("2")}
	assert.False(t, isAccelerator(amd, DefaultGPUResourceName), "the configured resource decides")
	assert.True(t, isAccelerator(amd, "amd.com/gpu"))
	assert.EqualValues(t, 2, budgetOf(amd, "amd.com/gpu", budgetSourceAuto).GPUCount, "capacity counts when allocatable is absent")
	for _, labels := range []map[string]string{{labelGPUPresent: "true"}, {labelGPUCount: "2"}, {labelGPUProduct: "NVIDIA-GB10-SHARED"}} {
		assert.True(t, isAccelerator(node("labelled", "8Gi", labels), DefaultGPUResourceName), labels)
	}
	assert.False(t, isAccelerator(node("off", "8Gi", map[string]string{labelGPUPresent: "false", labelGPUCount: "0"}), DefaultGPUResourceName))

	// Eligibility: every failing rule contributes one reason, in a fixed order.
	pinned := cacheLocation{Claim: "hf-cache", Nodes: []string{"a"}, Bound: true}
	strict := settings{NodeSelector: map[string]string{labelHostname: "a"}, CacheEnabled: true, CacheRedirectPolicy: true}
	a := nodeBudget{Name: "a", Ready: true, Labels: map[string]string{labelHostname: "a"}}
	b := nodeBudget{Name: "b", Ready: false, Labels: map[string]string{labelHostname: "b"}}
	ok, why := eligibility(a, strict, pinned)
	assert.True(t, ok)
	assert.Empty(t, why)
	ok, why = eligibility(b, strict, pinned)
	assert.False(t, ok)
	assert.Equal(t, "not ready; outside the serving node selector (kubernetes.io/hostname=a); cache claim hf-cache is pinned to a", why)

	// A shared, unbound, missing or disabled cache — or predictors that do not
	// mount the claim (redirect policy off) — never disqualify a node.
	open := settings{CacheEnabled: true, CacheRedirectPolicy: true}
	ready := nodeBudget{Name: "b", Ready: true, Labels: map[string]string{labelHostname: "b"}}
	for _, loc := range []cacheLocation{{Claim: "hf-cache", Shared: true, Bound: true}, {Claim: "hf-cache"}, {Claim: "hf-cache", Missing: true}} {
		ok, why = eligibility(ready, open, loc)
		assert.True(t, ok, why)
	}
	ok, _ = eligibility(ready, open, pinned)
	assert.False(t, ok, "pinned elsewhere, predictors mount it")
	ok, _ = eligibility(ready, settings{CacheEnabled: true}, pinned)
	assert.True(t, ok, "redirect policy off: predictors download themselves")
	ok, _ = eligibility(ready, settings{CacheRedirectPolicy: true}, pinned)
	assert.True(t, ok, "cache disabled")

	assert.Equal(t, "a=1, b=2", formatSelector(map[string]string{"b": "2", "a": "1"}))
	assert.Equal(t, "", formatSelector(nil))
}
