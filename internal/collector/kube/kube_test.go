package kube

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/yousafkhamza/changefeed/internal/config"
	"github.com/yousafkhamza/changefeed/internal/event"
)

func tmpl(image string, env ...corev1.EnvVar) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image, Env: env}}}}
}

func i32(v int32) *int32 { return &v }

func TestDiffWorkloadImage(t *testing.T) {
	ch, ok := DiffWorkload("Deployment", "checkout-api", tmpl("ghcr.io/shop/checkout:4d81f30"), tmpl("ghcr.io/shop/checkout:9c2b7e1"), i32(8), i32(8))
	if !ok || ch.Kind != event.KindDeploy || ch.Summary != "Rollout checkout-api: checkout:9c2b7e1" {
		t.Fatalf("%+v", ch)
	}
	if ch.Diff[0] != "~ image app: ghcr.io/shop/checkout:4d81f30 → ghcr.io/shop/checkout:9c2b7e1" {
		t.Errorf("diff: %v", ch.Diff)
	}
}

func TestDiffWorkloadEnvRedacts(t *testing.T) {
	old := tmpl("a:1", corev1.EnvVar{Name: "PAYMENT_TIMEOUT_MS", Value: "3000"}, corev1.EnvVar{Name: "DB_PASSWORD", Value: "old"})
	nw := tmpl("a:1", corev1.EnvVar{Name: "PAYMENT_TIMEOUT_MS", Value: "800"}, corev1.EnvVar{Name: "DB_PASSWORD", Value: "new"})
	ch, ok := DiffWorkload("Deployment", "checkout-api", old, nw, nil, nil)
	if !ok || ch.Kind != event.KindConfig || ch.Summary != "Deployment checkout-api: 2 env vars changed" {
		t.Fatalf("%+v", ch)
	}
	joined := strings.Join(ch.Diff, "\n")
	if !strings.Contains(joined, "PAYMENT_TIMEOUT_MS: 3000 → 800") || strings.Contains(joined, "old") || strings.Contains(joined, "new") {
		t.Errorf("diff: %s", joined)
	}
}

func TestDiffWorkloadScaleAndRestart(t *testing.T) {
	ch, ok := DiffWorkload("Deployment", "cart", tmpl("a:1"), tmpl("a:1"), i32(4), i32(7))
	if !ok || ch.Kind != event.KindScale || ch.Risk != event.RiskLow || ch.Summary != "Scaled cart 4 → 7 replicas" {
		t.Fatalf("%+v", ch)
	}
	ch, _ = DiffWorkload("Deployment", "cart", tmpl("a:1"), tmpl("a:1"), i32(4), i32(0))
	if ch.Risk != event.RiskHigh {
		t.Error("scale to zero must be high risk")
	}
	restart := tmpl("a:1")
	restart.Annotations = map[string]string{restartedAt: "2026-11-24T14:00:00Z"}
	ch, ok = DiffWorkload("Deployment", "cart", tmpl("a:1"), restart, i32(4), i32(4))
	if !ok || ch.Summary != "Rollout restart cart" {
		t.Fatalf("%+v", ch)
	}
	if _, ok := DiffWorkload("Deployment", "cart", tmpl("a:1"), tmpl("a:1"), i32(4), i32(4)); ok {
		t.Error("identical specs must not produce a change")
	}
	probe := tmpl("a:1")
	probe.Spec.Containers[0].ReadinessProbe = &corev1.Probe{InitialDelaySeconds: 5}
	if ch, ok := DiffWorkload("Deployment", "cart", tmpl("a:1"), probe, nil, nil); !ok || !strings.Contains(ch.Summary, "pod spec changed") {
		t.Errorf("probe change: %+v", ch)
	}
}

func TestDiffWorkloadResources(t *testing.T) {
	old, nw := tmpl("a:1"), tmpl("a:1")
	nw.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}
	ch, ok := DiffWorkload("Deployment", "cart", old, nw, nil, nil)
	if !ok || ch.Diff[0] != "~ limits app.memory: none → 512Mi" {
		t.Fatalf("%+v", ch)
	}
}

