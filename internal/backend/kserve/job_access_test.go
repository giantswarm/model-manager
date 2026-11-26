package kserve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	toolkitk8s "github.com/giantswarm/mcp-toolkit/k8s"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/requester"
)

func TestJobAccessUsesCurrentCallerAndResourceIdentity(t *testing.T) {
	var mode atomic.Value
	mode.Store("allowed")
	var applicationRequests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.Contains(request.Header.Get("Authorization"), "application-credential") {
			applicationRequests.Add(1)
		}
		if request.Header.Get("Authorization") != "Bearer fresh-caller" {
			writer.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(writer).Encode(metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonUnauthorized, Code: 401})
			return
		}
		if mode.Load() == "forbidden" {
			writer.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(writer).Encode(metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403})
			return
		}
		if mode.Load() == "removed" {
			writer.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(writer).Encode(metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: 404})
			return
		}
		uid := "original"
		if mode.Load() == "replacement" {
			uid = "new-object"
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]string{"namespace": "tenant", "name": "download", "uid": uid}})
	}))
	t.Cleanup(api.Close)
	users, err := toolkitk8s.NewUserClients(&rest.Config{Host: api.URL, BearerToken: "application-credential"}, nil)
	require.NoError(t, err)
	driver := &Backend{opts: backend.KServeOptions{ClientsFor: func(ctx context.Context) (kubernetes.Interface, dynamic.Interface, error) {
		token, _ := requester.IDTokenFromContext(ctx)
		clients, err := users.ForToken(token)
		if err != nil {
			return nil, nil, err
		}
		return clients.Clientset, clients.Dynamic, nil
	}}}
	object := &backend.JobObject{Group: "batch", Version: "v1", Resource: "jobs", Namespace: "tenant", Name: "download", UID: "original"}
	caller := requester.ContextWithIDToken(t.Context(), "fresh-caller")
	require.NoError(t, driver.CheckJobAccess(caller, object))
	mode.Store("forbidden")
	require.Error(t, driver.CheckJobAccess(caller, object))
	mode.Store("allowed")
	require.Error(t, driver.CheckJobAccess(requester.ContextWithIDToken(t.Context(), "expired-caller"), object))
	require.Error(t, driver.CheckJobAccess(t.Context(), object))
	mode.Store("replacement")
	require.ErrorIs(t, driver.CheckJobAccess(caller, object), backend.ErrNotFound)
	mode.Store("removed")
	require.NoError(t, driver.CheckJobAccess(caller, object))
	require.Zero(t, applicationRequests.Load())
}

func TestServingTaskAccessUsesTheOriginalNamespaceNameAndUID(t *testing.T) {
	var replacement atomic.Bool
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get("Authorization") != "Bearer current-caller" || request.URL.Path != "/apis/serving.kserve.io/v1alpha2/namespaces/tenant/llminferenceservices/served-model" {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		uid := "original"
		if replacement.Load() {
			uid = "replacement"
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"apiVersion": "serving.kserve.io/v1alpha2", "kind": "LLMInferenceService", "metadata": map[string]string{"name": "served-model", "namespace": "tenant", "uid": uid}})
	}))
	t.Cleanup(api.Close)
	users, err := toolkitk8s.NewUserClients(&rest.Config{Host: api.URL, BearerToken: "application-credential"}, nil)
	require.NoError(t, err)
	driver := &Backend{opts: backend.KServeOptions{ClientsFor: func(ctx context.Context) (kubernetes.Interface, dynamic.Interface, error) {
		credential, _ := requester.IDTokenFromContext(ctx)
		clients, err := users.ForToken(credential)
		if err != nil {
			return nil, nil, err
		}
		return clients.Clientset, clients.Dynamic, nil
	}}}
	object := &backend.JobObject{Group: llmisvcGVR.Group, Version: llmisvcGVR.Version, Resource: llmisvcGVR.Resource, Namespace: "tenant", Name: "served-model", UID: "original"}
	caller := requester.ContextWithIDToken(t.Context(), "current-caller")
	require.NoError(t, driver.CheckJobAccess(caller, object))
	replacement.Store(true)
	require.ErrorIs(t, driver.CheckJobAccess(caller, object), backend.ErrNotFound)
	require.EqualValues(t, 2, requests.Load())
	object.Resource = "secrets"
	require.ErrorIs(t, driver.CheckJobAccess(caller, object), backend.ErrForbidden)
	require.EqualValues(t, 2, requests.Load())
}
