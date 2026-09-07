package lmstudio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
)

const (
	modelGranite = "ibm/granite-4-micro"
	modelQwen    = "qwen/qwen3-1.7b"
	modelEmbed   = "text-embedding-nomic-embed-text-v1.5"
)

// fakeLMStudio is an httptest stand-in for LM Studio's /api/v1 API.
type fakeLMStudio struct {
	mu     sync.Mutex
	models map[string]*apiModel
	// jobs are the download jobs the fake has started.
	jobs map[string]*downloadJob
	// pollsLeft counts how many status reads stay "downloading" before the
	// job completes; 0 completes on the first read.
	pollsLeft int
	// downloadFail makes a started download fail instead of completing.
	downloadFail string
	srv          *httptest.Server
}

func newFakeLMStudio(t *testing.T) *fakeLMStudio {
	t.Helper()
	f := &fakeLMStudio{models: map[string]*apiModel{}, jobs: map[string]*downloadJob{}}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		list := []apiModel{}
		for _, m := range f.models {
			list = append(list, *m)
		}
		_ = json.NewEncoder(w).Encode(modelsResponse{Models: &list})
	})

	mux.HandleFunc("POST /api/v1/models/download", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.models[req.Model]; ok {
			_ = json.NewEncoder(w).Encode(downloadJob{JobID: "job_dup", Status: statusAlreadyDownloaded, TotalSizeBytes: f.models[req.Model].SizeBytes})
			return
		}
		if req.Model == "" {
			writeErr(w, http.StatusBadRequest, "invalid_request", "Invalid artifact name. Must be in kebab-case")
			return
		}
		job := &downloadJob{JobID: "job_" + req.Model, Status: statusDownloading, TotalSizeBytes: 2_100_000_000}
		f.jobs[job.JobID] = job
		f.jobs[job.JobID+"_ref"] = &downloadJob{JobID: req.Model}
		_ = json.NewEncoder(w).Encode(job)
	})

	mux.HandleFunc("GET /api/v1/models/download/status/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		job, ok := f.jobs[id]
		if !ok {
			writeErr(w, http.StatusNotFound, "job_not_found", fmt.Sprintf("Download job with id '%s' not found", id))
			return
		}
		if f.downloadFail != "" {
			job.Status, job.Error = statusFailed, f.downloadFail
			_ = json.NewEncoder(w).Encode(job)
			return
		}
		if f.pollsLeft > 0 {
			f.pollsLeft--
			job.DownloadedBytes += 700_000_000
			job.BytesPerSecond = 50_000_000
			_ = json.NewEncoder(w).Encode(job)
			return
		}
		job.Status = statusCompleted
		// The download landed: the model is in the library now.
		ref := f.jobs[id+"_ref"]
		if ref != nil {
			f.models[ref.JobID] = llm(ref.JobID, job.TotalSizeBytes, true)
		}
		_ = json.NewEncoder(w).Encode(job)
	})

	mux.HandleFunc("POST /api/v1/models/load", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		m, ok := f.models[req.Model]
		if !ok {
			writeErr(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("Model %s not found in downloaded models", req.Model))
			return
		}
		inst := loadedInstance{ID: m.Key}
		inst.Config.ContextLength = 4096
		m.LoadedInstances = append(m.LoadedInstances, inst)
		_ = json.NewEncoder(w).Encode(loadResponse{InstanceID: inst.ID, Status: "loaded"})
	})

	mux.HandleFunc("POST /api/v1/models/unload", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			InstanceID string `json:"instance_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, m := range f.models {
			for i, inst := range m.LoadedInstances {
				if inst.ID == req.InstanceID {
					m.LoadedInstances = append(m.LoadedInstances[:i], m.LoadedInstances[i+1:]...)
					_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": req.InstanceID})
					return
				}
			}
		}
		writeErr(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("Model with instance identifier '%s' is not loaded.", req.InstanceID))
	})

	// Everything outside /api/v1 answers HTTP 200 with an error document —
	// LM Studio really does this, Ollama's /api/version included, so the
	// fake must too or the tests prove nothing about the shape checks.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("Unexpected endpoint or method. (%s %s)", r.Method, r.URL.Path),
		})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// writeErr answers the nested error document the /api/v1 endpoints use.
func writeErr(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"type": kind, "message": message},
	})
}

// llm builds a library entry for a text model.
func llm(key string, size int64, tools bool) *apiModel {
	m := &apiModel{
		Key: key, Type: typeLLM, Publisher: "test", DisplayName: key,
		Architecture: "llama", SizeBytes: size, Format: "gguf",
		ParamsString: "4B", MaxContextLength: 131072,
		Capabilities:    &modelCapabilities{TrainedForToolUse: tools},
		LoadedInstances: []loadedInstance{},
	}
	m.Quantization = &struct {
		Name          string `json:"name"`
		BitsPerWeight int    `json:"bits_per_weight"`
	}{Name: "Q4_K_M", BitsPerWeight: 4}
	return m
}

// embedding builds a library entry for an embedding model: LM Studio sends
// no capability object at all for those.
func embedding(key string, size int64) *apiModel {
	return &apiModel{Key: key, Type: "embedding", SizeBytes: size, Format: "gguf", LoadedInstances: []loadedInstance{}}
}

func (f *fakeLMStudio) add(m *apiModel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models[m.Key] = m
}

func newTestBackend(t *testing.T) (*fakeLMStudio, *Backend) {
	t.Helper()
	f := newFakeLMStudio(t)
	b, err := New(backend.LMStudioOptions{Endpoint: f.srv.URL})
	require.NoError(t, err)
	// Real time is not the thing under test.
	b.pollInterval = time.Millisecond
	return f, b
}

func TestNewValidatesEndpoint(t *testing.T) {
	_, err := New(backend.LMStudioOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint is required")

	_, err = New(backend.LMStudioOptions{Endpoint: "127.0.0.1:1234"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be an http(s) URL")

	b, err := New(backend.LMStudioOptions{Endpoint: "http://host:1234/"})
	require.NoError(t, err)
	assert.Equal(t, "http://host:1234", b.endpoint)
	// AgentHost defaults to the endpoint.
	assert.Equal(t, "http://host:1234/v1", b.agentBaseURL())
}

func TestCapabilitiesHasNoDelete(t *testing.T) {
	_, b := newTestBackend(t)
	caps := b.Capabilities()
	assert.False(t, caps.Delete, "LM Studio has no delete over REST")
	assert.False(t, caps.NodeInventory, "LM Studio reports no host hardware")
	assert.True(t, caps.Pull)
	assert.True(t, caps.PullProgress)
	assert.True(t, caps.Load)
	assert.True(t, caps.Unload)
	assert.True(t, caps.LoadedModels)

	// Delete is refused with the sentinel the service maps to 501.
	err := b.Delete(context.Background(), modelGranite)
	require.Error(t, err)
	assert.ErrorIs(t, err, backend.ErrUnsupported)
	assert.Contains(t, err.Error(), "lms rm")
}

func TestInfoHealthIsTheInventoryShape(t *testing.T) {
	f, b := newTestBackend(t)
	f.add(llm(modelGranite, 2_100_000_000, true))

	info := b.Info(context.Background())
	assert.True(t, info.Healthy)
	// LM Studio serves no version anywhere.
	assert.Empty(t, info.Version)
	assert.Equal(t, backend.NameLMStudio, info.Backend)
	assert.Equal(t, f.srv.URL+"/v1", info.AgentEndpoint)
	assert.True(t, info.Loading.OnDemand)
}

// A server that answers 200 without a models array is not an LM Studio, and
// must not read as a healthy one. This is the 200-on-unknown-path trap.
func TestInfoUnhealthyWithoutModelsArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":"Unexpected endpoint or method. (GET /api/v1/models)"}`))
	}))
	defer srv.Close()
	b, err := New(backend.LMStudioOptions{Endpoint: srv.URL})
	require.NoError(t, err)

	info := b.Info(context.Background())
	assert.False(t, info.Healthy)
	assert.Contains(t, info.Message, "without a models array")
}

