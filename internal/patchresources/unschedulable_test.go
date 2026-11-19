package patchresources

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/internal/change"
	"github.com/a-hagi613/pagerless/internal/sandbox"
)

const (
	unschedulableManifestPath = "apps/checkout/deployment.yaml"
	cpuResource               = "cpu"
	memoryResource            = "memory"
	defaultCPURequest         = "100m"
	nodeMemoryBound           = "1Gi"
	unschedulableMemory       = "256Gi"
)

func unschedulableLiveWorkload(requests corev1.ResourceList) *change.LiveWorkload {
	deployment := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: appName,
						Resources: corev1.ResourceRequirements{
							Requests: requests,
						},
					}},
				},
			},
		},
	}
	return &change.LiveWorkload{Object: deployment, PodSpec: &deployment.Spec.Template.Spec}
}

type localRepo struct {
	t    *testing.T
	bare string
	work string
}

func newLocalRepo(t *testing.T) *localRepo {
	t.Helper()
	dir := t.TempDir()
	r := &localRepo{
		t:    t,
		bare: filepath.Join(dir, "sandbox.git"),
		work: filepath.Join(dir, "work"),
	}
	r.git(dir, "init", "--bare", "--initial-branch=main", r.bare)
	r.git(dir, "clone", r.bare, r.work)
	r.commit("README.md", "sandbox\n", "Initial commit")
	return r
}

