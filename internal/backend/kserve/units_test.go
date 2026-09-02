package kserve

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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
		"DIR\ttiny\t453864\t2\t1700000000",
		"DIR\tqwen3-14b\t0\t0\t1700000100",
		"MARKER\ttiny\t{\"model\":\"org/tiny\",\"dir\":\"tiny\",\"bytesExpected\":453864}",
		"junk line",
		"END",
	}, "\n")
	entries, err := parseScan("n1", strings.NewReader(out))
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "qwen3-14b", entries[0].Dir)
	assert.Nil(t, entries[0].Marker)
	assert.Equal(t, "tiny", entries[1].Dir)
	assert.EqualValues(t, 453864, entries[1].Bytes)
	assert.Equal(t, 2, entries[1].Files)
	assert.Equal(t, "org/tiny", entries[1].Marker.Model)
	assert.Equal(t, time.Unix(1700000000, 0).UTC(), entries[1].MTime)

	_, err = parseScan("n1", strings.NewReader("DIR\tx\t1\t1\t1\n"))
	assert.ErrorContains(t, err, "truncated")

	// The real script against a temporary cache root.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "tiny", "sub"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "tiny", "config.json"), make([]byte, 100), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "tiny", "sub", "model.safetensors"), make([]byte, 2000), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "empty"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, markersDir), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, markersDir, "tiny.json"), []byte(`{"model":"org/tiny","dir":"tiny"}`+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "stray-file"), []byte("x"), 0o600))
	cmd := exec.Command("sh", "-c", scanScript)
	cmd.Env = append(os.Environ(), "MM_CACHE_ROOT="+root)
	raw, err := cmd.CombinedOutput()
	require.NoError(t, err, string(raw))
	entries, err = parseScan("n1", strings.NewReader(string(raw)))
	require.NoError(t, err, string(raw))
	require.Len(t, entries, 2, string(raw))
	assert.Equal(t, "empty", entries[0].Dir)
	assert.EqualValues(t, 0, entries[0].Bytes)
	assert.Equal(t, "tiny", entries[1].Dir)
	assert.EqualValues(t, 2100, entries[1].Bytes)
	assert.Equal(t, 2, entries[1].Files)
	require.NotNil(t, entries[1].Marker)
	assert.Equal(t, "org/tiny", entries[1].Marker.Model)
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
	s, msg, ready := servedStatus(obj(map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}))
	assert.Equal(t, statusReady, s)
	assert.True(t, ready)
	assert.Empty(t, msg)

	s, msg, ready = servedStatus(obj(map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "PredictorNotReady", "message": "waiting"}}}))
	assert.Equal(t, statusNotReady, s)
	assert.False(t, ready)
	assert.Equal(t, "PredictorNotReady waiting", msg)

	s, msg, _ = servedStatus(obj(map[string]any{"modelStatus": map[string]any{"transitionStatus": "BlockedByFailedLoad", "lastFailureInfo": map[string]any{"reason": "ModelLoadFailed", "message": "OOM"}}}))
	assert.Equal(t, statusNotReady, s)
	assert.Equal(t, "ModelLoadFailed OOM", msg)

	s, _, _ = servedStatus(obj(map[string]any{}))
	assert.Equal(t, statusPending, s)
	s, _, _ = servedStatus(obj(map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "Unknown", "reason": "Scheduling"}}}))
	assert.Equal(t, statusPending, s)
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
// capabilities; the serving namespace commonly enforces that profile.
var pssBaselineCapabilities = map[corev1.Capability]bool{
	"AUDIT_WRITE": true, "CHOWN": true, "DAC_OVERRIDE": true, "FOWNER": true, "FSETID": true,
	"KILL": true, "MKNOD": true, "NET_BIND_SERVICE": true, "SETFCAP": true, "SETGID": true,
	"SETPCAP": true, "SETUID": true, "SYS_CHROOT": true,
}

func TestCachePodStaysWithinPodSecurityBaseline(t *testing.T) {
	b := &Backend{opts: backend.KServeOptions{InitImage: "alpine:3"}}
	pod := b.cachePod("scan", settings{Namespace: "serving"}, "node-a", "true", true)
	for _, c := range pod.Spec.Containers {
		require.NotNil(t, c.SecurityContext)
		require.NotNil(t, c.SecurityContext.Capabilities)
		assert.Equal(t, []corev1.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop)
		for _, cap := range c.SecurityContext.Capabilities.Add {
			assert.True(t, pssBaselineCapabilities[cap], "capability %s is not allowed by the baseline profile", cap)
		}
		assert.False(t, *c.SecurityContext.AllowPrivilegeEscalation)
	}
}
