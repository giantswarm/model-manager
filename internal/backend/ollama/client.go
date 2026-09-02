package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a minimal Ollama HTTP API client covering the management surface
// model-manager proxies: tags, ps, pull (streamed), delete, show, version and
// keep_alive-driven load/unload.
type Client struct {
	base string
	http *http.Client
}

// APIError is a non-2xx answer from Ollama.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ollama: %s (HTTP %d)", e.Message, e.Status)
}

// NewClient returns a client for the given base URL (e.g. http://127.0.0.1:11434).
func NewClient(endpoint string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimRight(endpoint, "/"), http: hc}
}

type apiDetails struct {
	ParentModel       string   `json:"parent_model"`
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
	ContextLength     int64    `json:"context_length"`
	EmbeddingLength   int64    `json:"embedding_length"`
}

// apiModel is an entry of /api/tags or /api/ps (the latter adds the memory
// and expiry fields).
type apiModel struct {
	Name          string     `json:"name"`
	Model         string     `json:"model"`
	ModifiedAt    time.Time  `json:"modified_at"`
	Size          int64      `json:"size"`
	Digest        string     `json:"digest"`
	Details       apiDetails `json:"details"`
	Capabilities  []string   `json:"capabilities"`
	SizeVRAM      int64      `json:"size_vram"`
	ExpiresAt     *time.Time `json:"expires_at"`
	ContextLength int64      `json:"context_length"`
}

type modelsResponse struct {
	Models []apiModel `json:"models"`
}

type showResponse struct {
	Details      apiDetails `json:"details"`
	Capabilities []string   `json:"capabilities"`
	ModifiedAt   time.Time  `json:"modified_at"`
}

// keyModel is the request field Ollama uses for the model reference.
const keyModel = "model"

// pullEvent is one NDJSON line of a streamed /api/pull.
type pullEvent struct {
	Status    string `json:"status"`
	Digest    string `json:"digest"`
	Total     int64  `json:"total"`
	Completed int64  `json:"completed"`
	Error     string `json:"error"`
}

// Version returns the Ollama server version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/version", nil, &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

// Tags lists downloaded models.
func (c *Client) Tags(ctx context.Context) ([]apiModel, error) {
	var out modelsResponse
	if err := c.do(ctx, http.MethodGet, "/api/tags", nil, &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// PS lists loaded models.
func (c *Client) PS(ctx context.Context) ([]apiModel, error) {
	var out modelsResponse
	if err := c.do(ctx, http.MethodGet, "/api/ps", nil, &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// Show returns model details.
func (c *Client) Show(ctx context.Context, name string) (*showResponse, error) {
	var out showResponse
	if err := c.do(ctx, http.MethodPost, "/api/show", map[string]any{keyModel: name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes a downloaded model.
func (c *Client) Delete(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/delete", map[string]any{keyModel: name}, nil)
}

// SetKeepAlive loads (positive keep-alive) or unloads (0) a model by issuing
// an empty generate request. Embedding-only models reject /api/generate, so
// the call falls back to /api/embed with the same keep_alive.
func (c *Client) SetKeepAlive(ctx context.Context, name string, keepAlive any) error {
	err := c.do(ctx, http.MethodPost, "/api/generate", map[string]any{keyModel: name, "keep_alive": keepAlive}, nil)
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(apiErr.Message), "embed") {
		return c.do(ctx, http.MethodPost, "/api/embed", map[string]any{keyModel: name, "input": "", "keep_alive": keepAlive}, nil)
	}
	return err
}

// Pull streams /api/pull and calls onEvent for each progress line. It returns
// when the stream ends with status "success", or with the first error.
func (c *Client) Pull(ctx context.Context, ref string, onEvent func(pullEvent)) error {
	body, err := json.Marshal(map[string]any{keyModel: ref, "stream": true})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Pulls run for minutes; bypass the per-call timeout but keep dial/TLS
	// settings by cloning the transport.
	streamClient := &http.Client{Transport: c.http.Transport}
	resp, err := streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: pull %s: %w", ref, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return readAPIError(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev pullEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("ollama: pull %s: bad progress line: %w", ref, err)
		}
		if ev.Error != "" {
			return &APIError{Status: resp.StatusCode, Message: ev.Error}
		}
		if onEvent != nil {
			onEvent(ev)
		}
		if ev.Status == "success" {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ollama: pull %s: stream: %w", ref, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("ollama: pull %s: stream ended without success", ref)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: %s %s: %w", method, path, err)
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
		return fmt.Errorf("ollama: %s %s: decode: %w", method, path, err)
	}
	return nil
}

func readAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	msg := strings.TrimSpace(string(raw))
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error != "" {
		msg = parsed.Error
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &APIError{Status: resp.StatusCode, Message: msg}
}
