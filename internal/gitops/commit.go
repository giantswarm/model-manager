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
	// Cluster is the remote cluster the files land on, empty for
	// model-manager's own.
	Cluster string `json:"cluster,omitempty"`
	// Also are the request's further parts, each in its own location — a
	// remote target's ModelConfig on model-manager's own cluster; a part in
	// another repository has its own pull request.
	Also []Result `json:"also,omitempty"`
}

// File is one file of a commit. Content is shown on a dry run, never for a
// secret file.
type File struct {
	Path    string `json:"path"`
	Action  string `json:"action"`
	Content string `json:"content,omitempty"`
}

// Request is one commit: its first part (the objects written, all in
// Namespace, the objects removed, the Flux object applying the target as it
// exists — nil: none — and the remote cluster they land on), the explicit
// target of that part, the further parts, and the pull request's text.
type Request struct {
	Namespace string
	Write     []*unstructured.Unstructured
	Remove    []*unstructured.Unstructured
	Owner     *Owner
	// Cluster is the remote cluster the first part lands on; nil is
	// model-manager's own.
	Cluster *Cluster
	// Target is the first part's explicit location; the further parts
	// follow their Flux provenance.
	Target Target
	// Also are the further parts: objects that land elsewhere — a remote
	// target's ModelConfig in the kagent namespace of model-manager's own
	// cluster.
	Also []Part
	// Verb and Subject make the branch (model-manager/<verb>-<subject>) and
	// the title; Summary opens the body; Tool names the tool in it.
	Verb, Subject, Summary, Tool string
	DryRun                       bool
}

// Part is one location's share of a commit: the objects written (all in
// Namespace) and removed, the Flux object applying the target as it exists
// (nil: none), and the remote cluster they land on (nil: model-manager's
// own).
type Part struct {
	Namespace string
	Write     []*unstructured.Unstructured
	Remove    []*unstructured.Unstructured
	Owner     *Owner
	Cluster   *Cluster
}

// Cluster is a remote cluster a part lands on: its name, and the caller's
// client of it, which reads the namespace's Flux provenance there. The Flux
// objects themselves are read on model-manager's own cluster, where Flux
// runs and applies to the remote one through spec.kubeConfig.
type Cluster struct {
	Name    string
	Dynamic func(ctx context.Context) dynamic.Interface
}

// parts are the request's parts, the first one first.
func (r Request) parts() []Part {
	return append([]Part{{Namespace: r.Namespace, Write: r.Write, Remove: r.Remove, Owner: r.Owner, Cluster: r.Cluster}}, r.Also...)
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

// planned is one part's plan: its location, the layout's plan and the
// answer it gets.
type planned struct {
	loc  location
	plan *layout.Plan
	res  *Result
}

// Commit plans the request against the base branch and opens the pull
// request as the caller (or, dry run, answers what it would open): one per
// repository the parts land in. Every refusal is decided before anything is
// written.
func (c *Committer) Commit(ctx context.Context, req Request) (*Result, error) {
	gh, ok := identity.GitHubFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: commit mode opens the pull request with your GitHub authorization, and this call carries none: %s", ErrAuthRequired, signIn)
	}
	var remote Remote
	branch := "model-manager/" + req.Verb + "-" + req.Subject
	var plans []planned
	for i, part := range req.parts() {
		target := Target{}
		if i == 0 {
			target = req.Target
		}
		loc, err := c.locate(ctx, part, target)
		if err != nil {
			return nil, err
		}
		if remote == nil {
			if remote, err = c.remote(gh.Token); err != nil {
				return nil, commitError(err)
			}
		}
		p, err := c.plan(ctx, remote, loc, part)
		if err != nil {
			return nil, err
		}
		p.res.Branch, p.res.Author = branch, gh.Login
		for _, f := range p.plan.Files {
			file := File{Path: f.Path, Action: string(f.Action)}
			if req.DryRun {
				file.Content = string(f.Content)
			}
			p.res.Files = append(p.res.Files, file)
		}
		plans = append(plans, p)
	}
	var changes []commit.Change
	for _, p := range plans {
		if p.plan.Changed() {
			changes = append(changes, p.plan.Change())
		}
	}
	out := plans[0].res
	if !req.DryRun && len(changes) > 0 {
		title := fmt.Sprintf("feat(model-manager): %s %s", req.Verb, req.Subject)
		prs, err := commit.Open(ctx, remote, commit.Request{Branch: branch, Title: title, Body: body(req, gh.Login, plans)}, changes)
		if err != nil {
			return nil, commitError(err)
		}
		for _, p := range plans {
			for _, pr := range prs {
				if pr.Repository.String() == p.res.Repository && p.plan.Changed() {
					p.res.PullRequest, p.res.Number = pr.URL, pr.Number
				}
			}
		}
	}
	for _, p := range plans[1:] {
		out.Also = append(out.Also, *p.res)
	}
	return out, nil
}

