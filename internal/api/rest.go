// Package api exposes the service twice from one process: REST/JSON under
// /api/v1 for the portal backend, and MCP tools for muster. Both map onto the
// same service methods so behavior cannot drift between them.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	spec "github.com/giantswarm/model-manager/api"
	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

// Prefix is the REST base path.
const Prefix = "/api/v1"

const maxBodyBytes = 1 << 20

// REST serves the JSON API.
type REST struct {
	svc *service.Service
	log *slog.Logger
}

// NewREST builds the REST handler set.
func NewREST(svc *service.Service, log *slog.Logger) *REST {
	if log == nil {
		log = slog.Default()
	}
	return &REST{svc: svc, log: log}
}

// Register mounts the routes on mux.
func (h *REST) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Prefix+"/backend", h.getBackend)
	mux.HandleFunc("GET "+Prefix+"/openapi.yaml", h.getOpenAPI)
	mux.HandleFunc("GET "+Prefix+"/models", h.listModels)
	mux.HandleFunc("GET "+Prefix+"/loaded", h.listLoaded)
	mux.HandleFunc("POST "+Prefix+"/models/pull", h.pull)
	mux.HandleFunc("POST "+Prefix+"/models/load", h.load)
	mux.HandleFunc("POST "+Prefix+"/models/unload", h.unload)
	mux.HandleFunc("POST "+Prefix+"/models/wire", h.wire)
	mux.HandleFunc("POST "+Prefix+"/models/unwire", h.unwire)
	mux.HandleFunc("POST "+Prefix+"/models/fit-check", h.fitCheck)
	mux.HandleFunc("GET "+Prefix+"/models/{name...}", h.getModel)
	mux.HandleFunc("DELETE "+Prefix+"/models/{name...}", h.deleteModel)
	mux.HandleFunc("GET "+Prefix+"/jobs", h.listJobs)
	mux.HandleFunc("GET "+Prefix+"/jobs/{id}", h.getJob)
	mux.HandleFunc("DELETE "+Prefix+"/jobs/{id}", h.cancelJob)
	// kserve capabilities (501 unsupported when the flag is false).
	mux.HandleFunc("GET "+Prefix+"/presets", h.listPresets)
	mux.HandleFunc("GET "+Prefix+"/search", h.search)
	mux.HandleFunc("GET "+Prefix+"/nodes", h.listNodes)
}

type modelRequest struct {
	Model     string `json:"model"`
	Wire      *bool  `json:"wire,omitempty"`
	KeepAlive string `json:"keepAlive,omitempty"`
	// Preset / Node are kserve concerns: the serving preset to use and the
	// node to pull to / serve on.
	Preset string `json:"preset,omitempty"`
	Node   string `json:"node,omitempty"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (h *REST) getBackend(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.svc.Backend(r.Context()))
}

func (h *REST) getOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(spec.OpenAPI)
}

func (h *REST) listModels(w http.ResponseWriter, r *http.Request) {
	models, err := h.svc.ListModels(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (h *REST) getModel(w http.ResponseWriter, r *http.Request) {
	m, err := h.svc.GetModel(r.Context(), r.PathValue("name"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *REST) deleteModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	unwire := !strings.EqualFold(r.URL.Query().Get("unwire"), "false")
	if err := h.svc.Delete(r.Context(), name, unwire); err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{argModel: name, "deleted": true, "unwired": unwire})
}

func (h *REST) listLoaded(w http.ResponseWriter, r *http.Request) {
	loaded, err := h.svc.ListLoaded(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"loaded": loaded})
}

func (h *REST) pull(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decodeModelRequest(w, r)
	if !ok {
		return
	}
	job, created, err := h.svc.Pull(r.Context(), service.PullOptions{Model: req.Model, Wire: req.Wire, Preset: req.Preset, Node: req.Node})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job, "created": created})
}

func (h *REST) load(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decodeModelRequest(w, r, true)
	if !ok {
		return
	}
	m, err := h.svc.Load(r.Context(), service.LoadOptions{Model: req.Model, KeepAlive: req.KeepAlive, Preset: req.Preset, Node: req.Node})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *REST) fitCheck(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decodeModelRequest(w, r, true)
	if !ok {
		return
	}
	res, err := h.svc.FitCheck(r.Context(), backend.FitRequest{Model: req.Model, Preset: req.Preset, Node: req.Node})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *REST) listPresets(w http.ResponseWriter, r *http.Request) {
	presets, err := h.svc.Presets(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"presets": presets})
}

func (h *REST) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			h.writeError(w, fmt.Errorf("%w: limit must be a non-negative integer", backend.ErrInvalid))
			return
		}
		limit = n
	}
	hits, err := h.svc.Search(r.Context(), q.Get("q"), limit)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": q.Get("q"), "results": hits})
}

func (h *REST) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.svc.Nodes(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (h *REST) unload(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decodeModelRequest(w, r)
	if !ok {
		return
	}
	if err := h.svc.Unload(r.Context(), req.Model); err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{argModel: req.Model, "loaded": false})
}

func (h *REST) wire(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decodeModelRequest(w, r)
	if !ok {
		return
	}
	ref, err := h.svc.Wire(r.Context(), req.Model)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{argModel: req.Model, "modelConfig": ref})
}

func (h *REST) unwire(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decodeModelRequest(w, r)
	if !ok {
		return
	}
	if err := h.svc.Unwire(r.Context(), req.Model); err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{argModel: req.Model, "modelConfig": nil})
}

func (h *REST) listJobs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": h.svc.Jobs()})
}

func (h *REST) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := h.svc.Job(r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (h *REST) cancelJob(w http.ResponseWriter, r *http.Request) {
	job, err := h.svc.CancelJob(r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// decodeModelRequest reads the JSON body. presetOK lets "preset" stand in for
// "model" (load / fit-check on preset-driven backends).
func (h *REST) decodeModelRequest(w http.ResponseWriter, r *http.Request, presetOK ...bool) (modelRequest, bool) {
	var req modelRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		h.writeError(w, fmt.Errorf("%w: read body: %v", backend.ErrInvalid, err))
		return req, false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		h.writeError(w, fmt.Errorf("%w: JSON body with \"model\" is required", backend.ErrInvalid))
		return req, false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		h.writeError(w, fmt.Errorf("%w: invalid JSON: %v", backend.ErrInvalid, err))
		return req, false
	}
	allowPreset := len(presetOK) > 0 && presetOK[0]
	if strings.TrimSpace(req.Model) == "" && (!allowPreset || strings.TrimSpace(req.Preset) == "") {
		h.writeError(w, fmt.Errorf("%w: \"model\" is required", backend.ErrInvalid))
		return req, false
	}
	return req, true
}

func (h *REST) writeError(w http.ResponseWriter, err error) {
	status, code := statusFor(err)
	if status >= http.StatusInternalServerError {
		h.log.Error("request failed", "status", status, "error", err)
	}
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: err.Error()}})
}

// statusFor maps domain errors to HTTP status and a stable code clients can
// switch on.
func statusFor(err error) (int, string) {
	switch {
	case errors.Is(err, backend.ErrNotFound), errors.Is(err, jobs.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, backend.ErrInvalid):
		return http.StatusBadRequest, "invalid_request"
	case errors.Is(err, backend.ErrUnsupported):
		return http.StatusNotImplemented, "unsupported"
	case errors.Is(err, backend.ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, backend.ErrUnfit):
		return http.StatusPreconditionFailed, "does_not_fit"
	default:
		return http.StatusBadGateway, "backend_error"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