func (r *localRepo) commit(path, content, message string) {
	r.t.Helper()
	full := filepath.Join(r.work, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.git(r.work, "add", path)
	r.git(r.work, "commit", "--message", message)
	r.git(r.work, "push", "origin", "HEAD:main")
}

func (r *localRepo) tag(name string) {
	r.t.Helper()
	r.git(r.work, "tag", "--force", name, "HEAD")
	r.git(r.work, "push", "--quiet", "--force", "origin", "refs/tags/"+name)
}

func (r *localRepo) repository() sandbox.Repository {
	return sandbox.Repository{URL: r.bare, Branch: "main"}
}

func (r *localRepo) git(dir string, args ...string) {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Pagerless Test", "GIT_AUTHOR_EMAIL=test@pagerless.dev",
		"GIT_COMMITTER_NAME=Pagerless Test", "GIT_COMMITTER_EMAIL=test@pagerless.dev",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
}

func TestCheckUnschedulableTreatsKnownGoodTagAsOptional(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{
		Name: "worker-1",
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}
	reader := fake.NewClientBuilder().WithObjects(node).Build()
	liveUnschedulable := func() *change.LiveWorkload {
		return unschedulableLiveWorkload(list(cpuResource, defaultCPURequest, memoryResource, unschedulableMemory))
	}

	t.Run("without a known-good tag, node allocatable bounds still apply", func(t *testing.T) {
		t.Parallel()
		repo := newLocalRepo(t)
		repo.commit(unschedulableManifestPath, header+appContainer, "Add checkout without known-good tag")
		r := &renderer{reader: reader, sandbox: repo.repository()}

		errs, err := r.checkUnschedulable(
			t.Context(), liveUnschedulable(), checkout, unschedulableManifestPath, appName,
			corev1.ResourceRequirements{Requests: list(cpuResource, defaultCPURequest, memoryResource, nodeMemoryBound)},
		)
		if err != nil || len(errs) != 0 {
			t.Fatalf("errs = %+v, err = %v, want accepted when fitting the node without known-good tag", errs, err)
		}

		errs, err = r.checkUnschedulable(
			t.Context(), liveUnschedulable(), checkout, unschedulableManifestPath, appName,
			corev1.ResourceRequirements{Requests: list(cpuResource, defaultCPURequest, memoryResource, "8Gi")},
		)
		if err != nil || len(errs) != 1 || !strings.Contains(errs[0].Message, "allocatable") {
			t.Fatalf("errs = %+v, err = %v, want node allocatable refusal", errs, err)
		}
	})

	t.Run("with a known-good tag, the declared request caps the Proposal", func(t *testing.T) {
		t.Parallel()
		repo := newLocalRepo(t)
		repo.commit(unschedulableManifestPath, header+`        - name: app
          image: x
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
`, "Add checkout at known-good")
		repo.tag(sandbox.KnownGoodTag)
		repo.commit(unschedulableManifestPath, header+`        - name: app
          image: x
          resources:
            requests:
              cpu: 100m
              memory: 256Gi
`, "Inject unschedulable memory request")
		r := &renderer{reader: reader, sandbox: repo.repository()}

		errs, err := r.checkUnschedulable(
			t.Context(), liveUnschedulable(), checkout, unschedulableManifestPath, appName,
			corev1.ResourceRequirements{Requests: list(cpuResource, defaultCPURequest, memoryResource, "128Mi")},
		)
		if err != nil || len(errs) != 0 {
			t.Fatalf("errs = %+v, err = %v, want accepted at the known-good request", errs, err)
		}

		errs, err = r.checkUnschedulable(
			t.Context(), liveUnschedulable(), checkout, unschedulableManifestPath, appName,
			corev1.ResourceRequirements{Requests: list(cpuResource, defaultCPURequest, memoryResource, nodeMemoryBound)},
		)
		if err != nil || len(errs) != 1 || !strings.Contains(errs[0].Message, "exceeds the known-good request 128Mi") {
			t.Fatalf("errs = %+v, err = %v, want known-good refusal", errs, err)
		}
	})

	t.Run("with a known-good tag that did not declare the request, node allocatable bounds apply", func(t *testing.T) {
		t.Parallel()
		repo := newLocalRepo(t)
		repo.commit(unschedulableManifestPath, header+appContainer, "Add checkout without resource requests")
		repo.tag(sandbox.KnownGoodTag)
		r := &renderer{reader: reader, sandbox: repo.repository()}

		errs, err := r.checkUnschedulable(
			t.Context(), liveUnschedulable(), checkout, unschedulableManifestPath, appName,
			corev1.ResourceRequirements{Requests: list(cpuResource, defaultCPURequest, memoryResource, nodeMemoryBound)},
		)
		if err != nil || len(errs) != 0 {
			t.Fatalf("errs = %+v, err = %v, want accepted when known-good declared no memory request", errs, err)
		}
	})

	t.Run("a resource no node reports as allocatable is refused", func(t *testing.T) {
		t.Parallel()
		repo := newLocalRepo(t)
		repo.commit(unschedulableManifestPath, header+appContainer, "Add checkout")
		r := &renderer{reader: reader, sandbox: repo.repository()}
		live := unschedulableLiveWorkload(list("nvidia.com/gpu", "1"))

		errs, err := r.checkUnschedulable(
			t.Context(), live, checkout, unschedulableManifestPath, appName,
			corev1.ResourceRequirements{Requests: list("nvidia.com/gpu", "1")},
		)
		if err != nil || len(errs) != 1 || !strings.Contains(errs[0].Message, "no node reports allocatable") {
			t.Fatalf("errs = %+v, err = %v, want unreachable resource refusal", errs, err)
		}
	})

	t.Run("an unexpected Sandbox failure fails closed", func(t *testing.T) {
		t.Parallel()
		r := &renderer{
			reader:  reader,
			sandbox: sandbox.Repository{URL: filepath.Join(t.TempDir(), "missing.git"), Branch: "main"},
		}
		_, err := r.checkUnschedulable(
			context.Background(), liveUnschedulable(), checkout, unschedulableManifestPath, appName,
			corev1.ResourceRequirements{Requests: list(cpuResource, defaultCPURequest, memoryResource, nodeMemoryBound)},
		)
		if err == nil {
			t.Fatal("checkUnschedulable() succeeded with an unreadable Sandbox, want an error")
		}
	})
}