// plan is one part's layout plan at its location, after the refusals that
// read the base.
func (c *Committer) plan(ctx context.Context, remote Remote, loc location, part Part) (planned, error) {
	dir := loc.directory()
	write, err := files(dir, part.Write)
	if err != nil {
		return planned{}, err
	}
	var remove []string
	for _, obj := range part.Remove {
		remove = append(remove, fileOf(dir, obj))
	}
	if err := c.inDirectory(ctx, remote, loc, part); err != nil {
		return planned{}, err
	}
	plan, err := layout.Build(ctx, remote, dir, write, remove)
	var secret *layout.SecretError
	if errors.As(err, &secret) {
		return planned{}, fmt.Errorf("%w: %s holds the placeholder API key, and %s: model-manager commits a Secret only encrypted for the repository's age recipients — pass apiKeySecret (an existing Secret) or apiKeyPassthrough, give the path an age creation rule, or use mode apply", backend.ErrInvalid, strings.Join(secret.Paths, ", "), secret.Reason)
	}
	if err != nil {
		return planned{}, commitError(err)
	}
	res := &Result{
		Repository: loc.Repository.String(), Base: loc.Branch, Directory: dir.Path(),
		Kustomization: loc.kustomization, Prune: loc.prune,
	}
	if part.Cluster != nil {
		res.Cluster = part.Cluster.Name
	}
	if len(part.Remove) > 0 && !loc.prune && loc.kustomization != "" {
		res.LiveSteps = append(res.LiveSteps, fmt.Sprintf("Flux does not prune Kustomization %s (spec.prune false): after the merge the objects stay on the installation — delete them by hand", loc.kustomization))
	}
	return planned{loc: loc, plan: plan, res: res}, nil
}

// inDirectory refuses a write on an object Flux applies from a file outside
// model-manager's directory (a second file would make the Kustomization's
// build fail on the duplicate), and a removal of an object that is not in
// the directory at all (it was written live: removed live).
func (c *Committer) inDirectory(ctx context.Context, remote Remote, loc location, part Part) error {
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
		case obj.GetKind() == part.firstKind() && part.Owner != nil:
			return fmt.Errorf("%w: %s %s/%s is applied from git by %s, but not from %s on %s: change it in the file it is written in", backend.ErrConflict, obj.GetKind(), obj.GetNamespace(), obj.GetName(), part.Owner, p, loc.Branch)
		}
		return nil
	}
	for _, obj := range part.Remove {
		if obj.GetKind() == "Secret" {
			continue // the placeholder follows its ModelConfig
		}
		if err := check(obj, true); err != nil {
			return err
		}
	}
	if len(part.Write) > 0 {
		return check(part.Write[0], false)
	}
	return nil
}

// firstKind is the kind of the object the part is about: its first.
func (p Part) firstKind() string {
	for _, list := range [][]*unstructured.Unstructured{p.Write, p.Remove} {
		if len(list) > 0 {
			return list[0].GetKind()
		}
	}
	return ""
}

// on names the cluster a part lands on in a refusal.
func (p Part) on() string {
	if p.Cluster == nil {
		return "model-manager's own cluster"
	}
	return "cluster " + p.Cluster.Name
}

// locate is a part's location: the explicit target, else the Flux
// provenance of the target — its own owner, else the namespace's.
func (c *Committer) locate(ctx context.Context, part Part, target Target) (location, error) {
	if part.Owner != nil && part.Owner.Kind == KindHelmRelease {
		return location{}, fmt.Errorf("%w: the object is rendered by %s: change the release's values in git", backend.ErrConflict, part.Owner)
	}
	if !target.Empty() {
		if target.Repository == "" || target.Branch == "" {
			return location{}, fmt.Errorf("%w: an explicit commit target needs repository (owner/name) and branch; path defaults to the repository root", backend.ErrInvalid)
		}
		loc, err := provenance.Explicit(target.Repository, target.Branch, target.Path)
		if err != nil {
			return location{}, fmt.Errorf("%w: %v", backend.ErrInvalid, err)
		}
		return location{Location: loc}, nil
	}
	owner := part.Owner
	if owner == nil {
		var err error
		if owner, err = c.namespaceOwner(ctx, part); err != nil {
			return location{}, err
		}
	}
	if part.Cluster != nil {
		return c.resolveRemote(ctx, owner, part)
	}
	return c.resolve(ctx, owner, part)
}

// namespaceOwner is the Flux object the part's namespace carries, read on
// the cluster the part lands on.
func (c *Committer) namespaceOwner(ctx context.Context, part Part) (*Owner, error) {
	dyn := c.dyn(ctx)
	if part.Cluster != nil {
		dyn = part.Cluster.Dynamic(ctx)
	}
	ns, err := dyn.Resource(namespaceGVR).Get(ctx, part.Namespace, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read namespace %s on %s for its Flux provenance: %w", part.Namespace, part.on(), err)
	}
	owner := OwnerOf(ns.GetLabels())
	if owner == nil {
		return nil, fmt.Errorf("%w: namespace %s on %s is not applied by Flux (no %s label): commit mode needs the repository — pass repository, branch and path, or use mode apply", backend.ErrInvalid, part.Namespace, part.on(), LabelKustomizeName)
	}
	return owner, nil
}

