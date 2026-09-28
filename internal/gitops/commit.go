package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/layout"
	"github.com/giantswarm/gitops-commit/provenance"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/identity"
)

// Directory is the directory model-manager writes its files to, under the
// directory the owning Flux Kustomization builds from (gitops-commit's
// layout: its own kustomization.yaml, the parent's entry).
const Directory = "model-manager"

// The Flux objects commit mode follows to the repository that owns a
// namespace.
var (
	KustomizationGVR = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	GitRepositoryGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
	HelmReleaseGVR   = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	namespaceGVR     = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
)

// ErrAuthRequired is a commit without the person's GitHub authorization —
// or with one GitHub refused; the message names the consent that gives one.
var ErrAuthRequired = errors.New("GitHub authorization required")

// signIn is the one-time consent a commit needs.
const signIn = "connect model-manager in muster (core_auth_login server=model-manager), then call again"

// Target is an explicit location for a commit: the repository (owner/name),
// its branch and the directory model-manager's directory goes under. Empty:
// the location follows from Flux provenance.
type Target struct {
	Repository string
	Branch     string
	Path       string
}

// Empty reports whether no field is set.
func (t Target) Empty() bool { return t.Repository == "" && t.Branch == "" && t.Path == "" }

// Remote is what a commit needs of GitHub, acting as the person: the pull
// request seams and the reads of the base.
type Remote interface {
	commit.Remote
	commit.Reader
}

// RemoteFor builds the remote from the person's App user token.
type RemoteFor func(token string) (Remote, error)

// Committer lands rendered objects in the repository that owns their
// namespace, as one pull request opened as the person.
type Committer struct {
	remote RemoteFor
	dyn    func(ctx context.Context) dynamic.Interface
}

// NewCommitter builds a Committer; dyn is the Kubernetes client of the call
// (the caller's own under downstream OAuth), which reads the Flux objects.
func NewCommitter(remote RemoteFor, dyn func(ctx context.Context) dynamic.Interface) *Committer {
	return &Committer{remote: remote, dyn: dyn}
}

// Result is the pull request a commit opened — or, dry run, would open —
// in the repository that owns the target: cluster-manager's commit answer.
type Result struct {
	Repository string `json:"repository"`
	Base       string `json:"base"`
	Directory  string `json:"directory"`
	// Kustomization is the Flux Kustomization (namespace/name) that lands the
	// files after the merge, Prune its spec.prune; empty for an explicit
	// target no Kustomization was found for.
	Kustomization string `json:"kustomization,omitempty"`
	Prune         bool   `json:"prune"`
	Branch        string `json:"branch"`
	Files         []File `json:"files"`
	// PullRequest is the URL of the pull request opened as the caller, empty
	// on a dry run and when nothing changes.
	PullRequest string `json:"pullRequest,omitempty"`
	Number      int    `json:"number,omitempty"`
	// Author is the GitHub login the pull request is opened as.
	Author string `json:"author,omitempty"`
	// LiveSteps are what the merge alone does not do on the installation.
	LiveSteps []string `json:"liveSteps,omitempty"`
}

// File is one file of a commit. Content is shown on a dry run, never for a
// secret file.
type File struct {
	Path    string `json:"path"`
	Action  string `json:"action"`
	Content string `json:"content,omitempty"`
}

// Request is one commit: the objects written (all in Namespace), the
// objects removed, the Flux object applying the target as it exists (nil:
// none), the explicit target, and the pull request's text.
type Request struct {
	Namespace string
	Write     []*unstructured.Unstructured
	Remove    []*unstructured.Unstructured
	Owner     *Owner
	Target    Target
	// Verb and Subject make the branch (model-manager/<verb>-<subject>) and
	// the title; Summary opens the body; Tool names the tool in it.
	Verb, Subject, Summary, Tool string
	DryRun                       bool
}

// location is where the commit goes and how Flux lands it.
type location struct {
	provenance.Location
	kustomization string
	prune         bool
}