func TestListModelsTagsCapabilities(t *testing.T) {
	f, b := newTestBackend(t)
	f.add(llm(modelGranite, 2_100_000_000, true))
	f.add(llm(modelQwen, 1_000_000_000, false))
	f.add(embedding(modelEmbed, 84_106_624))

	models, err := b.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 3, "the library is the inventory, embeddings included")

	byName := map[string]backend.Model{}
	for _, m := range models {
		byName[m.Name] = m
	}
	granite := byName[modelGranite]
	// size_bytes is already bytes — no GB conversion, unlike lemonade.
	assert.Equal(t, int64(2_100_000_000), granite.SizeBytes)
	assert.Equal(t, "Q4_K_M", granite.Quantization)
	assert.Equal(t, "gguf", granite.Format)
	assert.Equal(t, "llama", granite.Family)
	assert.Equal(t, int64(131072), granite.ContextLength)
	assert.Equal(t, []string{"completion", "tools"}, granite.Capabilities)
	require.NotNil(t, granite.Downloaded)
	assert.True(t, *granite.Downloaded)

	// A model not trained for tool use says so.
	assert.Equal(t, []string{"completion"}, byName[modelQwen].Capabilities)
	// An embedding model is listed and tagged, as on lemonade — LM Studio
	// sends no capability object for it at all.
	assert.Equal(t, []string{"embedding"}, byName[modelEmbed].Capabilities)
}

