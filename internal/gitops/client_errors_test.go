package gitops

import (
	"context"
	"errors"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

func TestCommitClientSelectionErrors(t *testing.T) {
	selectionError := errors.New("caller client construction failed")
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			fake := commit.NewFake()
			committer := committer(cluster(), fake)
			selectClient := func(context.Context) (dynamic.Interface, error) { return nil, selectionError }
			request := Request{Namespace: "kagent", Write: []*unstructured.Unstructured{modelConfig("model")}, Verb: "wire", Subject: "model"}
			if remote {
				request.Cluster = &Cluster{Name: "workload", Dynamic: selectClient}
			} else {
				committer.dyn = selectClient
			}
			_, err := committer.Commit(asPerson(), request)
			require.ErrorIs(t, err, selectionError)
			require.Empty(t, fake.PullRequests())
		})
	}
}
