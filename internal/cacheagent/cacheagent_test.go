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

func TestScan(t *testing.T) {
	root := fixtureRoot(t)
	inv, err := Scan(root)
	require.NoError(t, err)
	assert.Equal(t, root, inv.Root)
	assert.False(t, inv.ScannedAt.IsZero())
	require.Len(t, inv.Entries, 3, inv.Entries)
	assert.Equal(t, "alias", inv.Entries[0].Dir, "a symlinked directory is listed")
	assert.EqualValues(t, 0, inv.Entries[0].Bytes, "but not followed — `find` in the scan pod does not either")
	assert.Equal(t, 0, inv.Entries[0].Files)
	assert.Equal(t, "empty", inv.Entries[1].Dir)
	assert.EqualValues(t, 0, inv.Entries[1].Bytes)
	assert.Equal(t, 0, inv.Entries[1].Files)
	assert.Nil(t, inv.Entries[1].Marker)
	tiny := inv.Entries[2]
	assert.Equal(t, "tiny", tiny.Dir)
	assert.EqualValues(t, 2100, tiny.Bytes)
	assert.Equal(t, 2, tiny.Files)
	assert.False(t, tiny.MTime.IsZero())
	require.NotNil(t, tiny.Marker)
	assert.Equal(t, "org/tiny", tiny.Marker.Model)
	assert.Equal(t, "abc", tiny.Marker.Revision)
	assert.Equal(t, "tiny", tiny.Marker.Dir, "the file stem fills a missing dir")
	require.Len(t, inv.Warnings, 1, inv.Warnings)
	assert.Contains(t, inv.Warnings[0], "broken.json")

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
	assert.Len(t, inv.Entries, 3)

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
