package service

import (
	"context"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/jobs"
)

func jobAuthorizer(driver backend.Backend) func(context.Context, jobs.Job) error {
	access, ok := driver.(backend.JobAccess)
	if !ok {
		return nil
	}
	return func(ctx context.Context, job jobs.Job) error { return access.CheckJobAccess(ctx, job.Object) }
}
