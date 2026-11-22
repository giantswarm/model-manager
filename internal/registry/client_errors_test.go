package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"

	"github.com/giantswarm/model-manager/internal/backend"
)

func TestStoreClientSelectionErrors(t *testing.T) {
	selectionError := errors.New("caller client construction failed")
	store := NewStore(func(context.Context) (kubernetes.Interface, error) { return nil, selectionError }, "default")
	_, err := store.Get(t.Context(), backend.NameOllama)
	require.ErrorIs(t, err, selectionError)
	_, err = store.Remove(t.Context(), backend.NameOllama)
	require.ErrorIs(t, err, selectionError)
	document, err := backend.ParseDocument([]byte("apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nmetadata:\n  name: ollama\nspec:\n  kind: ollama\n  endpoint: http://localhost:11434\n"))
	require.NoError(t, err)
	_, err = store.Apply(t.Context(), document)
	require.ErrorIs(t, err, selectionError)
}