// What ListModels shows and what GetModel finds must agree.
func TestListAndGetAgree(t *testing.T) {
	f, b := newTestBackend(t)
	f.add(llm(modelGranite, 2_100_000_000, true))
	f.add(embedding(modelEmbed, 84_106_624))
	ctx := context.Background()

	models, err := b.ListModels(ctx)
	require.NoError(t, err)
	for _, m := range models {
		got, err := b.GetModel(ctx, m.Name)
		require.NoError(t, err, m.Name)
		assert.Equal(t, m.Name, got.Name)
		assert.Equal(t, m.Capabilities, got.Capabilities)
	}
}

func TestGetModel(t *testing.T) {
	f, b := newTestBackend(t)
	f.add(llm(modelGranite, 2_100_000_000, true))

	got, err := b.GetModel(context.Background(), modelGranite)
	require.NoError(t, err)
	assert.Equal(t, modelGranite, got.Name)

	// Case folding, as on lemonade.
	got, err = b.GetModel(context.Background(), "IBM/Granite-4-Micro")
	require.NoError(t, err)
	assert.Equal(t, modelGranite, got.Name)

	_, err = b.GetModel(context.Background(), "nope/nope")
	assert.ErrorIs(t, err, backend.ErrNotFound)

	_, err = b.GetModel(context.Background(), "  ")
	assert.ErrorIs(t, err, backend.ErrInvalid)
}

func TestPullFollowsTheDownloadJob(t *testing.T) {
	f, b := newTestBackend(t)
	f.pollsLeft = 2

	var seen []backend.Progress
	err := b.Pull(context.Background(), backend.PullRequest{Ref: modelGranite}, func(p backend.Progress) {
		seen = append(seen, p)
	})
	require.NoError(t, err)
	require.NotEmpty(t, seen)

	last := seen[len(seen)-1]
	assert.Equal(t, "success", last.Status)
	assert.Equal(t, int64(2_100_000_000), last.BytesTotal)
	assert.Equal(t, last.BytesTotal, last.BytesCompleted, "a finished pull reports the whole size")

	// Progress never goes backwards.
	for i := 1; i < len(seen); i++ {
		assert.GreaterOrEqual(t, seen[i].BytesCompleted, seen[i-1].BytesCompleted)
	}
	// The model is in the library afterwards.
	models, err := b.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, modelGranite, models[0].Name)
}

func TestPullAlreadyDownloaded(t *testing.T) {
	f, b := newTestBackend(t)
	f.add(llm(modelGranite, 2_100_000_000, true))

	var seen []backend.Progress
	err := b.Pull(context.Background(), backend.PullRequest{Ref: modelGranite}, func(p backend.Progress) {
		seen = append(seen, p)
	})
	require.NoError(t, err)
	require.Len(t, seen, 1, "nothing to download, so one final event")
	assert.Equal(t, "success", seen[0].Status)
}