func (l location) directory() layout.Directory {
	return layout.Directory{Location: l.Location, Name: Directory}
}

// Commit plans the request against the base branch and opens the pull
// request as the caller (or, dry run, answers what it would open). Every
// refusal is decided before anything is written.
func (c *Committer) Commit(ctx context.Context, req Request) (*Result, error) {
	gh, ok := identity.GitHubFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: commit mode opens the pull request with your GitHub authorization, and this call carries none: %s", ErrAuthRequired, signIn)
	}
	loc, err := c.locate(ctx, req)
	if err != nil {
		return nil, err
	}
	remote, err := c.remote(gh.Token)
	if err != nil {
		return nil, commitError(err)
	}
	dir := loc.directory()
	write, err := files(dir, req.Write)
	if err != nil {
		return nil, err
	}
	var remove []string
	for _, obj := range req.Remove {
		remove = append(remove, fileOf(dir, obj))
	}
	if err := c.inDirectory(ctx, remote, loc, req); err != nil {
		return nil, err
	}
	plan, err := layout.Build(ctx, remote, dir, write, remove)
	var secret *layout.SecretError
	if errors.As(err, &secret) {
		return nil, fmt.Errorf("%w: %s holds the placeholder API key, and %s: model-manager commits a Secret only encrypted for the repository's age recipients — pass apiKeySecret (an existing Secret) or apiKeyPassthrough, give the path an age creation rule, or use mode apply", backend.ErrInvalid, strings.Join(secret.Paths, ", "), secret.Reason)
	}
	if err != nil {
		return nil, commitError(err)
	}
	branch := "model-manager/" + req.Verb + "-" + req.Subject
	out := &Result{
		Repository: loc.Repository.String(), Base: loc.Branch, Directory: dir.Path(),
		Kustomization: loc.kustomization, Prune: loc.prune, Branch: branch, Author: gh.Login,
	}
	for _, f := range plan.Files {
		file := File{Path: f.Path, Action: string(f.Action)}
		if req.DryRun {
			file.Content = string(f.Content)
		}
		out.Files = append(out.Files, file)
	}
	if len(req.Remove) > 0 && !loc.prune && loc.kustomization != "" {
		out.LiveSteps = append(out.LiveSteps, fmt.Sprintf("Flux does not prune Kustomization %s (spec.prune false): after the merge the objects stay on the installation — delete them by hand", loc.kustomization))
	}
	if req.DryRun || !plan.Changed() {
		return out, nil
	}
	title := fmt.Sprintf("feat(model-manager): %s %s", req.Verb, req.Subject)
	prs, err := commit.Open(ctx, remote, commit.Request{Branch: branch, Title: title, Body: body(req, gh.Login, out)}, []commit.Change{plan.Change()})
	if err != nil {
		return nil, commitError(err)
	}
	out.PullRequest, out.Number = prs[0].URL, prs[0].Number
	return out, nil
}

// inDirectory refuses a write on an object Flux applies from a file outside
// model-manager's directory (a second file would make the Kustomization's
// build fail on the duplicate), and a removal of an object that is not in
// the directory at all (it was written live: removed live).
func (c *Committer) inDirectory(ctx context.Context, remote Remote, loc location, req Request) error {
	check := func(obj *unstructured.Unstructured, removal bool) error {
		p := fileOf(loc.directory(), obj)
		raw, err := remote.ReadFile(ctx, loc.Repository, loc.Branch, p)
		switch {
		case err == nil && raw != nil:
			return nil
		case err != nil && !errors.Is(err, commit.ErrFileNotFound):
			return commitError(err)
		case removal:
			return fmt.Errorf("%w: %s %s/%s is not in %s (no %s on %s): it was written live — remove it with mode apply", backend.ErrConflict, obj.GetKind(), obj.GetNamespace(), obj.GetName(), loc.Repository, p, loc.Branch)
		case obj.GetKind() == req.firstKind() && req.Owner != nil:
			return fmt.Errorf("%w: %s %s/%s is applied from git by %s, but not from %s on %s: change it in the file it is written in", backend.ErrConflict, obj.GetKind(), obj.GetNamespace(), obj.GetName(), req.Owner, p, loc.Branch)
		}
		return nil
	}
	for _, obj := range req.Remove {
		if obj.GetKind() == "Secret" {
			continue // the placeholder follows its ModelConfig
		}
		if err := check(obj, true); err != nil {
			return err
		}
	}
	if len(req.Write) > 0 {
		return check(req.Write[0], false)
	}
	return nil
}

