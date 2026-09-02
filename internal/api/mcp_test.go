package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

func callTool(t *testing.T, srv interface {
	HandleMessage(ctx context.Context, message json.RawMessage) mcp.JSONRPCMessage
}, name string, args map[string]any) (string, bool) {
	t.Helper()
	req := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	}
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	resp := srv.HandleMessage(context.Background(), raw)
	out, err := json.Marshal(resp)
	require.NoError(t, err)
	var parsed struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	if parsed.Error != nil {
		return parsed.Error.Message, true
	}
	require.NotEmpty(t, parsed.Result.Content, string(out))
	return parsed.Result.Content[0].Text, parsed.Result.IsError
}

func TestMCPToolsMirrorREST(t *testing.T) {
	fb := newFakeBackend()
	fw := newFakeWirer()
	svc := service.New(fb, jobs.NewManager(), fw, &service.WiringInfo{Namespace: "kagent"}, service.Config{AutoWire: true}, nil)
	srv := NewMCPServer(svc, "test")

	// tools/list exposes every tool.
	listReq, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	resp := srv.HandleMessage(context.Background(), listReq)
	out, _ := json.Marshal(resp)
	var listed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &listed))
	names := map[string]bool{}
	for _, tool := range listed.Result.Tools {
		names[tool.Name] = true
	}
	for _, want := range ToolNames() {
		assert.True(t, names[want], "tool %s missing", want)
	}
	assert.Len(t, listed.Result.Tools, len(ToolNames()))

	text, isErr := callTool(t, srv, ToolGetBackend, nil)
	require.False(t, isErr, text)
	var be map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &be))
	assert.Equal(t, "ollama", be["backend"])
	assert.Equal(t, true, be["capabilities"].(map[string]any)["wire"])

	text, isErr = callTool(t, srv, ToolPullModel, map[string]any{"model": "smollm2:135m"})
	require.False(t, isErr, text)
	var pulled map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &pulled))
	id := pulled["job"].(map[string]any)["id"].(string)

	var job map[string]any
	for i := 0; i < 500; i++ {
		text, isErr = callTool(t, srv, ToolGetJob, map[string]any{"id": id})
		require.False(t, isErr, text)
		require.NoError(t, json.Unmarshal([]byte(text), &job))
		if job["phase"] == "succeeded" || job["phase"] == "failed" {
			break
		}
	}
	require.Equal(t, "succeeded", job["phase"], fmt.Sprint(job))
	assert.Equal(t, "smollm2-135m", job["result"].(map[string]any)["name"])

	text, isErr = callTool(t, srv, ToolListModels, nil)
	require.False(t, isErr, text)
	assert.Contains(t, text, `"smollm2:135m"`)
	assert.Contains(t, text, `"modelConfig"`)

	text, isErr = callTool(t, srv, ToolLoadModel, map[string]any{"model": "smollm2:135m", "keepAlive": "1m"})
	require.False(t, isErr, text)
	text, isErr = callTool(t, srv, ToolListLoadedModels, nil)
	require.False(t, isErr, text)
	assert.Contains(t, text, `"smollm2:135m"`)
	text, isErr = callTool(t, srv, ToolUnloadModel, map[string]any{"model": "smollm2:135m"})
	require.False(t, isErr, text)

	text, isErr = callTool(t, srv, ToolGetModel, map[string]any{"model": "missing:1b"})
	assert.True(t, isErr)
	assert.Contains(t, text, "not_found")

	text, isErr = callTool(t, srv, ToolDeleteModel, map[string]any{"model": "smollm2:135m"})
	require.False(t, isErr, text)
	assert.Empty(t, fw.refs, "delete unwires")

	text, isErr = callTool(t, srv, ToolPullModel, map[string]any{})
	assert.True(t, isErr, text)
}
