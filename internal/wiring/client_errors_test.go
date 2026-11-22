package wiring

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/model-manager/internal/backend"
)

func TestClientSelectionFailureStopsWiring(t *testing.T) {
	selectionError := errors.New("caller client construction failed")
	wirer, base := newFakeKagent(t)
	wirer.WithClientFor(func(context.Context) (dynamic.Interface, error) { return nil, selectionError })
	_, err := wirer.List(t.Context())
	require.ErrorIs(t, err, selectionError)
	_, err = wirer.ListAll(t.Context())
	require.ErrorIs(t, err, selectionError)
	_, err = wirer.Render(t.Context(), "model", ollamaEndpoint("model"))
	require.ErrorIs(t, err, selectionError)
	_, err = wirer.Lookup(t.Context(), backend.NameOllama, "model")
	require.ErrorIs(t, err, selectionError)
	require.Empty(t, base.Actions())
}
