package kserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/model-manager/internal/backend"
)

// hubClient is a minimal Hugging Face Hub API client: search, repository
// info, file tree and the safetensors index.
type hubClient struct {
	base  string
	http  *http.Client
	token func(ctx context.Context) string
}

// hubError is a non-2xx hub answer.
type hubError struct {
	Status int
	Body   string
}

func (e *hubError) Error() string {
	return fmt.Sprintf("hugging face hub: HTTP %d: %s", e.Status, strings.TrimSpace(e.Body))
}

func newHubClient(base string, hc *http.Client, token func(ctx context.Context) string) *hubClient {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if token == nil {
		token = func(context.Context) string { return "" }
	}
	return &hubClient{base: strings.TrimRight(base, "/"), http: hc, token: token}
}

// hubModel is the subset of GET /api/models/{repo} the driver reads.
type hubModel struct {
	ID           string    `json:"id"`
	Author       string    `json:"author"`
	SHA          string    `json:"sha"`
	Downloads    int64     `json:"downloads"`
	Likes        int64     `json:"likes"`
	Gated        any       `json:"gated"` // false | "auto" | "manual"
	Private      bool      `json:"private"`
	PipelineTag  string    `json:"pipeline_tag"`
	LibraryName  string    `json:"library_name"`
	Tags         []string  `json:"tags"`
	LastModified time.Time `json:"lastModified"`
	Siblings     []struct {
		Filename string `json:"rfilename"`
	} `json:"siblings"`
	Safetensors *struct {
		Total int64 `json:"total"`
	} `json:"safetensors"`
}

func (m *hubModel) isGated() bool {
	switch v := m.Gated.(type) {
	case bool:
		return v
	case string:
		return v != "" && v != "false"
	}
	return false
}

// hubFile is one entry of GET /api/models/{repo}/tree/{revision}.
type hubFile struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		Size int64 `json:"size"`
	} `json:"lfs"`
}

func (f hubFile) bytes() int64 {
	if f.LFS != nil && f.LFS.Size > 0 {
		return f.LFS.Size
	}
	return f.Size
}

// Search proxies GET /api/models?search=.
func (c *hubClient) Search(ctx context.Context, query string, limit int) ([]backend.SearchResult, error) {
	q := url.Values{}
	q.Set("search", query)
	q.Set("limit", strconv.Itoa(limit))
	q.Set("sort", "downloads")
	q.Set("direction", "-1")
	var hits []hubModel
	if err := c.getJSON(ctx, "/api/models?"+q.Encode(), &hits); err != nil {
		return nil, err
	}
	out := make([]backend.SearchResult, 0, len(hits))
	for _, h := range hits {
		out = append(out, backend.SearchResult{
			ID:           h.ID,
			Author:       h.Author,
			Downloads:    h.Downloads,
			Likes:        h.Likes,
			Gated:        h.isGated(),
			Private:      h.Private,
			PipelineTag:  h.PipelineTag,
			Library:      h.LibraryName,
			Tags:         h.Tags,
			LastModified: h.LastModified,
		})
	}
	return out, nil
}

// Model fetches repository metadata; a 404 maps to backend.ErrNotFound and a
// 401/403 (gated or private without a valid token) to backend.ErrInvalid.
func (c *hubClient) Model(ctx context.Context, repo string) (*hubModel, error) {
	var m hubModel
	if err := c.getJSON(ctx, "/api/models/"+escapeRepo(repo), &m); err != nil {
		return nil, mapHubErr(err, repo)
	}
	return &m, nil
}

// Tree lists every file of the repository at revision (default main).
func (c *hubClient) Tree(ctx context.Context, repo, revision string) ([]hubFile, error) {
	if revision == "" {
		revision = "main"
	}
	var files []hubFile
	if err := c.getJSON(ctx, "/api/models/"+escapeRepo(repo)+"/tree/"+url.PathEscape(revision)+"?recursive=true", &files); err != nil {
		return nil, mapHubErr(err, repo)
	}
	out := files[:0]
	for _, f := range files {
		if f.Type == "file" {
			out = append(out, f)
		}
	}
	return out, nil
}

// safetensorsIndex is what model.safetensors.index.json says about a
// checkpoint: the total_size its metadata declares and the distinct shard
// files its weight_map names.
type safetensorsIndex struct {
	TotalSize int64
	Shards    []string
}

// shardBytes sums the shards the index names, from the file tree; a shard the
// tree does not hold counts nothing.
func (idx *safetensorsIndex) shardBytes(files []hubFile) int64 {
	size := make(map[string]int64, len(files))
	for _, f := range files {
		size[f.Path] = f.bytes()
	}
	var total int64
	for _, s := range idx.Shards {
		total += size[s]
	}
	return total
}

