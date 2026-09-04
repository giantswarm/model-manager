package lemonade

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a minimal Lemonade Server HTTP client covering the management
// surface model-manager proxies: health (the loaded models), models, pull
// (streamed), load, unload, delete and system-info. Lemonade serves its own
// endpoints next to the OpenAI-compatible ones under /api/v1.
type Client struct {
	base string
	http *http.Client
	// loadTimeout bounds a load: Lemonade starts the backend process and
	// reads the weights before it answers, which outlives the per-call
	// timeout for large models.
	loadTimeout time.Duration
}

// APIError is a non-2xx answer from Lemonade, a 2xx body that says
// status: error, or the error event of a streamed pull.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("lemonade: %s (%s, HTTP %d)", e.Message, e.Code, e.Status)
	}
	return fmt.Sprintf("lemonade: %s (HTTP %d)", e.Message, e.Status)
}

const (
	apiPrefix = "/api/v1"
	// keyModelName is the request field Lemonade uses for the model id.
	keyModelName = "model_name"

	eventProgress = "progress"
	eventComplete = "complete"
	eventError    = "error"

	statusError = "error"
)

// NewClient returns a client for the given base URL (e.g. http://127.0.0.1:13305).
// loadTimeout bounds POST /api/v1/load (0: 10 minutes).
func NewClient(endpoint string, hc *http.Client, loadTimeout time.Duration) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if loadTimeout <= 0 {
		loadTimeout = 10 * time.Minute
	}
	return &Client{base: strings.TrimRight(endpoint, "/"), http: hc, loadTimeout: loadTimeout}
}

// apiModel is an entry of GET /api/v1/models (the OpenAI model object with
// Lemonade's extensions).
type apiModel struct {
	ID         string `json:"id"`
	Checkpoint string `json:"checkpoint"`
	// Recipe is the runtime the model runs with: flm (FastFlowLM on the
	// NPU), llamacpp, ryzenai-llm, vllm, whispercpp, sd-cpp, ...
	Recipe string `json:"recipe"`
	// SizeGB is the download size in GB.
	SizeGB float64  `json:"size"`
	Labels []string `json:"labels"`
	// Downloaded is nil when the server does not say (older servers list
	// downloads only).
	Downloaded       *bool `json:"downloaded"`
	ContextLength    int64 `json:"context_length"`
	MaxContextWindow int64 `json:"max_context_window"`
}

// downloaded reports whether the entry is on disk; absent means yes.
func (m apiModel) downloaded() bool { return m.Downloaded == nil || *m.Downloaded }

type modelsResponse struct {
	Data []apiModel `json:"data"`
}

// healthResponse is GET /api/v1/health.
type healthResponse struct {
	Status          string        `json:"status"`
	Version         string        `json:"version"`
	AllModelsLoaded []loadedModel `json:"all_models_loaded"`
}

// loadedModel is one entry of health.all_models_loaded.
type loadedModel struct {
	ModelName  string `json:"model_name"`
	Checkpoint string `json:"checkpoint"`
	// Type is llm, embedding, reranking, transcription, image or tts.
	Type string `json:"type"`
	// Device is the space-separated device list: cpu, gpu, npu, "gpu npu".
	Device string `json:"device"`
	Pinned bool   `json:"pinned"`
	Recipe string `json:"recipe"`
	// Status / BackendHealth say whether the backend process answers yet.
	Status           string `json:"status"`
	BackendHealth    string `json:"backend_health"`
	MaxContextWindow int64  `json:"max_context_window"`
	RecipeOptions    struct {
		CtxSize int64 `json:"ctx_size"`
	} `json:"recipe_options"`
}

// systemInfo is the part of GET /api/v1/system-info the node view uses.
type systemInfo struct {
	OSVersion      string `json:"OS Version"`
	Processor      string `json:"Processor"`
	PhysicalMemory string `json:"Physical Memory"`
	// ModelStorage is drive-level: the volume holding the model store, not
	// a sum over model files.
	ModelStorage *struct {
		Path       string `json:"path"`
		UsedBytes  int64  `json:"used_bytes"`
		TotalBytes int64  `json:"total_bytes"`
		FreeBytes  int64  `json:"free_bytes"`
	} `json:"model_storage"`
	Devices struct {
		CPU *struct {
			Name      string `json:"name"`
			Family    string `json:"family"`
			Available bool   `json:"available"`
		} `json:"cpu"`
		AMDGPU    []device `json:"amd_gpu"`
		NvidiaGPU []device `json:"nvidia_gpu"`
		AMDNPU    *device  `json:"amd_npu"`
	} `json:"devices"`
}

type device struct {
	Name       string  `json:"name"`
	Family     string  `json:"family"`
	Available  bool    `json:"available"`
	Integrated bool    `json:"integrated"`
	VRAMGB     float64 `json:"vram_gb"`
}

// statusResponse is the body of load, unload and delete.
type statusResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// pullEvent is one Server-Sent Event of a streamed POST /api/v1/pull.
type pullEvent struct {
	// Event is progress, complete or error.
	Event string `json:"-"`
	// The file being downloaded.
	File            string `json:"file"`
	FileIndex       int    `json:"file_index"`
	TotalFiles      int    `json:"total_files"`
	BytesDownloaded int64  `json:"bytes_downloaded"`
	BytesTotal      int64  `json:"bytes_total"`
	// The whole download, when the server reports it.
	TotalDownloadSize         int64   `json:"total_download_size"`
	CompletedFilesBytes       int64   `json:"completed_files_bytes"`
	CumulativeBytesDownloaded int64   `json:"cumulative_bytes_downloaded"`
	Percent                   float64 `json:"percent"`
	// The error event.
	Code  string `json:"code"`
	Error string `json:"error"`
}

