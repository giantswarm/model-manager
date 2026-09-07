package lmstudio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a minimal LM Studio HTTP client covering the management surface
// model-manager proxies, all under /api/v1: the model inventory, the
// download that backs a pull with its status job, and load/unload. LM Studio
// serves these next to the OpenAI-compatible endpoints under /v1.
type Client struct {
	base string
	http *http.Client
	// loadTimeout bounds a load: LM Studio reads the weights before it
	// answers, which outlives the per-call timeout for large models.
	loadTimeout time.Duration
}

// APIError is a non-2xx answer from LM Studio.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("lmstudio: %s (%s, HTTP %d)", e.Message, e.Code, e.Status)
	}
	return fmt.Sprintf("lmstudio: %s (HTTP %d)", e.Message, e.Status)
}

const (
	apiPrefix = "/api/v1"
	// keyModel is the request field naming a model; unload takes the loaded
	// instance instead.
	keyModel      = "model"
	keyInstanceID = "instance_id"

	// The download job states GET /models/download/status/{id} reports.
	statusDownloading       = "downloading"
	statusPaused            = "paused"
	statusCompleted         = "completed"
	statusFailed            = "failed"
	statusAlreadyDownloaded = "already_downloaded"

	// The model types LM Studio reports; only llm serves agents.
	typeLLM = "llm"
)

// NewClient returns a client for the given base URL (e.g. http://127.0.0.1:1234).
// loadTimeout bounds POST /api/v1/models/load (0: 10 minutes).
func NewClient(endpoint string, hc *http.Client, loadTimeout time.Duration) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if loadTimeout <= 0 {
		loadTimeout = 10 * time.Minute
	}
	return &Client{base: strings.TrimRight(endpoint, "/"), http: hc, loadTimeout: loadTimeout}
}

// apiModel is an entry of GET /api/v1/models. Every entry is downloaded —
// LM Studio's inventory is the local library, not a catalog.
type apiModel struct {
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
	Publisher   string `json:"publisher"`
	// Type is llm, embedding or vlm.
	Type         string `json:"type"`
	Architecture string `json:"architecture"`
	Quantization *struct {
		Name          string `json:"name"`
		BitsPerWeight int    `json:"bits_per_weight"`
	} `json:"quantization"`
	SizeBytes    int64  `json:"size_bytes"`
	ParamsString string `json:"params_string"`
	Format       string `json:"format"`
	// MaxContextLength is what the weights allow; a loaded instance reports
	// the context it was actually loaded with.
	MaxContextLength int64 `json:"max_context_length"`
	// LoadedInstances is empty for a model that is downloaded but not
	// resident; one model can have several.
	LoadedInstances []loadedInstance `json:"loaded_instances"`
	// Capabilities is absent for embedding models.
	Capabilities    *modelCapabilities `json:"capabilities"`
	SelectedVariant string             `json:"selected_variant"`
}

// loadedInstance is one resident copy of a model. ID is what unload takes.
type loadedInstance struct {
	ID     string `json:"id"`
	Config struct {
		ContextLength int64 `json:"context_length"`
	} `json:"config"`
}

// modelCapabilities is what LM Studio knows about a model's training.
// TrainedForToolUse is the one agents need: LM Studio will accept `tools`
// for any model, emulating them through the prompt, but only a model trained
// for them calls them reliably.
type modelCapabilities struct {
	Vision            bool            `json:"vision"`
	TrainedForToolUse bool            `json:"trained_for_tool_use"`
	Reasoning         json.RawMessage `json:"reasoning"`
}

// modelsResponse is GET /api/v1/models. Models is a pointer so an absent
// key is distinguishable from an empty library: LM Studio answers HTTP 200
// with an {"error":…} document for every path outside /api/v1, so the
// envelope's shape — not the status code — is what proves this is LM Studio.
type modelsResponse struct {
	Models *[]apiModel `json:"models"`
}