// firstKind is the kind of the object the request is about: its first.
func (r Request) firstKind() string {
	for _, list := range [][]*unstructured.Unstructured{r.Write, r.Remove} {
		if len(list) > 0 {
			return list[0].GetKind()
		}
	}
	return ""
}

// locate is the commit's location: the explicit target, else the Flux
// provenance of the target — its own owner, else the namespace's.
func (c *Committer) locate(ctx context.Context, req Request) (location, error) {
	if req.Owner != nil && req.Owner.Kind == KindHelmRelease {
		return location{}, fmt.Errorf("%w: the object is rendered by %s: change the release's values in git", backend.ErrConflict, req.Owner)
	}
	if !req.Target.Empty() {
		if req.Target.Repository == "" || req.Target.Branch == "" {
			return location{}, fmt.Errorf("%w: an explicit commit target needs repository (owner/name) and branch; path defaults to the repository root", backend.ErrInvalid)
		}
		loc, err := provenance.Explicit(req.Target.Repository, req.Target.Branch, req.Target.Path)
		if err != nil {
			return location{}, fmt.Errorf("%w: %v", backend.ErrInvalid, err)
		}
		return location{Location: loc}, nil
	}
	owner := req.Owner
	if owner == nil {
		var err error
		if owner, err = c.namespaceOwner(ctx, req.Namespace); err != nil {
			return location{}, err
		}
	}
	return c.resolve(ctx, owner, req)
}

// namespaceOwner is the Kustomization the namespace's own object carries,
// through the HelmRelease that renders it where a chart creates it.
func (c *Committer) namespaceOwner(ctx context.Context, namespace string) (*Owner, error) {
	ns, err := c.dyn(ctx).Resource(namespaceGVR).Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read namespace %s for its Flux provenance: %w", namespace, err)
	}
	owner := OwnerOf(ns.GetLabels())
	if owner == nil {
		return nil, fmt.Errorf("%w: namespace %s is not applied by Flux (no %s label): commit mode needs the repository — pass repository, branch and path, or use mode apply", backend.ErrInvalid, namespace, LabelKustomizeName)
	}
	return owner, nil
}

