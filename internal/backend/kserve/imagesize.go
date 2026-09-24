package kserve

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// modelImagePlatform is the platform a model image's size is read for. The
// platforms of a model image share every layer but the base (a busybox shell
// for KServe's modelcar sidecar), so the one read stands for all of them.
var modelImagePlatform = v1.Platform{OS: "linux", Architecture: "amd64"}

// modelImageBytes is what containerd pulls onto a node for a preset served
// from a model image (giantswarm/model-manager#150): the sum of the layers of
// the image the oci:// storage URI names, read anonymously from its registry —
// the index and one manifest, no layer. A model image is its checkpoint in
// uncompressed tar layers, so the sum is the weights and the files beside
// them as the image carries them, not the Hub repository's whole tree.
func modelImageBytes(ctx context.Context, storageURI string) (int64, error) {
	ref, err := name.ParseReference(strings.TrimPrefix(storageURI, "oci://"))
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", storageURI, err)
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous), remote.WithPlatform(modelImagePlatform))
	if err != nil {
		return 0, fmt.Errorf("read the manifest of %s: %w", ref, err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return 0, fmt.Errorf("read the manifest of %s: %w", ref, err)
	}
	var total int64
	for _, l := range manifest.Layers {
		total += l.Size
	}
	return total, nil
}
