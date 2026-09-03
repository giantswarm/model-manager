package kserve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The cache index remembers which repository filled which cache directory.
// The KServe storage-initializer fills <claim>/<InferenceService name> from
// the InferenceService's hf:// storageUri, but leaves nothing behind that says
// so: once the InferenceService is deleted the directory is just a name. While
// an InferenceService exists the driver therefore records the pair — name,
// repository, revision, preset label — in a ConfigMap of the serving namespace
// (DefaultCacheIndexConfigMap, chart value kserve.cache.indexConfigMap), one
// JSON entry per directory. The inventory reads it like a pre-warm marker, so
// the directory keeps its repository, and with it its preset, after the
// InferenceService is gone. A record is dropped when model-manager removes the
// directory. Pre-warm downloads need no record: their marker on the claim says
// the same.

// indexEntry is what the index remembers about one directory.
type indexEntry struct {
	Model            string    `json:"model"`
	Revision         string    `json:"revision,omitempty"`
	Dir              string    `json:"dir"`
	Preset           string    `json:"preset,omitempty"`
	InferenceService string    `json:"inferenceService,omitempty"`
	RecordedAt       time.Time `json:"recordedAt"`
}

// same reports whether two entries carry the same facts (RecordedAt aside).
func (e indexEntry) same(o indexEntry) bool {
	return e.Model == o.Model && e.Revision == o.Revision && e.Dir == o.Dir && e.Preset == o.Preset && e.InferenceService == o.InferenceService
}

// cacheIndex is the driver's copy of the index ConfigMap.
type cacheIndex struct {
	mu       sync.Mutex
	entries  map[string]indexEntry
	loadedAt time.Time
	warnedAt time.Time
}

func (c *cacheIndex) set(entries map[string]indexEntry) {
	c.mu.Lock()
	c.entries = entries
	c.loadedAt = time.Now()
	c.mu.Unlock()
}

// indexEntryFor derives the record an InferenceService stands for: its name is
// the cache directory the storage-initializer fills, its hf:// storageUri the
// repository. Anything else (pvc://, s3://, no URI) records nothing. The
// preset is remembered only when the object says so (the preset label) — a
// preset inferred from the name is derived again at read time.
func indexEntryFor(sv served) (indexEntry, bool) {
	if !strings.HasPrefix(sv.StorageURI, "hf://") {
		return indexEntry{}, false
	}
	repo, revision := splitRevision(sv.StorageURI)
	if !isRepoID(repo) {
		return indexEntry{}, false
	}
	e := indexEntry{Model: repo, Revision: revision, Dir: sv.Name, InferenceService: sv.Name}
	if sv.PresetLabelled {
		e.Preset = sv.Preset
	}
	return e, true
}

// decodeIndex reads the ConfigMap data (dir -> JSON entry); unreadable
// entries are dropped.
func decodeIndex(data map[string]string) map[string]indexEntry {
	out := make(map[string]indexEntry, len(data))
	for dir, raw := range data {
		var e indexEntry
		if err := json.Unmarshal([]byte(raw), &e); err != nil || e.Model == "" {
			continue
		}
		if e.Dir == "" {
			e.Dir = dir
		}
		out[dir] = e
	}
	return out
}

func encodeIndex(entries map[string]indexEntry) map[string]string {
	out := make(map[string]string, len(entries))
	for dir, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			continue
		}
		out[dir] = string(raw)
	}
	return out
}

// readIndex returns the stored index, re-read from the ConfigMap when the
// copy is older than the inventory TTL. A missing ConfigMap is an empty index;
// an unreadable one keeps the previous copy. Callers must not modify the map.
func (b *Backend) readIndex(ctx context.Context) map[string]indexEntry {
	b.index.mu.Lock()
	if b.index.entries != nil && time.Since(b.index.loadedAt) < b.opts.InventoryTTL {
		out := b.index.entries
		b.index.mu.Unlock()
		return out
	}
	b.index.mu.Unlock()
	s := b.cfg.settings(ctx)
	cm, err := b.cs.CoreV1().ConfigMaps(s.Namespace).Get(ctx, b.opts.CacheIndexConfigMap, metav1.GetOptions{})
	var entries map[string]indexEntry
	switch {
	case err == nil:
		entries = decodeIndex(cm.Data)
	case errors.IsNotFound(err):
		entries = map[string]indexEntry{}
	default:
		b.warnIndex("reading the cache index failed", s.Namespace, err)
		b.index.mu.Lock()
		defer b.index.mu.Unlock()
		if b.index.entries != nil {
			return b.index.entries
		}
		return map[string]indexEntry{}
	}
	b.index.set(entries)
	return entries
}

