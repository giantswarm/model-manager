package kserve

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/giantswarm/model-manager/internal/backend"
)

// Annotations on download Jobs so a restarted model-manager can adopt them.
const (
	DirAnnotation      = "model-manager.giantswarm.io/cache-dir"
	NodeAnnotation     = "model-manager.giantswarm.io/node"
	PresetAnnotation   = "model-manager.giantswarm.io/preset"
	RevisionAnnotation = "model-manager.giantswarm.io/revision"
	BytesAnnotation    = "model-manager.giantswarm.io/bytes-total"
	componentDownload  = "download"
	jobPodLabel        = "job-name"

	// downloadEntrypoint is the KServe storage-initializer's entrypoint
	// (src_uri dest_path pairs).
	downloadEntrypoint = "/storage-initializer/scripts/initializer-entrypoint"
	downloadUID        = int64(1000)
)

// downloadScript wraps the storage-initializer so progress (apparent bytes in
// the target directory) is printed every 10 s and, on success, a marker file
// records which repository landed in which directory.
const downloadScript = `set -u
export HF_HOME="${HF_HOME:-/tmp/hf}"
mkdir -p "$HF_HOME" 2>/dev/null || true
report() { printf 'PROGRESS %s\n' "$(du -sb "$MM_DST" 2>/dev/null | cut -f1)"; }
report
` + downloadEntrypoint + ` "$MM_SRC" "$MM_DST" &
pid=$!
while kill -0 "$pid" 2>/dev/null; do sleep 10; report; done
wait "$pid"; rc=$?
report
if [ "$rc" -ne 0 ]; then echo "DOWNLOAD FAILED rc=$rc"; exit "$rc"; fi
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '{"model":"%s","revision":"%s","dir":"%s","bytesExpected":%s,"completedAt":"%s","job":"%s"}\n' "$MM_MODEL" "$MM_REVISION" "$MM_DIRNAME" "${MM_BYTES:-0}" "$now" "$MM_JOB" > "$MM_MARKER"
echo DONE`

var progressLine = regexp.MustCompile(`(?m)^PROGRESS (\d+)\s*$`)

// downloadPlan is everything a pre-warm download needs.
type downloadPlan struct {
	Repo       string
	Revision   string
	Dir        string
	Node       string
	Preset     string
	BytesTotal int64
}

func (p downloadPlan) jobName() string { return prefixed(jobPrefix, p.Dir) }

func (p downloadPlan) storageURI() string {
	if p.Revision != "" {
		return "hf://" + p.Repo + ":" + p.Revision
	}
	return "hf://" + p.Repo
}