// downloadJob is the answer of POST /models/download and of the status job
// it starts. The counters are float64 because LM Studio reports a fractional
// rate (bytes_per_second: 5248034.973097618) — an int64 field fails the whole
// decode on it, and the byte counts are the same kind of number.
type downloadJob struct {
	JobID           string  `json:"job_id"`
	Status          string  `json:"status"`
	TotalSizeBytes  float64 `json:"total_size_bytes"`
	DownloadedBytes float64 `json:"downloaded_bytes"`
	BytesPerSecond  float64 `json:"bytes_per_second"`
	Error           string  `json:"error"`
}

// loadResponse is the answer of POST /models/load.
type loadResponse struct {
	InstanceID string `json:"instance_id"`
	Status     string `json:"status"`
}

// Models lists the local library.
func (c *Client) Models(ctx context.Context) ([]apiModel, error) {
	var out modelsResponse
	if err := c.do(ctx, c.http, http.MethodGet, "/models", nil, &out); err != nil {
		return nil, err
	}
	if out.Models == nil {
		return nil, &APIError{Status: http.StatusOK, Message: "GET /api/v1/models answered without a models array (not an LM Studio 0.4.0+ API)"}
	}
	return *out.Models, nil
}

// StartDownload asks LM Studio to fetch a model from its hub and returns the
// job that tracks it. quantization is optional; empty lets LM Studio pick the
// variant that fits the host.
func (c *Client) StartDownload(ctx context.Context, ref, quantization string) (*downloadJob, error) {
	body := map[string]any{keyModel: ref}
	if quantization != "" {
		body["quantization"] = quantization
	}
	var out downloadJob
	if err := c.do(ctx, c.http, http.MethodPost, "/models/download", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DownloadStatus reads a download job.
func (c *Client) DownloadStatus(ctx context.Context, jobID string) (*downloadJob, error) {
	var out downloadJob
	// The job id goes in the path; escape it so an id with a slash cannot
	// address another endpoint.
	if err := c.do(ctx, c.http, http.MethodGet, "/models/download/status/"+url.PathEscape(jobID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Load makes a downloaded model resident and returns the instance id.
func (c *Client) Load(ctx context.Context, key string) (string, error) {
	hc := &http.Client{Transport: c.http.Transport, Timeout: c.loadTimeout}
	var out loadResponse
	if err := c.do(ctx, hc, http.MethodPost, "/models/load", map[string]any{keyModel: key}, &out); err != nil {
		return "", err
	}
	return out.InstanceID, nil
}

// Unload evicts one resident instance, addressed by its instance id.
func (c *Client) Unload(ctx context.Context, instanceID string) error {
	return c.do(ctx, c.http, http.MethodPost, "/models/unload", map[string]any{keyInstanceID: instanceID}, nil)
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+apiPrefix+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("lmstudio: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return readAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("lmstudio: %s %s: decode: %w", method, path, err)
	}
	return nil
}

func readAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	apiErr := &APIError{Status: resp.StatusCode}
	apiErr.Code, apiErr.Message = parseErrorBody(raw)
	if apiErr.Message == "" {
		apiErr.Message = strings.TrimSpace(string(raw))
	}
	if apiErr.Message == "" {
		apiErr.Message = http.StatusText(resp.StatusCode)
	}
	return apiErr
}

// parseErrorBody reads the error shapes LM Studio answers with: the nested
// {"error":{"type":…,"message":…,"code":…}} of the /api/v1 endpoints and the
// flat {"error":"message"} of an unrecognised path.
func parseErrorBody(raw []byte) (code, message string) {
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil || len(body.Error) == 0 {
		return "", ""
	}
	var s string
	if json.Unmarshal(body.Error, &s) == nil {
		return "", s
	}
	var obj struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body.Error, &obj) == nil {
		code = obj.Type
		if code == "" {
			code = obj.Code
		}
		return code, obj.Message
	}
	return "", ""
}
