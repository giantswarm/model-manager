// Package registry loads backend documents — ConfigMaps in model-manager's
// namespace carrying backend.DocumentLabel — into the service as they
// appear, change and go, and writes them for add_backend / remove_backend.
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/service"
)

// Builder constructs the backend a valid document describes.
type Builder func(doc *backend.Document) (backend.Backend, error)

// Registry watches the documents of one namespace with the ServiceAccount —
// the one background read model-manager keeps under downstream OAuth, since
// its own configuration is not per-caller data — and keeps the service's
// backend set in step. A document failing the schema is reported and not
// loaded, and a loaded one that breaks drops its backend in the same step
// (service.DeregisterDocument); the process keeps running.
type Registry struct {
	client    kubernetes.Interface
	namespace string
	build     Builder
	svc       *service.Service
	log       *slog.Logger

	mu    sync.Mutex
	names map[string]backend.Name // ConfigMap name -> the backend it registered
}

// New builds a Registry over client for the documents in namespace.
func New(client kubernetes.Interface, namespace string, build Builder, svc *service.Service, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{client: client, namespace: namespace, build: build, svc: svc, log: log.With("component", "registry"), names: map[string]backend.Name{}}
}

// Run watches until ctx is done. It returns once the informer has synced
// nothing yet only on a ctx cancellation; a watch that cannot start (RBAC)
// is logged and retried by the informer.
func (r *Registry) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(r.client, 0,
		informers.WithNamespace(r.namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = backend.DocumentSelector }),
	)
	inf := factory.Core().V1().ConfigMaps().Informer()
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { r.upsert(obj) },
		UpdateFunc: func(_, obj any) { r.upsert(obj) },
		DeleteFunc: func(obj any) { r.delete(obj) },
	}); err != nil {
		return fmt.Errorf("backend document informer: %w", err)
	}
	if err := inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		r.log.Warn("watching backend documents failed; retrying (the ServiceAccount needs get/list/watch on ConfigMaps in its namespace)", "namespace", r.namespace, "error", err)
	}); err != nil {
		return fmt.Errorf("backend document informer: %w", err)
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		return ctx.Err()
	}
	r.log.Info("watching backend documents", "namespace", r.namespace, "selector", backend.DocumentSelector, "backends", r.svc.Names())
	<-ctx.Done()
	return nil
}

func (r *Registry) upsert(obj any) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, source, err := r.load(cm)
	if err == nil {
		// Registered and no longer reported in one step: a reader that finds
		// the backend must not find its document listed as invalid too
		// (giantswarm/model-manager#101).
		err = r.svc.RegisterDocument(b, source, cm.Name)
	}
	if err != nil {
		r.log.Warn("backend document not loaded", "configMap", cm.Name, "error", err)
		// Reported and no longer registered in one step, the mirror: a broken
		// document drops what its ConfigMap registered, whichever way it
		// broke, rather than leaving the last good backend serving beside the
		// report.
		r.forgetLocked(cm.Name, err.Error())
		return
	}
	// A ConfigMap's name fixes its backend's name (load refuses any other),
	// so what it registered before is the backend just replaced.
	r.names[cm.Name] = b.Name()
	r.log.Info("backend registered", "backend", b.Name(), "source", source, "configMap", cm.Name)
}

// load parses and builds cm's document without registering it; it returns
// the built backend with its source for the caller to register.
func (r *Registry) load(cm *corev1.ConfigMap) (b backend.Backend, source string, err error) {
	raw, ok := cm.Data[backend.DocumentKey]
	if !ok {
		return nil, "", fmt.Errorf("no %s key", backend.DocumentKey)
	}
	doc, err := backend.ParseDocument([]byte(raw))
	if err != nil {
		return nil, "", err
	}
	name := doc.Name()
	if cm.Name != backend.DocumentName(name) {
		return nil, "", fmt.Errorf("metadata.name: the ConfigMap of the %s document is named %s", name, backend.DocumentName(name))
	}
	b, err = r.build(doc)
	if err != nil {
		return nil, "", fmt.Errorf("build %s backend: %w", name, err)
	}
	return b, doc.Spec.Source, nil
}

func (r *Registry) delete(obj any) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		if t, isTomb := obj.(cache.DeletedFinalStateUnknown); isTomb {
			cm, ok = t.Obj.(*corev1.ConfigMap)
		}
		if !ok {
			return
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgetLocked(cm.Name, "")
}

// forgetLocked drops the backend configMap registered, if any, and records
// problem as its report (an empty problem clears it) in one step.
func (r *Registry) forgetLocked(configMap, problem string) {
	name := r.names[configMap]
	delete(r.names, configMap)
	if r.svc.DeregisterDocument(name, configMap, problem) {
		r.log.Info("backend removed", "backend", name, "configMap", configMap)
	}
}

// Store writes backend documents as the caller: ClientFor returns the
// clients a request should use (the caller's own under downstream OAuth).
type Store struct {
	clientFor func(ctx context.Context) kubernetes.Interface
	namespace string
}

// NewStore builds a Store over the documents in namespace.
func NewStore(clientFor func(ctx context.Context) kubernetes.Interface, namespace string) *Store {
	return &Store{clientFor: clientFor, namespace: namespace}
}

// Namespace is where the documents live.
func (s *Store) Namespace() string { return s.namespace }

// Get reads the document of the backend name; absent is backend.ErrNotFound.
func (s *Store) Get(ctx context.Context, name backend.Name) (*backend.Document, error) {
	cm, err := s.clientFor(ctx).CoreV1().ConfigMaps(s.namespace).Get(ctx, backend.DocumentName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: no %s backend document in %s", backend.ErrNotFound, name, s.namespace)
	}
	if err != nil {
		return nil, err
	}
	raw, ok := cm.Data[backend.DocumentKey]
	if !ok {
		return nil, fmt.Errorf("%w: %s has no %s key", backend.ErrInvalid, cm.Name, backend.DocumentKey)
	}
	return backend.ParseDocument([]byte(raw))
}

// Apply writes doc as its ConfigMap, creating or replacing it. It reports
// whether the document was created (else updated).
func (s *Store) Apply(ctx context.Context, doc *backend.Document) (created bool, err error) {
	if err := doc.Validate(); err != nil {
		return false, fmt.Errorf("%w: %v", backend.ErrInvalid, err)
	}
	cm, err := doc.ConfigMap(s.namespace)
	if err != nil {
		return false, err
	}
	cms := s.clientFor(ctx).CoreV1().ConfigMaps(s.namespace)
	existing, err := cms.Get(ctx, cm.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = cms.Create(ctx, cm, metav1.CreateOptions{})
		return true, err
	case err != nil:
		return false, err
	}
	existing.Labels = cm.Labels
	existing.Data = cm.Data
	_, err = cms.Update(ctx, existing, metav1.UpdateOptions{})
	return false, err
}

// Remove deletes the document of the backend name; it reports whether one
// existed.
func (s *Store) Remove(ctx context.Context, name backend.Name) (bool, error) {
	err := s.clientFor(ctx).CoreV1().ConfigMaps(s.namespace).Delete(ctx, backend.DocumentName(name), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// WaitFor blocks until the service's view of the backend name matches present (the
// informer has delivered the write) or the timeout passes; it is what makes
// add_backend / remove_backend answer with the resulting state.
func WaitFor(ctx context.Context, svc *service.Service, name backend.Name, present bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		_, has := svc.Has(name)
		if has == present {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