// buildJob composes the download Job: an init container (root) creates the
// cache directory world-writable — the same step the modelServing Kyverno
// policy's hf-cache-init container performs for InferenceServices — and the
// storage-initializer downloads into it as its own uid.
func (b *Backend) buildJob(plan downloadPlan, s settings) *batchv1.Job {
	dst := cacheMount + "/" + plan.Dir
	markers := cacheMount + "/" + markersDir
	env := []corev1.EnvVar{
		{Name: "MM_SRC", Value: plan.storageURI()},
		{Name: "MM_DST", Value: dst},
		{Name: "MM_DIRNAME", Value: plan.Dir},
		{Name: "MM_MARKER", Value: markers + "/" + plan.Dir + ".json"},
		{Name: "MM_MODEL", Value: plan.Repo},
		{Name: "MM_REVISION", Value: plan.Revision},
		{Name: "MM_BYTES", Value: strconv.FormatInt(plan.BytesTotal, 10)},
		{Name: "MM_JOB", Value: plan.jobName()},
		{Name: "HF_HOME", Value: "/tmp/hf"},
	}
	if len(b.opts.DownloadIgnorePatterns) > 0 {
		raw, _ := json.Marshal(b.opts.DownloadIgnorePatterns)
		env = append(env, corev1.EnvVar{Name: "STORAGE_IGNORE_PATTERNS", Value: string(raw)})
	}
	if b.opts.HFTokenSecret != "" {
		env = append(env, corev1.EnvVar{Name: "HF_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: b.opts.HFTokenSecret},
			Key:                  b.opts.HFTokenSecretKey,
			Optional:             ptr.To(true),
		}}})
	}
	labels := b.labels(map[string]string{ComponentLabel: componentDownload, "model-manager.giantswarm.io/model-dir": plan.Dir})
	annotations := map[string]string{
		ModelAnnotation:    plan.Repo,
		DirAnnotation:      plan.Dir,
		NodeAnnotation:     plan.Node,
		PresetAnnotation:   plan.Preset,
		RevisionAnnotation: plan.Revision,
		BytesAnnotation:    strconv.FormatInt(plan.BytesTotal, 10),
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        plan.jobName(),
			Namespace:   s.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](2),
			TTLSecondsAfterFinished: ptr.To(int32(b.opts.JobTTL / time.Second)), // #nosec G115 -- bounded by the option
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{ModelAnnotation: plan.Repo}},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					NodeName:                     plan.Node,
					InitContainers: []corev1.Container{{
						Name:    "hf-cache-init",
						Image:   b.opts.InitImage,
						Command: []string{scriptShell, "-ec", fmt.Sprintf("mkdir -p %q %q && chmod 0777 %q %q", dst, markers, dst, markers)},
						SecurityContext: &corev1.SecurityContext{
							RunAsUser:                ptr.To[int64](0),
							RunAsNonRoot:             ptr.To(false),
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
								Add:  []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER"},
							},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: cacheVolume, MountPath: cacheMount}},
					}},
					Containers: []corev1.Container{{
						Name:    "download",
						Image:   b.opts.DownloadImage,
						Command: []string{scriptShell, "-c", downloadScript},
						Env:     env,
						SecurityContext: &corev1.SecurityContext{
							RunAsUser:                ptr.To(downloadUID),
							RunAsNonRoot:             ptr.To(true),
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: cacheVolume, MountPath: cacheMount},
							{Name: "tmp", MountPath: "/tmp"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: cacheVolume, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: s.CacheClaim}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
	return job
}

// ensureJob returns the running Job for the plan, adopting an existing active
// one; a finished leftover (failed, or succeeded but not yet TTL-collected) is
// replaced.
func (b *Backend) ensureJob(ctx context.Context, plan downloadPlan) (job *batchv1.Job, adopted bool, err error) {
	s := b.cfg.settings(ctx)
	jobs := b.cs.BatchV1().Jobs(s.Namespace)
	existing, err := jobs.Get(ctx, plan.jobName(), metav1.GetOptions{})
	switch {
	case err == nil:
		if existing.Labels[ManagedByLabel] != ManagedByValue {
			return nil, false, fmt.Errorf("%w: Job %s/%s exists but is not managed by %s", backend.ErrConflict, s.Namespace, existing.Name, ManagedByValue)
		}
		if !jobFinished(existing) && existing.DeletionTimestamp == nil {
			return existing, true, nil
		}
		if err := b.deleteJob(ctx, s.Namespace, existing.Name); err != nil {
			return nil, false, err
		}
		if err := b.waitJobGone(ctx, s.Namespace, existing.Name); err != nil {
			return nil, false, err
		}
	case !errors.IsNotFound(err):
		return nil, false, fmt.Errorf("get Job %s/%s: %w", s.Namespace, plan.jobName(), err)
	}
	created, err := jobs.Create(ctx, b.buildJob(plan, s), metav1.CreateOptions{FieldManager: ManagedByValue})
	if err != nil {
		return nil, false, fmt.Errorf("create download Job %s/%s: %w", s.Namespace, plan.jobName(), err)
	}
	return created, false, nil
}

func (b *Backend) deleteJob(ctx context.Context, namespace, name string) error {
	propagation := metav1.DeletePropagationBackground
	err := b.cs.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete Job %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (b *Backend) waitJobGone(ctx context.Context, namespace, name string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, err := b.cs.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("job %s/%s did not go away", namespace, name)
}

func jobFinished(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailure(j *batchv1.Job) string {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return strings.TrimSpace(c.Reason + " " + c.Message)
		}
	}
	return ""
}

func jobSucceeded(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// watchJob follows a download Job until it finishes, reporting bytes on disk
// (from the pod's PROGRESS lines) against the expected total. Cancelling ctx
// deletes the Job; partial files stay so a retry resumes.
func (b *Backend) watchJob(ctx context.Context, plan downloadPlan, progress func(backend.Progress)) error {
	s := b.cfg.settings(ctx)
	name := plan.jobName()
	report := func(status string, done int64) {
		if progress != nil {
			progress(backend.Progress{Status: status, BytesCompleted: done, BytesTotal: plan.BytesTotal})
		}
	}
	report("download job created", 0)
	ticker := time.NewTicker(b.opts.PollInterval)
	defer ticker.Stop()
	var lastBytes int64
	for {
		job, err := b.cs.BatchV1().Jobs(s.Namespace).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return fmt.Errorf("download Job %s/%s disappeared", s.Namespace, name)
		}
		if err != nil {
			b.log.Warn("polling download Job failed", "job", name, "error", err)
		} else {
			pod := b.latestJobPod(ctx, s.Namespace, name)
			if pod != nil {
				if n, ok := b.podProgress(ctx, pod); ok && n > lastBytes {
					lastBytes = n
				}
			}
			switch {
			case jobSucceeded(job):
				if plan.BytesTotal > 0 {
					lastBytes = plan.BytesTotal
				}
				report("downloaded", lastBytes)
				b.inv.invalidate()
				return nil
			case jobFailure(job) != "":
				tail := ""
				if pod != nil {
					tail, _ = b.podLogs(ctx, pod.Namespace, pod.Name, 15)
				}
				return fmt.Errorf("download failed: %s: %s", jobFailure(job), lastLines(tail, 5))
			case pod == nil:
				report("waiting for the download pod", lastBytes)
			case pod.Status.Phase == corev1.PodPending:
				if reason := podStuckReason(pod); reason != "" {
					_ = b.deleteJob(ctx, s.Namespace, name)
					return fmt.Errorf("download pod cannot start: %s", reason)
				}
				report("starting the download pod", lastBytes)
			default:
				report("downloading", lastBytes)
			}
		}
		select {
		case <-ctx.Done():
			dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := b.deleteJob(dctx, s.Namespace, name); err != nil {
				b.log.Warn("deleting cancelled download Job failed", "job", name, "error", err)
			}
			cancel()
			b.inv.invalidate()
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// latestJobPod returns the newest pod of a Job (nil when none exists yet).
func (b *Backend) latestJobPod(ctx context.Context, namespace, job string) *corev1.Pod {
	pods, err := b.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: jobPodLabel + "=" + job})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.After(pods.Items[j].CreationTimestamp.Time)
	})
	return &pods.Items[0]
}

