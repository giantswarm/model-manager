// Package cacheagent reads a mounted model cache — one subdirectory per
// InferenceService under the claim root, plus the hidden markers directory a
// pre-warm download writes — and serves the result as JSON. The
// `model-manager cache-agent` subcommand runs it in the DaemonSet the chart
// renders for kserve.inventory.mode=daemonset; the kserve driver reads
// GET /inventory from the agent on a node instead of running a scan pod there.
package cacheagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// InventoryPath serves the scan.
	InventoryPath = "/inventory"
	// HealthPath answers the probes.
	HealthPath = "/healthz"
	// MarkersDir is the hidden directory under the cache root holding one
	// <dir>.json marker per pre-warm download.
	MarkersDir = ".model-manager"
	// DefaultRoot is where the DaemonSet mounts the cache claim.
	DefaultRoot = "/cache"
	// DefaultPort is the agent's listen port.
	DefaultPort = 8081
)

// Marker is what a pre-warm download records about its directory.
type Marker struct {
	Model         string    `json:"model"`
	Revision      string    `json:"revision,omitempty"`
	Dir           string    `json:"dir"`
	BytesExpected int64     `json:"bytesExpected,omitempty"`
	CompletedAt   time.Time `json:"completedAt,omitempty"`
	Job           string    `json:"job,omitempty"`
}

// Entry is one top-level directory of the cache: apparent bytes and count of
// the regular files below it, the directory's mtime, and its marker when a
// pre-warm download wrote one.
type Entry struct {
	Dir    string    `json:"dir"`
	Bytes  int64     `json:"bytes"`
	Files  int       `json:"files"`
	MTime  time.Time `json:"mtime,omitempty"`
	Marker *Marker   `json:"marker,omitempty"`
}

// Inventory is one scan of the cache root.
type Inventory struct {
	// Node is the node the agent runs on (from NODE_NAME).
	Node      string    `json:"node,omitempty"`
	Root      string    `json:"root"`
	ScannedAt time.Time `json:"scannedAt"`
	Entries   []Entry   `json:"entries"`
	// Warnings lists what could not be read completely; the entries are
	// still usable.
	Warnings []string `json:"warnings,omitempty"`
}

// Scan walks the cache root the way the scan pod's shell script does: every
// visible top-level directory is an entry (a symlink to a directory is listed
// but, like `find`, not followed), regular files only, symlinks below the top
// level not followed. Dot-directories are skipped; the markers directory is
// read for markers.
func Scan(root string) (*Inventory, error) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read cache root %s: %w", root, err)
	}
	inv := &Inventory{Root: root, ScannedAt: time.Now().UTC(), Entries: []Entry{}}
	markers := readMarkers(root, inv)
	for _, d := range dirs {
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(root, name)
		info, err := os.Stat(path)
		if err != nil {
			inv.Warnings = append(inv.Warnings, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if !info.IsDir() {
			continue
		}
		e := Entry{Dir: name, MTime: info.ModTime().UTC(), Marker: markers[name]}
		walkErr := filepath.WalkDir(path, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				inv.Warnings = append(inv.Warnings, fmt.Sprintf("%s: %v", p, err))
				return nil
			}
			if !de.Type().IsRegular() {
				return nil
			}
			fi, err := de.Info()
			if err != nil {
				inv.Warnings = append(inv.Warnings, fmt.Sprintf("%s: %v", p, err))
				return nil
			}
			e.Bytes += fi.Size()
			e.Files++
			return nil
		})
		if walkErr != nil {
			inv.Warnings = append(inv.Warnings, fmt.Sprintf("%s: %v", name, walkErr))
		}
		inv.Entries = append(inv.Entries, e)
	}
	sort.Slice(inv.Entries, func(i, j int) bool { return inv.Entries[i].Dir < inv.Entries[j].Dir })
	return inv, nil
}

// readMarkers decodes <root>/.model-manager/*.json, keyed by file stem.
func readMarkers(root string, inv *Inventory) map[string]*Marker {
	out := map[string]*Marker{}
	entries, err := os.ReadDir(filepath.Join(root, MarkersDir))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			inv.Warnings = append(inv.Warnings, fmt.Sprintf("%s: %v", MarkersDir, err))
		}
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, MarkersDir, e.Name())) // #nosec G304 -- <cache root>/.model-manager/<entry>: a directory entry below the mounted cache, not user input
		if err != nil {
			inv.Warnings = append(inv.Warnings, fmt.Sprintf("%s/%s: %v", MarkersDir, e.Name(), err))
			continue
		}
		var m Marker
		if err := json.Unmarshal(raw, &m); err != nil {
			inv.Warnings = append(inv.Warnings, fmt.Sprintf("%s/%s: %v", MarkersDir, e.Name(), err))
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if m.Dir == "" {
			m.Dir = name
		}
		out[name] = &m
	}
	return out
}

// Handler serves Scan(root) at InventoryPath (fresh on every request — the
// driver caches per node) and "ok" at HealthPath.
func Handler(root, node string, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+HealthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET "+InventoryPath, func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		inv, err := Scan(root)
		if err != nil {
			log.Error("cache scan failed", "root", root, "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		inv.Node = node
		log.Debug("cache scanned", "root", root, "entries", len(inv.Entries), "warnings", len(inv.Warnings), "took", time.Since(started), "from", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(inv)
	})
	return mux
}