// resolve follows owner to its GitRepository and path: a HelmRelease
// through the Kustomization that applies it.
func (c *Committer) resolve(ctx context.Context, owner *Owner, part Part) (location, error) {
	if owner.Kind == KindHelmRelease {
		hr, err := c.dyn(ctx).Resource(HelmReleaseGVR).Namespace(owner.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil {
			return location{}, fmt.Errorf("get %s: %w", owner, err)
		}
		next := OwnerOf(hr.GetLabels())
		if next == nil || next.Kind != KindKustomization {
			return location{}, fmt.Errorf("%w: %s is not applied by a Flux Kustomization: commit mode needs the repository — pass repository, branch and path", backend.ErrInvalid, owner)
		}
		owner = next
	}
	ks, err := c.dyn(ctx).Resource(KustomizationGVR).Namespace(owner.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil {
		return location{}, fmt.Errorf("get %s: %w", owner, err)
	}
	return c.locationOf(ctx, owner, ks, part)
}

// resolveRemote is resolve for a part on a remote cluster, whose objects
// Flux on model-manager's own cluster applies through spec.kubeConfig: an
// owning Kustomization must apply through one; an owning HelmRelease (which
// renders the namespace there) names the cluster's kubeconfig Secret, and
// the location is the one Kustomization in its namespace applying through
// the same Secret.
func (c *Committer) resolveRemote(ctx context.Context, owner *Owner, part Part) (location, error) {
	dyn := c.dyn(ctx)
	elsewhere := fmt.Sprintf("pass repository, branch and path of a directory a Flux Kustomization applies to %s", part.on())
	if owner.Kind == KindKustomization {
		ks, err := dyn.Resource(KustomizationGVR).Namespace(owner.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil {
			return location{}, fmt.Errorf("get %s: %w", owner, err)
		}
		if kubeConfigSecret(ks) == "" {
			return location{}, fmt.Errorf("%w: %s applies to model-manager's own cluster, not to %s (no spec.kubeConfig): its files would land there — %s", backend.ErrInvalid, owner, part.on(), elsewhere)
		}
		return c.locationOf(ctx, owner, ks, part)
	}
	hr, err := dyn.Resource(HelmReleaseGVR).Namespace(owner.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil {
		return location{}, fmt.Errorf("get %s: %w", owner, err)
	}
	secret := kubeConfigSecret(hr)
	if secret == "" {
		return location{}, fmt.Errorf("%w: namespace %s on %s is rendered by %s, which has no spec.kubeConfig naming that cluster — %s", backend.ErrInvalid, part.Namespace, part.on(), owner, elsewhere)
	}
	list, err := dyn.Resource(KustomizationGVR).Namespace(owner.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return location{}, fmt.Errorf("list Kustomizations in %s: %w", owner.Namespace, err)
	}
	var matches []*unstructured.Unstructured
	var names []string
	for i := range list.Items {
		if kubeConfigSecret(&list.Items[i]) == secret {
			matches = append(matches, &list.Items[i])
			names = append(names, owner.Namespace+"/"+list.Items[i].GetName())
		}
	}
	if len(matches) != 1 {
		return location{}, fmt.Errorf("%w: namespace %s on %s is rendered by %s; %d Flux Kustomizations in %s apply through its kubeconfig Secret %s (%s), and commit mode needs exactly one — %s",
			backend.ErrInvalid, part.Namespace, part.on(), owner, len(matches), owner.Namespace, secret, strings.Join(names, ", "), elsewhere)
	}
	ks := matches[0]
	return c.locationOf(ctx, &Owner{Kind: KindKustomization, Namespace: ks.GetNamespace(), Name: ks.GetName()}, ks, part)
}

// kubeConfigSecret is the Secret a Flux object's spec.kubeConfig names, ""
// for one applying to its own cluster.
func kubeConfigSecret(obj *unstructured.Unstructured) string {
	return nested(obj, "spec", "kubeConfig", "secretRef", "name")
}

// locationOf is the Kustomization ks's (owner's) GitRepository and path, a
// Kustomization whose targetNamespace would move the part's objects out of
// its namespace refused.
func (c *Committer) locationOf(ctx context.Context, owner *Owner, ks *unstructured.Unstructured, part Part) (location, error) {
	dyn := c.dyn(ctx)
	if target := nested(ks, "spec", "targetNamespace"); target != "" {
		for _, obj := range append(slices.Clone(part.Write), part.Remove...) {
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
// which tool, the files of every part, and what lands them.
func body(req Request, login string, plans []planned) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s.\n\nOpened by @%s through model-manager (`%s`, mode commit).\n\nFiles:\n\n", req.Summary, login, req.Tool)
	for _, p := range plans {
		for _, f := range p.res.Files {
			if f.Action != string(layout.Unchanged) {
				fmt.Fprintf(&b, "- %s `%s`\n", f.Action, f.Path)
			}
		}
	}
	for _, p := range plans {
		if p.res.Kustomization == "" {
			continue
		}
		on := ""
		if p.res.Cluster != "" {
			on = " to cluster " + p.res.Cluster
		}
		fmt.Fprintf(&b, "\nFlux Kustomization `%s` applies `%s`%s after the merge.\n", p.res.Kustomization, p.res.Directory, on)
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
