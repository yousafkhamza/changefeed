package kube

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yousafkhamza/changefeed/internal/event"
	"github.com/yousafkhamza/changefeed/internal/redact"
)

// Annotations and labels changefeed reads from workloads.
const (
	AnnotationRevision = "changefeed/revision"
	AnnotationService  = "changefeed/service"
	LabelIgnore        = "changefeed/ignore"
	restartedAt        = "kubectl.kubernetes.io/restartedAt"
	maxValueLen        = 80
)

// WorkloadChange is the result of comparing two versions of a workload.
type WorkloadChange struct {
	Kind    event.Kind
	Risk    event.Risk
	Summary string
	Diff    []string
}

// DiffWorkload compares pod templates and replica counts of a Deployment,
// StatefulSet or DaemonSet. oldReplicas/newReplicas are nil for DaemonSets.
// It returns ok=false when nothing a human would care about changed.
func DiffWorkload(kindName, name string, oldT, newT corev1.PodTemplateSpec, oldReplicas, newReplicas *int32) (WorkloadChange, bool) {
	var (
		images, env, resources, lines []string
		restarted                     bool
	)
	oldC := containersByName(oldT.Spec)
	newC := containersByName(newT.Spec)
	for _, n := range sortedKeys(newC) {
		nc := newC[n]
		oc, existed := oldC[n]
		if !existed {
			lines = append(lines, fmt.Sprintf("+ container %s: %s", n, nc.Image))
			images = append(images, nc.Image)
			continue
		}
		if oc.Image != nc.Image {
			lines = append(lines, fmt.Sprintf("~ image %s: %s → %s", n, oc.Image, nc.Image))
			images = append(images, nc.Image)
		}
		env = append(env, diffEnv(n, oc.Env, nc.Env)...)
		resources = append(resources, diffResources(n, oc.Resources, nc.Resources)...)
	}
	for _, n := range sortedKeys(oldC) {
		if _, ok := newC[n]; !ok {
			lines = append(lines, "- container "+n)
		}
	}
	if oldT.Annotations[restartedAt] != newT.Annotations[restartedAt] && newT.Annotations[restartedAt] != "" {
		restarted = true
	}
	lines = append(lines, env...)
	lines = append(lines, resources...)

	var scaled bool
	var from, to int32
	if oldReplicas != nil && newReplicas != nil && *oldReplicas != *newReplicas {
		scaled, from, to = true, *oldReplicas, *newReplicas
		lines = append(lines, fmt.Sprintf("~ replicas: %d → %d", from, to))
	}

	templateChanged := !equality.Semantic.DeepEqual(oldT, newT)
	switch {
	case len(images) > 0:
		s := fmt.Sprintf("Rollout %s: %s", name, shortImage(images[0]))
		if len(images) > 1 {
			s = fmt.Sprintf("Rollout %s: %d images changed", name, len(images))
		}
		return WorkloadChange{Kind: event.KindDeploy, Risk: event.RiskMedium, Summary: s, Diff: lines}, true
	case restarted:
		return WorkloadChange{Kind: event.KindDeploy, Risk: event.RiskLow, Summary: "Rollout restart " + name, Diff: lines}, true
	case len(env)+len(resources) > 0:
		what := plural(len(env), "env var") + joinIf(len(env) > 0 && len(resources) > 0, " and ") + plural(len(resources), "resource setting")
		return WorkloadChange{Kind: event.KindConfig, Risk: event.RiskMedium, Summary: fmt.Sprintf("%s %s: %s changed", kindName, name, what), Diff: lines}, true
	case templateChanged:
		return WorkloadChange{Kind: event.KindDeploy, Risk: event.RiskLow, Summary: fmt.Sprintf("%s %s: pod spec changed", kindName, name), Diff: lines}, true
	case scaled:
		r := event.RiskLow
		if to == 0 || (to <= 1 && from > 1) {
			r = event.RiskHigh
		}
		return WorkloadChange{Kind: event.KindScale, Risk: r, Summary: fmt.Sprintf("Scaled %s %d → %d replicas", name, from, to), Diff: lines}, true
	}
	return WorkloadChange{}, false
}

// DiffConfigMap lists changed keys. Values are shown (redacted) only when
// showValues is set; keys that look sensitive never show values.
func DiffConfigMap(oldData, newData map[string]string, oldBin, newBin map[string][]byte, showValues bool) []string {
	var lines []string
	keys := map[string]bool{}
	for k := range oldData {
		keys[k] = true
	}
	for k := range newData {
		keys[k] = true
	}
	for _, k := range sortedKeys(keys) {
		ov, inOld := oldData[k]
		nv, inNew := newData[k]
		switch {
		case inOld && !inNew:
			lines = append(lines, "- "+k)
		case !inOld && inNew:
			lines = append(lines, "+ "+k+valueSuffix(k, nv, showValues))
		case ov != nv:
			if showValues && !redact.SensitiveKey(k) && short(ov) && short(nv) {
				lines = append(lines, "- "+k+": "+redact.Value(k, ov), "+ "+k+": "+redact.Value(k, nv))
			} else {
				lines = append(lines, fmt.Sprintf("~ %s (%d → %d bytes)", k, len(ov), len(nv)))
			}
		}
	}
	for _, k := range sortedKeys(mergeKeys(oldBin, newBin)) {
		if string(oldBin[k]) != string(newBin[k]) {
			lines = append(lines, "~ "+k+" (binary)")
		}
	}
	return lines
}

func valueSuffix(k, v string, show bool) string {
	if !show || redact.SensitiveKey(k) || !short(v) {
		return ""
	}
	return ": " + redact.Value(k, v)
}