func TestDiffConfigMap(t *testing.T) {
	old := map[string]string{"PAYMENT_TIMEOUT_MS": "3000", "STRIPE_API_KEY": "sk_live_x", "REMOVED": "1", "BIG": strings.Repeat("x", 200)}
	nw := map[string]string{"PAYMENT_TIMEOUT_MS": "800", "STRIPE_API_KEY": "sk_live_y", "ADDED": "on", "BIG": strings.Repeat("y", 210)}
	got := strings.Join(DiffConfigMap(old, nw, nil, nil, true), "\n")
	want := strings.Join([]string{
		"+ ADDED: on",
		"~ BIG (200 → 210 bytes)",
		"- PAYMENT_TIMEOUT_MS: 3000",
		"+ PAYMENT_TIMEOUT_MS: 800",
		"- REMOVED",
		"~ STRIPE_API_KEY (9 → 9 bytes)",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	hidden := strings.Join(DiffConfigMap(old, nw, nil, nil, false), "\n")
	if strings.Contains(hidden, "3000") || strings.Contains(hidden, ": on") {
		t.Errorf("values shown with showValues=false: %s", hidden)
	}
}

func TestRevisionAndServiceName(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "checkout-api-v2", Labels: map[string]string{"app.kubernetes.io/name": "Checkout-API"}}}
	if got := ServiceName(d); got != "checkout-api" {
		t.Errorf("service: %s", got)
	}
	for img, want := range map[string]string{
		"ghcr.io/shop/checkout:9c2b7e1":                  "9c2b7e1",
		"ghcr.io/shop/checkout:sha-9c2b7e1f":             "9c2b7e1f",
		"localhost:5000/checkout:v1.4.2":                 "",
		"ghcr.io/shop/checkout@sha256:abc":               "",
		"registry:5000/shop/checkout:4d81f30@sha256:abc": "4d81f30",
	} {
		if got := Revision(d, tmpl(img)); got != want {
			t.Errorf("Revision(%s) = %q, want %q", img, got, want)
		}
	}
	d.Annotations = map[string]string{AnnotationRevision: "abc1234"}
	if got := Revision(d, tmpl("x:latest")); got != "abc1234" {
		t.Errorf("annotation: %s", got)
	}
}

func TestActor(t *testing.T) {
	t1, t2 := metav1.NewTime(time.Unix(100, 0)), metav1.NewTime(time.Unix(200, 0))
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{ManagedFields: []metav1.ManagedFieldsEntry{
		{Manager: "argocd-controller", Time: &t1},
		{Manager: "kubectl-edit", Time: &t2},
		{Manager: "kube-controller-manager", Subresource: "status", Time: &t2},
	}}}
	if a, human := Actor(d); a != "kubectl-edit" || !human {
		t.Errorf("got %s %v", a, human)
	}
	d.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kube-controller-manager", Subresource: "scale", Time: &t2}}
	if a, human := Actor(d); a != "horizontal-pod-autoscaler" || human {
		t.Errorf("got %s %v", a, human)
	}
}

type memSink struct {
	mu  sync.Mutex
	evs []event.Event
}

func (m *memSink) Ingest(_ context.Context, e *event.Event) (bool, error) {
	if err := e.Normalize(time.Now()); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evs = append(m.evs, *e)
	return true, nil
}

func (m *memSink) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.evs)
}

func (m *memSink) wait(t *testing.T, n int) []event.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		if len(m.evs) >= n {
			out := append([]event.Event(nil), m.evs...)
			m.mu.Unlock()
			return out
		}
		m.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d events, got %d", n, m.count())
	return nil
}

func TestCollectorWithFakeCluster(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-api", Namespace: "checkout", Generation: 1, ResourceVersion: "1"},
		Spec:       appsv1.DeploymentSpec{Replicas: i32(8), Template: tmpl("ghcr.io/shop/checkout:4d81f30")},
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "checkout-config", Namespace: "checkout", Labels: map[string]string{"app": "checkout-api"}},
		Data: map[string]string{"PAYMENT_TIMEOUT_MS": "3000"}}
	client := fake.NewClientset(dep, cm)
	sink := &memSink{}
	cfg := config.Default().Collectors.Kubernetes
	cfg.Cluster = "prod-eu"
	col := New(cfg, client, nil, sink, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- col.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !col.Ready() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !col.Ready() {
		t.Fatal("collector never synced")
	}

	// Pre-existing objects must not produce events.
	time.Sleep(300 * time.Millisecond) // let the fake watch start; it drops changes made before
	if n := sink.count(); n != 0 {
		t.Fatalf("initial list produced %d events", n)
	}

	upd := dep.DeepCopy()
	upd.Generation, upd.ResourceVersion = 2, "2"
	upd.Spec.Template = tmpl("ghcr.io/shop/checkout:9c2b7e1")
	if _, err := client.AppsV1().Deployments("checkout").Update(ctx, upd, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	cm2 := cm.DeepCopy()
	cm2.Data["PAYMENT_TIMEOUT_MS"] = "800"
	if _, err := client.CoreV1().ConfigMaps("checkout").Update(ctx, cm2, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	evs := sink.wait(t, 2)
	byKind := map[event.Kind]event.Event{}
	for _, e := range evs {
		byKind[e.Kind] = e
	}
	d := byKind[event.KindDeploy]
	if d.Summary != "Rollout checkout-api: checkout:9c2b7e1" || d.Revision != "9c2b7e1" || d.Origin != "prod-eu/checkout" || d.Env != "prod" {
		t.Errorf("deploy event: %+v", d)
	}
	c := byKind[event.KindConfig]
	if c.Service != "checkout-api" || c.Summary != "ConfigMap checkout-config changed (1 key)" || len(c.Diff) != 2 {
		t.Errorf("config event: %+v", c)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v", err)
	}
}
