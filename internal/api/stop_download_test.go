package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

// stoppingBackend is a backend whose downloads are shared cluster objects.
type stoppingBackend struct {
	*fakeBackend
	got backend.StopDownloadRequest
	err error
}

func (s *stoppingBackend) StopDownload(_ context.Context, req backend.StopDownloadRequest) (*backend.StoppedDownload, error) {
	s.got = req
	if s.err != nil {
		return nil, s.err
	}
	return &backend.StoppedDownload{
		Object: backend.JobObject{Group: "batch", Version: "v1", Resource: "jobs", Namespace: "kserve", Name: "mm-pull-tiny", UID: "job-uid"},
		Model:  "org/tiny", Node: req.Node, Preset: req.Preset, DryRun: req.DryRun,
	}, nil
}

func stoppingService(t *testing.T) (*stoppingBackend, *service.Service) {
	t.Helper()
	fb := newFakeBackend()
	fb.name = backend.NameKServe
	sb := &stoppingBackend{fakeBackend: fb}
	return sb, service.New([]backend.Backend{sb}, jobs.NewManager(jobs.WithAnonymousAccess(true)), nil, nil, service.Config{}, nil)
}

func TestStopDownloadREST(t *testing.T) {
	sb, svc := stoppingService(t)
	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f := &fixture{svc: svc, srv: srv}

	status, body := f.do(t, http.MethodPost, Prefix+"/models/stop-download", map[string]any{"model": " org/tiny ", "node": "n1", "preset": "tiny", "uid": "job-uid", "dryRun": true})
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, backend.StopDownloadRequest{Ref: "org/tiny", Node: "n1", Preset: "tiny", UID: "job-uid", DryRun: true}, sb.got)
	require.Equal(t, false, body["stopped"])
	require.Equal(t, true, body[argDryRun])
	download := body["download"].(map[string]any)
	require.Equal(t, "mm-pull-tiny", download["object"].(map[string]any)["name"])

	status, body = f.do(t, http.MethodPost, Prefix+"/models/stop-download", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, true, body["stopped"])
	require.NotContains(t, body, argDryRun)

	sb.err = apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "mm-pull-tiny", nil)
	status, body = f.do(t, http.MethodPost, Prefix+"/models/stop-download", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusForbidden, status, body)

	status, body = f.do(t, http.MethodPost, Prefix+"/models/stop-download", map[string]any{})
	require.Equal(t, http.StatusBadRequest, status, body)
}

func TestStopDownloadUnsupportedWhereDownloadsAreNotShared(t *testing.T) {
	f := newFixture(t, false)
	status, body := f.do(t, http.MethodPost, Prefix+"/models/stop-download", map[string]any{"model": "qwen3:0.6b"})
	require.Equal(t, http.StatusNotImplemented, status, body)

	text, isErr := callTool(t, NewMCPServer(f.svc, buildinfo.Info{}), ToolStopDownload, map[string]any{argModel: "qwen3:0.6b"})
	require.True(t, isErr, text)
	require.Contains(t, text, "unsupported")
}

func TestStopDownloadMCP(t *testing.T) {
	sb, svc := stoppingService(t)
	text, isErr := callTool(t, NewMCPServer(svc, buildinfo.Info{}), ToolStopDownload, map[string]any{argModel: "tiny", argNode: "n1", argUID: "job-uid"})
	require.False(t, isErr, text)
	require.Equal(t, backend.StopDownloadRequest{Ref: "tiny", Node: "n1", UID: "job-uid"}, sb.got)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	require.Equal(t, true, out["stopped"])
	require.Equal(t, "kserve", out[argBackend])
	require.Contains(t, out["next"], "tasks observing it fail with download stopped")

	sb.err = backend.ErrConflict
	text, isErr = callTool(t, NewMCPServer(svc, buildinfo.Info{}), ToolStopDownload, map[string]any{argModel: "tiny"})
	require.True(t, isErr, text)
}
