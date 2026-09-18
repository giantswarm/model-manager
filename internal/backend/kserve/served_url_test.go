package kserve

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// An LLMInferenceService without a models Gateway in discovery is served to
// agents at its workload Service, whatever route KServe published as
// status.url; with the Gateway the published address stands.
func TestServedURLWithoutGatewayUsesWorkloadService(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"url": "http://inference.example.com/model-serving/qwen",
			"addresses": []any{
				map[string]any{"url": "http://inference.example.com/model-serving/qwen"},
			},
		},
	}}
	sv := served{Namespace: "model-serving", Name: "qwen"}

	if got, want := servedURL(obj, sv, ""), workloadURL("qwen", "model-serving"); got != want {
		t.Fatalf("without a Gateway: got %q, want the workload Service %q", got, want)
	}
	if got, want := servedURL(obj, sv, "https://models.example.com"), "http://inference.example.com/model-serving/qwen"; got != want {
		t.Fatalf("with a Gateway: got %q, want the published address %q", got, want)
	}
	local := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"url": "https://qwen-kserve-workload-svc.model-serving.svc.cluster.local"},
	}}
	if got, want := servedURL(local, sv, ""), "https://qwen-kserve-workload-svc.model-serving.svc.cluster.local"; got != want {
		t.Fatalf("a published cluster-local address stands: got %q, want %q", got, want)
	}
}
