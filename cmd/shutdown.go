package cmd

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	defaultShutdownTimeout   = 25 * time.Second
	telemetryShutdownReserve = 5 * time.Second
)

type shutdownBudgetKey struct{}

type shutdownBudget struct {
	parent             context.Context
	timeout            time.Duration
	once               sync.Once
	process, work      context.Context
	cancel, cancelWork context.CancelFunc
}

func newShutdownBudget(parent context.Context, timeout time.Duration) (*shutdownBudget, error) {
	if timeout <= telemetryShutdownReserve {
		return nil, fmt.Errorf("shutdown timeout must exceed %s", telemetryShutdownReserve)
	}
	return &shutdownBudget{parent: context.WithoutCancel(parent), timeout: timeout}, nil
}

func (b *shutdownBudget) start() {
	b.once.Do(func() {
		b.process, b.cancel = context.WithTimeout(b.parent, b.timeout)
		deadline, _ := b.process.Deadline()
		b.work, b.cancelWork = context.WithDeadline(b.process, deadline.Add(-telemetryShutdownReserve))
	})
}

func (b *shutdownBudget) Context() context.Context     { b.start(); return b.process }
func (b *shutdownBudget) WorkContext() context.Context { b.start(); return b.work }
func (b *shutdownBudget) Close()                       { b.start(); b.cancelWork(); b.cancel() }
