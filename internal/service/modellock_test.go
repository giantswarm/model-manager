package service

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
)

func TestModelLocksSerialisesOneModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var l modelLocks
		unlock, err := l.lock(t.Context(), backend.NameKServe, "org/Tiny")
		require.NoError(t, err)

		other, err := l.lock(t.Context(), backend.NameKServe, "org/big")
		require.NoError(t, err, "another model is not blocked")
		other()

		acquired := make(chan func(), 1)
		go func() {
			u, err := l.lock(t.Context(), backend.NameKServe, "org/tiny")
			if err == nil {
				acquired <- u
			}
		}()
		synctest.Wait()
		assert.Empty(t, acquired, "the same model, in any case, waits")

		unlock()
		synctest.Wait()
		require.Len(t, acquired, 1)
		(<-acquired)()
		assert.Empty(t, l.locks, "a lock nobody holds or waits for is dropped")
	})
}

func TestModelLocksWaitEndsWithTheContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var l modelLocks
		unlock, err := l.lock(t.Context(), backend.NameOllama, "qwen3:0.6b")
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		failed := make(chan error, 1)
		go func() {
			_, err := l.lock(ctx, backend.NameOllama, "qwen3:0.6b")
			failed <- err
		}()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-failed, context.Canceled)
		unlock()
		assert.Empty(t, l.locks)
	})
}