// podProgress reads the last PROGRESS line of the download container.
func (b *Backend) podProgress(ctx context.Context, pod *corev1.Pod) (int64, bool) {
	if pod.Status.Phase == corev1.PodPending {
		return 0, false
	}
	out, err := b.podLogs(ctx, pod.Namespace, pod.Name, 40)
	if err != nil {
		return 0, false
	}
	return parseProgress(out)
}

func parseProgress(logs string) (int64, bool) {
	matches := progressLine.FindAllStringSubmatch(logs, -1)
	if len(matches) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(matches[len(matches)-1][1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// runningDownloads lists the active download Jobs as pull requests (adoption
// after a restart of model-manager).
func (b *Backend) runningDownloads(ctx context.Context) ([]backend.PullRequest, error) {
	s := b.cfg.settings(ctx)
	list, err := b.cs.BatchV1().Jobs(s.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: ManagedByLabel + "=" + ManagedByValue + "," + ComponentLabel + "=" + componentDownload,
	})
	if err != nil {
		return nil, fmt.Errorf("list download Jobs in %s: %w", s.Namespace, err)
	}
	var out []backend.PullRequest
	for i := range list.Items {
		j := &list.Items[i]
		if jobFinished(j) || j.DeletionTimestamp != nil {
			continue
		}
		ref := j.Annotations[ModelAnnotation]
		if ref == "" {
			continue
		}
		if rev := j.Annotations[RevisionAnnotation]; rev != "" {
			ref += ":" + rev
		}
		out = append(out, backend.PullRequest{Ref: ref, Preset: j.Annotations[PresetAnnotation], Node: j.Annotations[NodeAnnotation]})
	}
	return out, nil
}