// resolve follows owner to its GitRepository and path: a HelmRelease
// through the Kustomization that applies it, a Kustomization whose
// targetNamespace would move the objects out of namespace refused.
func (c *Committer) resolve(ctx context.Context, owner *Owner, req Request) (location, error) {
	dyn := c.dyn(ctx)
	if owner.Kind == KindHelmRelease {
		hr, err := dyn.Resource(HelmReleaseGVR).Namespace(owner.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil {
			return location{}, fmt.Errorf("get %s: %w", owner, err)
		}
		next := OwnerOf(hr.GetLabels())
		if next == nil || next.Kind != KindKustomization {
			return location{}, fmt.Errorf("%w: %s is not applied by a Flux Kustomization: commit mode needs the repository — pass repository, branch and path", backend.ErrInvalid, owner)
		}
		owner = next
	}
	ks, err := dyn.Resource(KustomizationGVR).Namespace(owner.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil {
		return location{}, fmt.Errorf("get %s: %w", owner, err)
	}
	if target := nested(ks, "spec", "targetNamespace"); target != "" {
		for _, obj := range append(slices.Clone(req.Write), req.Remove...) {
			if obj.GetNamespace() != target {
				return location{}, fmt.Errorf("%w: %s sets targetNamespace %s, which would move %s %s out of %s: pass repository, branch and path", backend.ErrInvalid, owner, target, obj.GetKind(), obj.GetName(), obj.GetNamespace())
			}
		}
	}
	src := provenance.SourceRef{Kind: nested(ks, "spec", "sourceRef", "kind"), Name: nested(ks, "spec", "sourceRef", "name"), Namespace: nested(ks, "spec", "sourceRef", "namespace")}
	flux := provenance.Flux{Kustomizations: []provenance.Kustomization{{Name: owner.Name, Namespace: owner.Namespace, SourceRef: src, Path: nested(ks, "spec", "path")}}}
	if src.Kind == provenance.KindGitRepository {
		srcNS := src.Namespace
		if srcNS == "" {
			srcNS = owner.Namespace
		}
		repo, err := dyn.Resource(GitRepositoryGVR).Namespace(srcNS).Get(ctx, src.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return location{}, fmt.Errorf("get GitRepository %s/%s: %w", srcNS, src.Name, err)
		}
		if err == nil {
			flux.GitRepositories = []provenance.GitRepository{{Name: src.Name, Namespace: srcNS, URL: nested(repo, "spec", "url"), Branch: nested(repo, "spec", "ref", "branch")}}
		}
	}
	loc, err := flux.Resolve(owner.Namespace, owner.Name)
	if err != nil {
		return location{}, fmt.Errorf("%w: %s: %v — pass repository, branch and path", backend.ErrInvalid, owner, err)
	}
	prune, _, _ := unstructured.NestedBool(ks.Object, "spec", "prune")
	return location{Location: loc, kustomization: owner.Namespace + "/" + owner.Name, prune: prune}, nil
}

// fileOf is an object's file in the directory: named after a ModelConfig
// (and a Secret, in its secret file), after any other object with its kind
// appended — a serving object and the ModelConfig wiring it share a name.
func fileOf(dir layout.Directory, obj *unstructured.Unstructured) string {
	switch kind := obj.GetKind(); kind {
	case "ModelConfig", "Secret":
		return dir.ObjectFile(kind, obj.GetName())
	default:
		return dir.ObjectFile(kind, obj.GetName()+"-"+strings.ToLower(kind))
	}
}

// files renders the objects as the directory's files: one per object, a
// Secret in a secret file of its own (encrypted by the layout).
func files(dir layout.Directory, objs []*unstructured.Unstructured) (map[string][]byte, error) {
	out := make(map[string][]byte, len(objs))
	for _, obj := range objs {
		raw, err := sigsyaml.Marshal(obj.Object)
		if err != nil {
			return nil, fmt.Errorf("render %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
		out[fileOf(dir, obj)] = raw
	}
	return out, nil
}

// body is the pull request's description: what it does, who asked through
// which tool, the files, and what lands them.
func body(req Request, login string, res *Result) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s.\n\nOpened by @%s through model-manager (`%s`, mode commit).\n\nFiles:\n\n", req.Summary, login, req.Tool)
	for _, f := range res.Files {
		if f.Action != string(layout.Unchanged) {
			fmt.Fprintf(&b, "- %s `%s`\n", f.Action, f.Path)
		}
	}
	if res.Kustomization != "" {
		fmt.Fprintf(&b, "\nFlux Kustomization `%s` applies the change after the merge.\n", res.Kustomization)
	}
	return b.String()
}

// commitError answers a token GitHub refused with the consent to renew.
func commitError(err error) error {
	var auth *commit.AuthError
	if errors.As(err, &auth) {
		return fmt.Errorf("%w: GitHub refused your token on %s (status %d): reconnect model-manager in muster (core_auth_login server=model-manager) — the App must be installed on the repository and your account must be allowed to push to it", ErrAuthRequired, auth.Op, auth.Status)
	}
	return err
}

func nested(obj *unstructured.Unstructured, fields ...string) string {
	v, _, _ := unstructured.NestedString(obj.Object, fields...)
	return v
}
