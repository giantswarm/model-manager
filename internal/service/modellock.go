package service

import (
	"context"
	"strings"
	"sync"

	"github.com/giantswarm/model-manager/internal/backend"
)

// modelLocks holds one lock per backend and model, created on first use and
// dropped once nobody holds or waits for it. The zero value is ready to use.
type modelLocks struct {
	mu    sync.Mutex
	locks map[string]*modelLock
}

type modelLock struct {
	ch   chan struct{}
	refs int
}

// lock blocks until it holds the lock for model on b, or ctx ends. The
// model reference is compared case-insensitively, as the backends match it.
func (l *modelLocks) lock(ctx context.Context, b backend.Name, model string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := string(b) + "\x00" + strings.ToLower(model)
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*modelLock{}
	}
	ml, ok := l.locks[key]
	if !ok {
		ml = &modelLock{ch: make(chan struct{}, 1)}
		l.locks[key] = ml
	}
	ml.refs++
	l.mu.Unlock()

	select {
	case ml.ch <- struct{}{}:
		return func() {
			<-ml.ch
			l.release(key, ml)
		}, nil
	case <-ctx.Done():
		l.release(key, ml)
		return nil, ctx.Err()
	}
}

func (l *modelLocks) release(key string, ml *modelLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ml.refs--
	if ml.refs == 0 {
		delete(l.locks, key)
	}
}
