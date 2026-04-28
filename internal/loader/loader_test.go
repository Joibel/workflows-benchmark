package loader

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
)

const minimalYAML = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  generateName: hello-
spec:
  entrypoint: main
  templates:
  - name: main
    container:
      image: alpine:3
      command: [echo, hi]
`

func TestLoadInjectsCleanupDefaults(t *testing.T) {
	tpl, err := Load([]byte(minimalYAML), "run-1", Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !tpl.Injected.TTLStrategy {
		t.Errorf("expected TTLStrategy injection")
	}

	wf := tpl.Clone()
	if wf.Spec.TTLStrategy == nil || wf.Spec.TTLStrategy.SecondsAfterSuccess == nil {
		t.Fatalf("TTLStrategy.SecondsAfterSuccess not set")
	}
	if got := *wf.Spec.TTLStrategy.SecondsAfterSuccess; got != 0 {
		t.Errorf("SecondsAfterSuccess = %d, want 0", got)
	}
	if wf.Spec.PodGC != nil {
		t.Errorf("PodGC = %v, want nil (loader no longer injects podGC)", wf.Spec.PodGC)
	}
	if wf.Name != "" {
		t.Errorf("Name = %q, want empty (generateName only)", wf.Name)
	}
	if !strings.HasPrefix(wf.GenerateName, "hello-") {
		t.Errorf("GenerateName = %q, want hello- prefix preserved", wf.GenerateName)
	}
	if wf.Labels[RunIDLabel] != "run-1" {
		t.Errorf("run-id label = %q, want run-1", wf.Labels[RunIDLabel])
	}
}

func TestLoadPreservesUserTTL(t *testing.T) {
	y := minimalYAML + `
  ttlStrategy:
    secondsAfterSuccess: 60
    secondsAfterFailure: 3600
`
	tpl, err := Load([]byte(y), "run-x", Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tpl.Injected.TTLStrategy {
		t.Errorf("should not inject TTLStrategy when user supplied it")
	}
	wf := tpl.Clone()
	if got := *wf.Spec.TTLStrategy.SecondsAfterSuccess; got != 60 {
		t.Errorf("SecondsAfterSuccess = %d, want 60 (user value preserved)", got)
	}
}

func TestLoadPreservesUserPodGC(t *testing.T) {
	y := minimalYAML + `
  podGC:
    strategy: OnWorkflowSuccess
`
	tpl, err := Load([]byte(y), "run-y", Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wf := tpl.Clone()
	if wf.Spec.PodGC == nil || wf.Spec.PodGC.Strategy != wfv1.PodGCOnWorkflowSuccess {
		t.Errorf("PodGC.Strategy = %v, want OnWorkflowSuccess (user value preserved)", wf.Spec.PodGC)
	}
}

func TestLoadDefaultsGenerateNameWhenMissing(t *testing.T) {
	y := `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  name: foo
spec:
  entrypoint: main
  templates:
  - name: main
    container:
      image: alpine:3
`
	tpl, err := Load([]byte(y), "run-z", Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wf := tpl.Clone()
	if wf.Name != "" {
		t.Errorf("Name = %q, want empty", wf.Name)
	}
	if wf.GenerateName != "bench-" {
		t.Errorf("GenerateName = %q, want bench-", wf.GenerateName)
	}
}

func TestLoadRejectsNonWorkflowKind(t *testing.T) {
	y := `
apiVersion: argoproj.io/v1alpha1
kind: WorkflowTemplate
metadata:
  name: foo
spec:
  entrypoint: main
  templates:
  - name: main
    container:
      image: alpine:3
`
	_, err := Load([]byte(y), "run-a", Options{})
	if err == nil {
		t.Fatalf("expected error rejecting WorkflowTemplate")
	}
}

func TestCloneIsIndependent(t *testing.T) {
	tpl, err := Load([]byte(minimalYAML), "run-q", Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := tpl.Clone()
	b := tpl.Clone()
	a.Labels["mutated"] = "x"
	if _, ok := b.Labels["mutated"]; ok {
		t.Fatalf("Clone() leaked mutation across instances")
	}
}

func TestLoadRequiresRunID(t *testing.T) {
	if _, err := Load([]byte(minimalYAML), "", Options{}); err == nil {
		t.Fatalf("expected error on empty runID")
	}
}

func TestLoadInjectsImagePullSecrets(t *testing.T) {
	tpl, err := Load([]byte(minimalYAML), "run-p", Options{
		ImagePullSecrets: []string{"test", "other"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !tpl.Injected.ImagePullSecrets {
		t.Errorf("expected ImagePullSecrets injection")
	}
	wf := tpl.Clone()
	if len(wf.Spec.ImagePullSecrets) != 2 {
		t.Fatalf("ImagePullSecrets=%v, want 2 entries", wf.Spec.ImagePullSecrets)
	}
	if wf.Spec.ImagePullSecrets[0].Name != "test" {
		t.Errorf("first secret name=%q, want test", wf.Spec.ImagePullSecrets[0].Name)
	}
}

func TestLoadPreservesUserImagePullSecrets(t *testing.T) {
	y := minimalYAML + `
  imagePullSecrets:
  - name: mine
`
	tpl, err := Load([]byte(y), "run-s", Options{
		ImagePullSecrets: []string{"test"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tpl.Injected.ImagePullSecrets {
		t.Errorf("should not inject ImagePullSecrets when user supplied them")
	}
	wf := tpl.Clone()
	if len(wf.Spec.ImagePullSecrets) != 1 || wf.Spec.ImagePullSecrets[0].Name != "mine" {
		t.Errorf("ImagePullSecrets=%v, want [{mine}]", wf.Spec.ImagePullSecrets)
	}
}

func TestLoadIgnoresEmptyImagePullSecretEntries(t *testing.T) {
	tpl, err := Load([]byte(minimalYAML), "run-e", Options{
		ImagePullSecrets: []string{"", ""},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tpl.Injected.ImagePullSecrets {
		t.Errorf("should not mark injected when all entries are empty")
	}
	wf := tpl.Clone()
	if len(wf.Spec.ImagePullSecrets) != 0 {
		t.Errorf("ImagePullSecrets=%v, want empty", wf.Spec.ImagePullSecrets)
	}
}

func TestLoadInjectsServiceAccountName(t *testing.T) {
	tpl, err := Load([]byte(minimalYAML), "run-sa", Options{
		ServiceAccountName: "argo",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !tpl.Injected.ServiceAccountName {
		t.Errorf("expected ServiceAccountName injection")
	}
	if got := tpl.Clone().Spec.ServiceAccountName; got != "argo" {
		t.Errorf("ServiceAccountName=%q, want argo", got)
	}
}

func TestLoadPreservesUserServiceAccountName(t *testing.T) {
	y := minimalYAML + `
  serviceAccountName: mine
`
	tpl, err := Load([]byte(y), "run-sb", Options{
		ServiceAccountName: "argo",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tpl.Injected.ServiceAccountName {
		t.Errorf("should not inject ServiceAccountName when user supplied it")
	}
	if got := tpl.Clone().Spec.ServiceAccountName; got != "mine" {
		t.Errorf("ServiceAccountName=%q, want mine", got)
	}
}
