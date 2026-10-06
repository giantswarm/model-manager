package kserve

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// modelImagePlatform is the platform a model image's size is read for. The
// platforms of a model image share every layer but the base (a busybox shell
// for KServe's modelcar sidecar), so the one read stands for all of them.
var modelImagePlatform = v1.Platform{OS: "linux", Architecture: "amd64"}

const (
	// labelModelImageWeights is the label giantswarm/models stamps on a model
	// image: the bytes of its weight files.
	labelModelImageWeights = "io.giantswarm.models.weights.bytes"
	// modelImageConfigPath is the checkpoint's config.json inside a model
	// image: KServe's modelcar contract puts the checkpoint under /models.
	modelImageConfigPath = "models/config.json"
	// modelImageSmallFile bounds the files read past in search of
	// config.json: a model image's tar layers carry the checkpoint's small
	// files ahead of the weights (a layer of their own, or the head of the
	// first weights layer), so the search stops a layer at its first file
	// larger than this and no weight file is ever fetched.
	modelImageSmallFile = 16 << 20
)

// registryRequestTimeout bounds each step of one registry request — the
// connection, the TLS handshake, the response headers — so a request the
// registry leaves hanging fails in time to be retried within a fit check's
// budget (the hub's, a few seconds) instead of spending all of it
// (giantswarm/model-manager#249). A variable for the tests.
var registryRequestTimeout = 1500 * time.Millisecond

// registryBackoff spaces the retries of a failed registry request: short,
// since the fit check's budget is seconds.
var registryBackoff = remote.Backoff{Duration: 100 * time.Millisecond, Factor: 2, Steps: 3}

// registryTransport is the transport model images are read through: the
// default one with every step of a request bounded (registryRequestTimeout).
func registryTransport() http.RoundTripper {
	t := remote.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: registryRequestTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = registryRequestTimeout
	t.ResponseHeaderTimeout = registryRequestTimeout
	return t
}

// retryRegistry says which failed registry requests are retried: a timeout
// of one request — the registry's ping (/v2/) included — and a connection
// the registry dropped.
func retryRegistry(err error) bool {
	var ne net.Error
	return (errors.As(err, &ne) && ne.Timeout()) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
}

// describeRegistryFailure words a failed model image read: a registry that
// did not answer within the budget — its timed-out requests retried
// (retryRegistry) — is named as such; every other error is given as it is.
// The registry client joins the errors of its ping (https, then http) into
// one that does not unwrap, so a timeout is also recognised by its message.
func describeRegistryFailure(err error, budget time.Duration) string {
	msg := err.Error()
	if ne := net.Error(nil); (errors.As(err, &ne) && ne.Timeout()) || strings.Contains(msg, "timeout") || strings.Contains(msg, context.DeadlineExceeded.Error()) {
		return fmt.Sprintf("the registry did not answer within %s, timed-out requests retried (%s)", budget, msg)
	}
	return msg
}

// modelImage is what a preset served from a model image says about itself,
// read from its registry — never from the Hugging Face Hub, which is not
// where its weights come from (giantswarm/model-manager#189).
type modelImage struct {
	// Bytes is what containerd pulls onto a node: the sum of the layers.
	Bytes int64
	// WeightsBytes is the image's weights label; 0 when it carries none.
	WeightsBytes int64
	// Config is the checkpoint's config.json; nil when no small layer holds it.
	Config []byte
}

// modelImage is readModelImage, remembered per storage URI once read: a
// model image's reference names its content (a revision tag or a digest),
// so a fit check reads the registry once per image, not on every call.
func (b *Backend) modelImage(ctx context.Context, storageURI string) (modelImage, error) {
	b.imageMu.Lock()
	img, ok := b.images[storageURI]
	b.imageMu.Unlock()
	if ok {
		return img, nil
	}
	img, err := readModelImage(ctx, storageURI)
	if err != nil {
		return img, err
	}
	b.imageMu.Lock()
	defer b.imageMu.Unlock()
	if b.images == nil {
		b.images = map[string]modelImage{}
	}
	b.images[storageURI] = img
	return img, nil
}

// readModelImage reads a model image anonymously from its registry: the
// manifest of the one platform (giantswarm/model-manager#150), the config
// blob for the labels, and the head of each layer until config.json turns up
// — streamed, stopped at the first weight file (fileInLayer). An error reading
// config.json leaves Config nil: the size and the weights stand on their own.
func readModelImage(ctx context.Context, storageURI string) (modelImage, error) {
	ref, err := name.ParseReference(strings.TrimPrefix(storageURI, "oci://"))
	if err != nil {
		return modelImage{}, fmt.Errorf("parse %s: %w", storageURI, err)
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous), remote.WithPlatform(modelImagePlatform),
		remote.WithTransport(registryTransport()), remote.WithRetryPredicate(retryRegistry), remote.WithRetryBackoff(registryBackoff))
	if err != nil {
		return modelImage{}, fmt.Errorf("read the manifest of %s: %w", ref, err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return modelImage{}, fmt.Errorf("read the manifest of %s: %w", ref, err)
	}
	var out modelImage
	for _, l := range manifest.Layers {
		out.Bytes += l.Size
	}
	if cfg, err := img.ConfigFile(); err == nil {
		if v, err := strconv.ParseInt(cfg.Config.Labels[labelModelImageWeights], 10, 64); err == nil && v > 0 {
			out.WeightsBytes = v
		}
	}
	layers, err := img.Layers()
	if err != nil {
		return out, nil
	}
	for _, l := range layers {
		if raw, err := fileInLayer(l, modelImageConfigPath); err == nil && raw != nil {
			out.Config = raw
			break
		}
	}
	return out, nil
}

// fileInLayer returns the file at want in the layer's tar, nil when the
// layer does not hold it ahead of its first file larger than
// modelImageSmallFile: the stream is closed there, so what is read is the
// tar headers and the small files before it.
func fileInLayer(l v1.Layer, want string) ([]byte, error) {
	rc, err := l.Uncompressed()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Size > modelImageSmallFile {
			return nil, nil
		}
		if h.Typeflag == tar.TypeReg && strings.TrimPrefix(path.Clean("/"+h.Name), "/") == want {
			return io.ReadAll(tr)
		}
	}
}

// imageOnNode reports whether the node's kubelet lists the image the storage
// URI names among the images it holds (status.images): the reference as
// written, or the same repository at the same tag or digest.
func imageOnNode(storageURI string, images []string) bool {
	want := strings.TrimPrefix(storageURI, "oci://")
	wantRef, err := name.ParseReference(want)
	for _, img := range images {
		if img == want {
			return true
		}
		if err != nil {
			continue
		}
		got, err := name.ParseReference(img)
		if err == nil && got.Name() == wantRef.Name() {
			return true
		}
	}
	return false
}
