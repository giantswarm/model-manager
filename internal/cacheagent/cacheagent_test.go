package cacheagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "tiny", "sub"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "tiny", "config.json"), make([]byte, 100), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "tiny", "sub", "model.safetensors"), make([]byte, 2000), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(root, "tiny", "config.json"), filepath.Join(root, "tiny", "link.json")), "symlinks are not files")
	// A Mistral-style repository: consolidated weights and params.json, no config.json.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "mistral"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "mistral", "consolidated-00001-of-00002.safetensors"), make([]byte, 300), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "mistral", "params.json"), make([]byte, 30), 0o600))
	// Hugging Face client internals living on the same claim.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "hf-home", "hub", "models--org--tiny", "refs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "hf-home", "hub", "models--org--tiny", "refs", "main"), []byte("abc"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "hf-home", "CACHEDIR.TAG"), []byte("Signature: 8a477f597d28d172789f06886806bc55"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "empty"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".hidden"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, MarkersDir), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, MarkersDir, "tiny.json"), []byte(`{"model":"org/tiny","revision":"abc"}`+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, MarkersDir, "broken.json"), []byte(`{`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, MarkersDir, "notes.txt"), []byte(`x`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "stray-file"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(root, "tiny"), filepath.Join(root, "alias")), "a symlinked directory counts, as in the shell scan")
	return root
}

func TestIsModelFile(t *testing.T) {
	for _, name := range []string{"config.json", "model.safetensors", "model-00001-of-00002.safetensors", "consolidated.safetensors", "model.gguf", "pytorch_model.bin", "model.pt", "model.pth", "model.onnx"} {
		assert.True(t, IsModelFile(name), name)
	}
	for _, name := range []string{"README.md", "params.json", "generation_config.json", "tokenizer.json", "CACHEDIR.TAG", "model.safetensors.index.json", "chunk-cache"} {
		assert.False(t, IsModelFile(name), name)
	}
}

func TestScan(t *testing.T) {
	root := fixtureRoot(t)
	inv, err := Scan(root)
	require.NoError(t, err)
	assert.Equal(t, root, inv.Root)
	assert.False(t, inv.ScannedAt.IsZero())
	require.Len(t, inv.Entries, 5, inv.Entries)
	byDir := map[string]Entry{}
	for _, e := range inv.Entries {
		byDir[e.Dir] = e
		require.NotNil(t, e.HasModel, "every entry carries the verdict: %s", e.Dir)
	}
	assert.Equal(t, []string{"alias", "empty", "hf-home", "mistral", "tiny"}, func() []string {
		out := make([]string, 0, len(inv.Entries))
		for _, e := range inv.Entries {
			out = append(out, e.Dir)
		}
		return out
	}(), "sorted by name")

	alias := byDir["alias"]
	assert.EqualValues(t, 0, alias.Bytes, "a symlinked directory is listed but not followed — `find` in the scan pod does not either")
	assert.Equal(t, 0, alias.Files)
	assert.False(t, *alias.HasModel)
	empty := byDir["empty"]
	assert.EqualValues(t, 0, empty.Bytes)
	assert.Equal(t, 0, empty.Files)
	assert.Nil(t, empty.Marker)
	assert.False(t, *empty.HasModel)
	tiny := byDir["tiny"]
	assert.EqualValues(t, 2100, tiny.Bytes)
	assert.Equal(t, 2, tiny.Files)
	assert.False(t, tiny.MTime.IsZero())
	assert.True(t, *tiny.HasModel, "config.json at the top level")
	require.NotNil(t, tiny.Marker)
	assert.Equal(t, "org/tiny", tiny.Marker.Model)
	assert.Equal(t, "abc", tiny.Marker.Revision)
	assert.Equal(t, "tiny", tiny.Marker.Dir, "the file stem fills a missing dir")
	mistral := byDir["mistral"]
	assert.True(t, *mistral.HasModel, "weights at the top level count without a config.json")
	assert.EqualValues(t, 330, mistral.Bytes)
	hfHome := byDir["hf-home"]
	assert.False(t, *hfHome.HasModel, "the hub cache is not a model")
	assert.Equal(t, 2, hfHome.Files, "but its files and bytes are counted")
	require.Len(t, inv.Warnings, 1, inv.Warnings)
	assert.Contains(t, inv.Warnings[0], "broken.json")

	// The verdict is on the wire as hasModel.
	raw, err := json.Marshal(tiny)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"hasModel":true`)
	raw, err = json.Marshal(hfHome)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"hasModel":false`)
	var legacy Entry
	require.NoError(t, json.Unmarshal([]byte(`{"dir":"x","bytes":1,"files":1}`), &legacy))
	assert.Nil(t, legacy.HasModel, "an agent that predates the field says nothing")

	_, err = Scan(filepath.Join(root, "nope"))
	assert.ErrorContains(t, err, "read cache root")

	// No markers directory is fine.
	bare := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(bare, "x"), 0o750))
	inv, err = Scan(bare)
	require.NoError(t, err)
	assert.Empty(t, inv.Warnings)
	require.Len(t, inv.Entries, 1)
}

func TestHandler(t *testing.T) {
	root := fixtureRoot(t)
	srv := httptest.NewServer(Handler(root, "n1", nil))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + HealthPath)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	resp, err = http.Get(srv.URL + InventoryPath)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	var inv Inventory
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&inv))
	assert.Equal(t, "n1", inv.Node)
	assert.Len(t, inv.Entries, 5)

	resp, err = http.Post(srv.URL+InventoryPath, "text/plain", nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)

	broken := httptest.NewServer(Handler(filepath.Join(root, "nope"), "n1", nil))
	t.Cleanup(broken.Close)
	resp, err = http.Get(broken.URL + InventoryPath)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}