func TestPullFailureAndEmptyRef(t *testing.T) {
	f, b := newTestBackend(t)
	f.pollsLeft = 1
	f.downloadFail = "no space left on device"

	err := b.Pull(context.Background(), backend.PullRequest{Ref: modelGranite}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no space left on device")

	err = b.Pull(context.Background(), backend.PullRequest{Ref: " "}, nil)
	assert.ErrorIs(t, err, backend.ErrInvalid)
}

func TestPullHonoursContextCancellation(t *testing.T) {
	f, b := newTestBackend(t)
	// Never finishes on its own.
	f.pollsLeft = 1 << 30

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := b.Pull(ctx, backend.PullRequest{Ref: modelGranite}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestLoadUnloadAndListLoaded(t *testing.T) {
	f, b := newTestBackend(t)
	f.add(llm(modelGranite, 2_100_000_000, true))
	ctx := context.Background()

	loaded, err := b.ListLoaded(ctx)
	require.NoError(t, err)
	assert.Empty(t, loaded)

	require.NoError(t, b.Load(ctx, backend.LoadRequest{Name: modelGranite}))
	loaded, err = b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, modelGranite, loaded[0].Name)
	assert.Equal(t, statusLoaded, loaded[0].Status)
	assert.Equal(t, int64(4096), loaded[0].ContextLength, "the context it was loaded with, not the maximum")
	assert.Equal(t, int64(2_100_000_000), loaded[0].SizeBytes)

	require.NoError(t, b.Unload(ctx, modelGranite))
	loaded, err = b.ListLoaded(ctx)
	require.NoError(t, err)
	assert.Empty(t, loaded)

	// Unloading again is a no-op, as on ollama and lemonade.
	require.NoError(t, b.Unload(ctx, modelGranite))
	// So is unloading something that was never in the library.
	require.NoError(t, b.Unload(ctx, "nope/nope"))

	assert.ErrorIs(t, b.Unload(ctx, " "), backend.ErrInvalid)
	assert.ErrorIs(t, b.Load(ctx, backend.LoadRequest{Name: ""}), backend.ErrInvalid)
}

func TestLoadUnknownModel(t *testing.T) {
	_, b := newTestBackend(t)
	err := b.Load(context.Background(), backend.LoadRequest{Name: "nope/nope"})
	require.Error(t, err)
	assert.ErrorIs(t, err, backend.ErrNotFound)
}

func TestAgentEndpoint(t *testing.T) {
	for _, tc := range []struct{ agentHost, want string }{
		{"http://172.21.0.1:1234", "http://172.21.0.1:1234/v1"},
		{"http://172.21.0.1:1234/", "http://172.21.0.1:1234/v1"},
		// Already carrying the suffix: appended only once.
		{"http://172.21.0.1:1234/v1", "http://172.21.0.1:1234/v1"},
	} {
		b := NewWithClient(nil, "http://127.0.0.1:1234", tc.agentHost)
		ep := b.AgentEndpoint(modelGranite)
		assert.Equal(t, tc.want, ep.BaseURL, tc.agentHost)
		assert.Equal(t, ProviderOpenAI, ep.Provider)
		assert.Equal(t, modelGranite, ep.Model)
		assert.True(t, ep.PlaceholderAPIKey, "the kagent runtime insists on a key env var")
		assert.Empty(t, ep.Host, "the OpenAI provider carries a baseUrl, not a host")
	}
}

func TestMapErr(t *testing.T) {
	assert.NoError(t, mapErr(nil, modelGranite))

	err := mapErr(&APIError{Status: http.StatusNotFound, Code: "model_not_found", Message: "Model x not found in downloaded models"}, modelGranite)
	assert.ErrorIs(t, err, backend.ErrNotFound)

	err = mapErr(&APIError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "Invalid artifact name. Must be in kebab-case"}, modelGranite)
	assert.ErrorIs(t, err, backend.ErrInvalid)

	// A server error stays one: the service answers 502.
	err = mapErr(&APIError{Status: http.StatusInternalServerError, Message: "boom"}, modelGranite)
	assert.NotErrorIs(t, err, backend.ErrNotFound)
	assert.NotErrorIs(t, err, backend.ErrInvalid)
}

func TestParseErrorBody(t *testing.T) {
	// The nested shape the /api/v1 endpoints answer with.
	code, msg := parseErrorBody([]byte(`{"error":{"type":"job_not_found","message":"Download job with id 'nope' not found"}}`))
	assert.Equal(t, "job_not_found", code)
	assert.Contains(t, msg, "not found")

	// The flat shape an unrecognised path answers with.
	code, msg = parseErrorBody([]byte(`{"error":"Unexpected endpoint or method. (GET /api/version)"}`))
	assert.Empty(t, code)
	assert.Contains(t, msg, "Unexpected endpoint")

	// code falls back to the `code` field when there is no type.
	code, _ = parseErrorBody([]byte(`{"error":{"code":"invalid_arguments","message":"bad"}}`))
	assert.Equal(t, "invalid_arguments", code)

	code, msg = parseErrorBody([]byte(`not json`))
	assert.Empty(t, code)
	assert.Empty(t, msg)
}

func TestHostOf(t *testing.T) {
	assert.Equal(t, "172.21.0.1", hostOf("http://172.21.0.1:1234"))
	assert.Equal(t, "host.docker.internal", hostOf("http://host.docker.internal:1234/v1"))
	assert.Equal(t, "nonsense", hostOf("nonsense"))
}

func TestHumanBytes(t *testing.T) {
	assert.Equal(t, "512 B", humanBytes(512))
	assert.Equal(t, "50.0 MB", humanBytes(50_000_000))
	assert.Equal(t, "2.1 GB", humanBytes(2_100_000_000))
}

// The driver satisfies the interface the service consumes.
var _ backend.Backend = (*Backend)(nil)

// The driver deliberately does NOT offer node inventory.
func TestNoNodeLister(t *testing.T) {
	_, b := newTestBackend(t)
	_, ok := any(b).(backend.NodeLister)
	assert.False(t, ok, "LM Studio reports no host hardware, so the service answers 501")
}

// Guard against a poll interval nobody meant to ship.
func TestPollIntervalIsSane(t *testing.T) {
	assert.Greater(t, defaultPollInterval, 100*time.Millisecond)
	assert.LessOrEqual(t, defaultPollInterval, 10*time.Second)
}