// SafetensorsIndex reads model.safetensors.index.json; nil and no error when
// the repository has no index.
func (c *hubClient) SafetensorsIndex(ctx context.Context, repo, revision string, files []hubFile) (*safetensorsIndex, error) {
	const index = "model.safetensors.index.json"
	found := false
	for _, f := range files {
		if f.Path == index {
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}
	if revision == "" {
		revision = "main"
	}
	var doc struct {
		Metadata struct {
			TotalSize int64 `json:"total_size"`
		} `json:"metadata"`
		WeightMap map[string]string `json:"weight_map"`
	}
	if err := c.getJSON(ctx, "/"+escapeRepo(repo)+"/resolve/"+url.PathEscape(revision)+"/"+index, &doc); err != nil {
		return nil, mapHubErr(err, repo)
	}
	named := make(map[string]struct{})
	for _, shard := range doc.WeightMap {
		named[shard] = struct{}{}
	}
	shards := make([]string, 0, len(named))
	for shard := range named {
		shards = append(shards, shard)
	}
	slices.Sort(shards)
	return &safetensorsIndex{TotalSize: doc.Metadata.TotalSize, Shards: shards}, nil
}

// weightsFromIndex sizes a checkpoint from its safetensors index: the declared
// total_size when it agrees within one percent with the sum of the shards the
// weight_map names, else that sum — quantized repacks keep the original BF16
// total in the index while their shards hold half of it. Summing every
// .safetensors file of the tree would be wrong in turn: Mistral repositories
// carry a second, consolidated copy the index does not name. An index that
// names no shard the tree holds cannot be checked and its total stands; no
// index sizes nothing.
func weightsFromIndex(idx *safetensorsIndex, files []hubFile) (int64, string) {
	if idx == nil {
		return 0, ""
	}
	shards := idx.shardBytes(files)
	if shards == 0 || withinOnePercent(idx.TotalSize, shards) {
		return idx.TotalSize, weightsSourceIndex
	}
	return shards, weightsSourceShards
}

// withinOnePercent says whether a differs from b by at most one percent of b.
func withinOnePercent(a, b int64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d*100 <= b
}

// weightsFromTree sums the weight files: safetensors when present, else the
// other checkpoint formats.
func weightsFromTree(files []hubFile) int64 {
	var safetensors, other int64
	for _, f := range files {
		switch strings.ToLower(path.Ext(f.Path)) {
		case ".safetensors":
			safetensors += f.bytes()
		case ".bin", ".pt", ".pth", ".gguf", ".ckpt", ".msgpack", ".h5", ".onnx":
			other += f.bytes()
		}
	}
	if safetensors > 0 {
		return safetensors
	}
	return other
}

// downloadTotal sums every file a snapshot download fetches, minus the ignore
// patterns (fnmatch-style, as huggingface_hub applies them).
func downloadTotal(files []hubFile, ignore []string) int64 {
	var total int64
	for _, f := range files {
		if matchesAny(f.Path, ignore) {
			continue
		}
		total += f.bytes()
	}
	return total
}

func matchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if ok, _ := path.Match(p, name); ok {
			return true
		}
		if ok, _ := path.Match(p, path.Base(name)); ok {
			return true
		}
	}
	return false
}

func (c *hubClient) getJSON(ctx context.Context, p string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+p, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "model-manager")
	if tok := c.token(ctx); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hugging face hub: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("hugging face hub: read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &hubError{Status: resp.StatusCode, Body: string(body)}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("hugging face hub: decode %s: %w", p, err)
	}
	return nil
}

func mapHubErr(err error, repo string) error {
	var he *hubError
	if errors.As(err, &he) {
		switch he.Status {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s is not on the hub", backend.ErrNotFound, repo)
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: %s is gated or private; configure a hub token with access (HTTP %d)", backend.ErrInvalid, repo, he.Status)
		}
	}
	return err
}

func escapeRepo(repo string) string {
	parts := strings.SplitN(repo, "/", 2)
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// ModelConfig reads the checkpoint's config.json; nil and no error when the
// repository has none (a Mistral-format checkpoint ships params.json).
func (c *hubClient) ModelConfig(ctx context.Context, repo, revision string, files []hubFile) (json.RawMessage, error) {
	const config = "config.json"
	if !slices.ContainsFunc(files, func(f hubFile) bool { return f.Path == config }) {
		return nil, nil
	}
	if revision == "" {
		revision = "main"
	}
	var doc json.RawMessage
	if err := c.getJSON(ctx, "/"+escapeRepo(repo)+"/resolve/"+url.PathEscape(revision)+"/"+config, &doc); err != nil {
		return nil, mapHubErr(err, repo)
	}
	return doc, nil
}
