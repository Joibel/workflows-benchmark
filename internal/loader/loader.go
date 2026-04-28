// Package loader parses a user-supplied Workflow YAML into a benchmark
// template: a wfv1.Workflow that is cloned for every submission. A
// ttlStrategy is injected when missing so finished workflows don't
// accumulate in the cluster; pod cleanup is left to the user's workflow
// config and the controller's workflow GC. A run-id label is added so
// the watcher/informer can filter to just this run.
package loader

import (
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	RunIDLabel     = "workflows-benchmark.io/run-id"
	defaultPrefix  = "bench-"
	failureKeepSec = int32(86400)
)

// Template holds the parsed workflow plus the labels to stamp on each
// submission. Clone() produces a fresh Workflow suitable for Create().
type Template struct {
	base  wfv1.Workflow
	runID string
	// Injected records which cleanup settings this loader added, so main
	// can log what it changed vs what the user supplied.
	Injected Injections
}

type Injections struct {
	TTLStrategy        bool
	ImagePullSecrets   bool
	ServiceAccountName bool
}

// Options are knobs the caller can set to control injection behavior.
// Everything here has inject-if-missing semantics — if the user's YAML
// already sets the corresponding field, we respect it and do not
// overwrite.
type Options struct {
	// ImagePullSecrets, if non-empty, are injected into
	// spec.imagePullSecrets when the user's YAML has none. Use this on
	// clusters where benchmark pods need to pull from a private
	// registry.
	ImagePullSecrets []string

	// ServiceAccountName, if non-empty, is injected into
	// spec.serviceAccountName when the user's YAML leaves it blank.
	// Argo needs a SA with permission to create workflowtaskresults;
	// the default "default" SA typically lacks this, causing every
	// wait-container to fail with a forbidden error.
	ServiceAccountName string
}

// Load parses YAML bytes into a Template, injecting cleanup defaults.
// runID must be non-empty; it is added as a label on every clone.
func Load(data []byte, runID string, opts Options) (*Template, error) {
	if runID == "" {
		return nil, fmt.Errorf("runID must not be empty")
	}

	var wf wfv1.Workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow yaml: %w", err)
	}

	if wf.Kind != "" && wf.Kind != "Workflow" {
		return nil, fmt.Errorf("unsupported kind %q: only Workflow is accepted in v1", wf.Kind)
	}
	if len(wf.Spec.Templates) == 0 && wf.Spec.WorkflowTemplateRef == nil {
		return nil, fmt.Errorf("workflow has no templates and no workflowTemplateRef")
	}

	// generateName: honor user's choice if set, otherwise use our prefix.
	if wf.GenerateName == "" {
		wf.GenerateName = defaultPrefix
	}
	// A concrete name would collide on every Create — force generateName only.
	wf.Name = ""

	var inj Injections
	if wf.Spec.TTLStrategy == nil {
		zero := int32(0)
		keep := failureKeepSec
		wf.Spec.TTLStrategy = &wfv1.TTLStrategy{
			SecondsAfterSuccess: &zero,
			SecondsAfterFailure: &keep,
		}
		inj.TTLStrategy = true
	}
	if len(opts.ImagePullSecrets) > 0 && len(wf.Spec.ImagePullSecrets) == 0 {
		refs := make([]corev1.LocalObjectReference, 0, len(opts.ImagePullSecrets))
		for _, name := range opts.ImagePullSecrets {
			if name == "" {
				continue
			}
			refs = append(refs, corev1.LocalObjectReference{Name: name})
		}
		if len(refs) > 0 {
			wf.Spec.ImagePullSecrets = refs
			inj.ImagePullSecrets = true
		}
	}
	if opts.ServiceAccountName != "" && wf.Spec.ServiceAccountName == "" {
		wf.Spec.ServiceAccountName = opts.ServiceAccountName
		inj.ServiceAccountName = true
	}

	return &Template{base: wf, runID: runID, Injected: inj}, nil
}

// Clone returns a fresh Workflow ready to Create(). The run-id label is
// (re)stamped on every clone so the informer filter matches even if the
// base YAML had no labels map.
func (t *Template) Clone() *wfv1.Workflow {
	out := t.base.DeepCopy()
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	out.Labels[RunIDLabel] = t.runID
	// ResourceVersion/UID etc. must be empty on Create.
	out.ObjectMeta = metav1.ObjectMeta{
		GenerateName: out.GenerateName,
		Namespace:    out.Namespace,
		Labels:       out.Labels,
		Annotations:  out.Annotations,
	}
	return out
}

// RunID returns the run id stamped on every clone.
func (t *Template) RunID() string { return t.runID }