// Health returns the server status, version and loaded models.
func (c *Client) Health(ctx context.Context) (*healthResponse, error) {
	var out healthResponse
	if err := c.do(ctx, c.http, http.MethodGet, "/health", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Models lists the downloaded models.
func (c *Client) Models(ctx context.Context) ([]apiModel, error) {
	var out modelsResponse
	if err := c.do(ctx, c.http, http.MethodGet, "/models", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// SystemInfo returns the host's hardware, memory and model store.
func (c *Client) SystemInfo(ctx context.Context) (*systemInfo, error) {
	var out systemInfo
	if err := c.do(ctx, c.http, http.MethodGet, "/system-info", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Load loads a downloaded model; pinned exempts it from slot eviction.
func (c *Client) Load(ctx context.Context, name string, pinned bool) error {
	body := map[string]any{keyModelName: name}
	if pinned {
		body["pinned"] = true
	}
	hc := &http.Client{Transport: c.http.Transport, Timeout: c.loadTimeout}
	return c.doStatus(ctx, hc, "/load", body)
}

// Unload evicts a loaded model.
func (c *Client) Unload(ctx context.Context, name string) error {
	return c.doStatus(ctx, c.http, "/unload", map[string]any{keyModelName: name})
}

// Delete removes a downloaded model (unloading it first).
func (c *Client) Delete(ctx context.Context, name string) error {
	return c.doStatus(ctx, c.http, "/delete", map[string]any{keyModelName: name})
}

// doStatus posts and reads the {status, message} answer, turning a 2xx
// status: error into an APIError.
func (c *Client) doStatus(ctx context.Context, hc *http.Client, path string, in any) error {
	var out statusResponse
	if err := c.do(ctx, hc, http.MethodPost, path, in, &out); err != nil {
		return err
	}
	if out.Status == statusError {
		return &APIError{Status: http.StatusOK, Message: out.Message}
	}
	return nil
}

// Pull streams POST /api/v1/pull and calls onEvent for every progress event
// and the final complete event. It returns when the stream completes, or
// with the first error (the error event, or a non-2xx answer).
func (c *Client) Pull(ctx context.Context, name string, onEvent func(pullEvent)) error {
	body, err := json.Marshal(map[string]any{keyModelName: name, "stream": true})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+apiPrefix+"/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// Pulls run for minutes; bypass the per-call timeout but keep dial/TLS
	// settings by reusing the transport.
	streamClient := &http.Client{Transport: c.http.Transport}
	resp, err := streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("lemonade: pull %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return readAPIError(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var (
		event string
		data  []byte
	)
	// dispatch delivers the buffered event; done is true after complete.
	dispatch := func() (done bool, err error) {
		defer func() { event, data = "", nil }()
		if len(data) == 0 {
			return false, nil
		}
		var ev pullEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return false, fmt.Errorf("lemonade: pull %s: bad progress event: %w", name, err)
		}
		ev.Event = event
		if ev.Event == "" {
			ev.Event = eventProgress
		}
		switch {
		case ev.Event == eventError || ev.Error != "":
			msg := ev.Error
			if msg == "" {
				msg = "download failed"
			}
			return false, &APIError{Status: resp.StatusCode, Code: ev.Code, Message: msg}
		case ev.Event == eventComplete:
			if onEvent != nil {
				onEvent(ev)
			}
			return true, nil
		default:
			if onEvent != nil {
				onEvent(ev)
			}
			return false, nil
		}
	}
	for scanner.Scan() {
		line := bytes.TrimRight(scanner.Bytes(), "\r")
		trimmed := bytes.TrimSpace(line)
		switch {
		case len(trimmed) == 0:
			done, err := dispatch()
			if err != nil || done {
				return err
			}
		case bytes.HasPrefix(line, []byte("event:")):
			event = strings.TrimSpace(string(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			data = append(data, bytes.TrimSpace(line[len("data:"):])...)
		case bytes.HasPrefix(trimmed, []byte("{")):
			// Not a stream: the server answered with one JSON document
			// (an older server ignoring stream=true) — {status, message},
			// or one of the error shapes.
			var st statusResponse
			_ = json.Unmarshal(trimmed, &st)
			switch {
			case st.Status == statusError:
				return &APIError{Status: resp.StatusCode, Message: st.Message}
			case st.Status != "":
				// success
			default:
				if code, msg := parseErrorBody(trimmed); msg != "" {
					return &APIError{Status: resp.StatusCode, Code: code, Message: msg}
				}
			}
			if onEvent != nil {
				onEvent(pullEvent{Event: eventComplete})
			}
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("lemonade: pull %s: stream: %w", name, err)
	}
	// A stream that ends right after its last event without a blank line.
	if done, err := dispatch(); err != nil || done {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("lemonade: pull %s: stream ended without completing", name)
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
		return fmt.Errorf("lemonade: %s %s: %w", method, path, err)
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
		return fmt.Errorf("lemonade: %s %s: decode: %w", method, path, err)
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

// parseErrorBody reads the error shapes Lemonade answers with:
// {"error":"message"}, {"error":{"code":…,"message":…,"type":…}},
// {"status":"error","message":…} and {"code":…,"error":…}.
func parseErrorBody(raw []byte) (code, message string) {
	var body struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Code    string          `json:"code"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", ""
	}
	code, message = body.Code, body.Message
	if len(body.Error) == 0 {
		return code, message
	}
	var s string
	if json.Unmarshal(body.Error, &s) == nil {
		if message == "" {
			message = s
		}
		return code, message
	}
	var obj struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(body.Error, &obj) == nil {
		if obj.Message != "" {
			message = obj.Message
		}
		switch {
		case obj.Code != "":
			code = obj.Code
		case obj.Type != "":
			code = obj.Type
		}
	}
	return code, message
}