// cacheIndexWith is the index as the inventory uses it: what the ConfigMap
// remembers, overlaid with what the InferenceServices of this moment say. A
// live InferenceService always wins over a stale record, and a record the
// driver could not persist still names its directory for as long as the
// InferenceService exists.
func (b *Backend) cacheIndexWith(ctx context.Context, servedList []served) map[string]indexEntry {
	stored := b.readIndex(ctx)
	out := make(map[string]indexEntry, len(stored)+len(servedList))
	for dir, e := range stored {
		out[dir] = e
	}
	for _, sv := range servedList {
		if e, ok := indexEntryFor(sv); ok {
			out[sv.Name] = e
		}
	}
	return out
}

// recordServed remembers every InferenceService that serves a Hugging Face
// repository from its cache directory. Nothing is written while the index
// already says so.
func (b *Backend) recordServed(ctx context.Context, servedList []served) {
	want := make([]indexEntry, 0, len(servedList))
	for _, sv := range servedList {
		if e, ok := indexEntryFor(sv); ok {
			want = append(want, e)
		}
	}
	if len(want) == 0 {
		return
	}
	stored := b.readIndex(ctx)
	dirty := false
	for _, e := range want {
		if cur, ok := stored[e.Dir]; !ok || !cur.same(e) {
			dirty = true
			break
		}
	}
	if !dirty {
		return
	}
	var recorded []string
	err := b.updateIndex(ctx, func(entries map[string]indexEntry) bool {
		recorded = recorded[:0]
		for _, e := range want {
			if cur, ok := entries[e.Dir]; ok && cur.same(e) {
				continue
			}
			e.RecordedAt = time.Now().UTC()
			entries[e.Dir] = e
			recorded = append(recorded, e.Dir+"="+e.Model)
		}
		return len(recorded) > 0
	})
	if err != nil {
		b.warnIndex("recording the cache index failed", b.cfg.settings(ctx).Namespace, err)
		return
	}
	if len(recorded) > 0 {
		b.log.Info("cache index recorded", "configMap", b.opts.CacheIndexConfigMap, "directories", strings.Join(recorded, ","))
	}
}

// forgetDir drops a directory from the index once it is removed from the
// cache.
func (b *Backend) forgetDir(ctx context.Context, dir string) {
	err := b.updateIndex(ctx, func(entries map[string]indexEntry) bool {
		if _, ok := entries[dir]; !ok {
			return false
		}
		delete(entries, dir)
		return true
	})
	if err != nil {
		b.warnIndex("forgetting a cache index entry failed", b.cfg.settings(ctx).Namespace, err)
	}
}

// updateIndex applies mutate to the ConfigMap's current content and writes it
// back, creating the ConfigMap on first use; a concurrent write is retried on
// the re-read content. mutate returns false when nothing changed.
func (b *Backend) updateIndex(ctx context.Context, mutate func(map[string]indexEntry) bool) error {
	s := b.cfg.settings(ctx)
	name := b.opts.CacheIndexConfigMap
	cms := b.cs.CoreV1().ConfigMaps(s.Namespace)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cm, err := cms.Get(ctx, name, metav1.GetOptions{})
		create := false
		switch {
		case errors.IsNotFound(err):
			create = true
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: s.Namespace,
				Labels:    b.labels(map[string]string{ComponentLabel: componentCacheIndex}),
			}}
		case err != nil:
			return fmt.Errorf("get ConfigMap %s/%s: %w", s.Namespace, name, err)
		}
		entries := decodeIndex(cm.Data)
		if !mutate(entries) || (create && len(entries) == 0) {
			b.index.set(entries)
			return nil
		}
		cm.Data = encodeIndex(entries)
		if create {
			_, err = cms.Create(ctx, cm, metav1.CreateOptions{FieldManager: ManagedByValue})
		} else {
			_, err = cms.Update(ctx, cm, metav1.UpdateOptions{FieldManager: ManagedByValue})
		}
		if err == nil {
			b.index.set(entries)
			return nil
		}
		if errors.IsConflict(err) || errors.IsAlreadyExists(err) {
			lastErr = err
			continue
		}
		return fmt.Errorf("write ConfigMap %s/%s: %w", s.Namespace, name, err)
	}
	return fmt.Errorf("write ConfigMap %s/%s: %w", s.Namespace, name, lastErr)
}

// warnIndex logs an index failure at most every few minutes: the inventory
// works without the index, so a missing permission must not flood the log.
func (b *Backend) warnIndex(msg, namespace string, err error) {
	b.index.mu.Lock()
	recent := time.Since(b.index.warnedAt) < 5*time.Minute
	if !recent {
		b.index.warnedAt = time.Now()
	}
	b.index.mu.Unlock()
	if recent {
		return
	}
	args := []any{"configMap", namespace + "/" + b.opts.CacheIndexConfigMap, "error", err}
	if errors.IsForbidden(err) {
		args = append(args, "hint", "the chart's kserve Role grants create on ConfigMaps and update/patch on kserve.cache.indexConfigMap; upgrade the chart or grant them")
	}
	b.log.Warn(msg, args...)
}