func short(v string) bool { return len(v) <= maxValueLen && !strings.Contains(v, "\n") }

func diffEnv(container string, oldEnv, newEnv []corev1.EnvVar) []string {
	ov, nv := envMap(oldEnv), envMap(newEnv)
	var out []string
	for _, k := range sortedKeys(mergeKeys(ov, nv)) {
		a, inA := ov[k]
		b, inB := nv[k]
		switch {
		case inA && !inB:
			out = append(out, fmt.Sprintf("- env %s.%s", container, k))
		case !inA && inB:
			out = append(out, fmt.Sprintf("+ env %s.%s: %s", container, k, redact.Value(k, b)))
		case a != b:
			out = append(out, fmt.Sprintf("~ env %s.%s: %s → %s", container, k, redact.Value(k, a), redact.Value(k, b)))
		}
	}
	return out
}

// envMap flattens env vars; references (secretKeyRef etc.) are shown by
// source, never resolved.
func envMap(env []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		v := e.Value
		if vf := e.ValueFrom; vf != nil {
			switch {
			case vf.SecretKeyRef != nil:
				v = "secret:" + vf.SecretKeyRef.Name + "/" + vf.SecretKeyRef.Key
			case vf.ConfigMapKeyRef != nil:
				v = "configmap:" + vf.ConfigMapKeyRef.Name + "/" + vf.ConfigMapKeyRef.Key
			case vf.FieldRef != nil:
				v = "field:" + vf.FieldRef.FieldPath
			case vf.ResourceFieldRef != nil:
				v = "resource:" + vf.ResourceFieldRef.Resource
			}
		}
		if !short(v) {
			v = fmt.Sprintf("(%d bytes)", len(v))
		}
		m[e.Name] = v
	}
	return m
}

func diffResources(container string, a, b corev1.ResourceRequirements) []string {
	var out []string
	for _, side := range []struct {
		name string
		a, b corev1.ResourceList
	}{{"requests", a.Requests, b.Requests}, {"limits", a.Limits, b.Limits}} {
		for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			av, aok := side.a[res]
			bv, bok := side.b[res]
			as, bs := "none", "none"
			if aok {
				as = av.String()
			}
			if bok {
				bs = bv.String()
			}
			if as != bs {
				out = append(out, fmt.Sprintf("~ %s %s.%s: %s → %s", side.name, container, res, as, bs))
			}
		}
	}
	return out
}

func containersByName(spec corev1.PodSpec) map[string]corev1.Container {
	m := map[string]corev1.Container{}
	for _, c := range spec.InitContainers {
		m["init:"+c.Name] = c
	}
	for _, c := range spec.Containers {
		m[c.Name] = c
	}
	return m
}

// ServiceName picks the most specific name a team would recognize.
func ServiceName(obj metav1.Object) string {
	l, a := obj.GetLabels(), obj.GetAnnotations()
	for _, v := range []string{a[AnnotationService], l["app.kubernetes.io/name"], l["app"], l["k8s-app"]} {
		if v != "" {
			return strings.ToLower(v)
		}
	}
	return strings.ToLower(obj.GetName())
}

var reHexTag = regexp.MustCompile(`^(sha-)?[0-9a-f]{7,40}$`)

// Revision finds a commit SHA so the rollout can join its PR's change set:
// an explicit annotation, a hex app.kubernetes.io/version, or a hex image tag.
func Revision(obj metav1.Object, t corev1.PodTemplateSpec) string {
	for _, v := range []string{t.Annotations[AnnotationRevision], obj.GetAnnotations()[AnnotationRevision]} {
		if v != "" {
			return v
		}
	}
	if v := obj.GetLabels()["app.kubernetes.io/version"]; reHexTag.MatchString(strings.ToLower(v)) {
		return strings.TrimPrefix(strings.ToLower(v), "sha-")
	}
	for _, c := range t.Spec.Containers {
		if tag := imageTag(c.Image); reHexTag.MatchString(tag) {
			return strings.TrimPrefix(tag, "sha-")
		}
	}
	return ""
}

func imageTag(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndex(image, "/")
	if i := strings.LastIndex(image, ":"); i > slash {
		return strings.ToLower(image[i+1:])
	}
	return ""
}

func shortImage(image string) string {
	if i := strings.LastIndex(image, "/"); i >= 0 {
		return image[i+1:]
	}
	return image
}

// Actor returns the field manager of the latest spec change, e.g.
// "kubectl-edit", "argocd-controller" or "helm", and whether that manager is
// a human tool that bypasses CI.
func Actor(obj metav1.Object) (string, bool) {
	var (
		best metav1.ManagedFieldsEntry
		bt   time.Time
	)
	for _, mf := range obj.GetManagedFields() {
		if mf.Subresource == "status" || mf.Time == nil {
			continue
		}
		if t := mf.Time.Time; t.After(bt) || t.Equal(bt) && best.Manager == "" {
			best, bt = mf, mf.Time.Time
		}
	}
	m := best.Manager
	if best.Subresource == "scale" && m == "kube-controller-manager" {
		m = "horizontal-pod-autoscaler"
	}
	human := strings.HasPrefix(m, "kubectl") || m == "k9s" || strings.HasPrefix(m, "Mozilla") || m == "dashboard"
	return m, human
}

func plural(n int, word string) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func joinIf(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := slices.Collect(maps.Keys(m))
	sort.Strings(keys)
	return keys
}

func mergeKeys[V any](a, b map[string]V) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}
