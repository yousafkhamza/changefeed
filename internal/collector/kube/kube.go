// Package kube watches a Kubernetes cluster and records workload, ConfigMap
// and Helm release changes. It only uses get/list/watch.
package kube

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"

	"github.com/yousafkhamza/changefeed/internal/config"
	"github.com/yousafkhamza/changefeed/internal/event"
)

// Sink receives events; the server's Ingest satisfies it.
type Sink interface {
	Ingest(ctx context.Context, e *event.Event) (bool, error)
}

// Collector watches the cluster.
type Collector struct {
	cfg    config.Kubernetes
	client kubernetes.Interface
	meta   metadata.Interface // only needed for Helm releases
	sink   Sink
	log    *slog.Logger
	now    func() time.Time
	ready  atomic.Bool
}

// New builds a collector. meta may be nil when Helm watching is off.
func New(cfg config.Kubernetes, client kubernetes.Interface, meta metadata.Interface, sink Sink, log *slog.Logger) *Collector {
	return &Collector{cfg: cfg, client: client, meta: meta, sink: sink, log: log, now: time.Now}
}

// Ready reports whether the initial cache sync finished.
func (c *Collector) Ready() bool { return c.ready.Load() }

// Run starts informers and blocks until ctx is canceled.
func (c *Collector) Run(ctx context.Context) error {
	namespaces := c.cfg.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	var syncs []cache.InformerSynced
	for _, ns := range namespaces {
		f := informers.NewSharedInformerFactoryWithOptions(c.client, 0, informers.WithNamespace(ns))
		for _, w := range c.cfg.Watch {
			var inf cache.SharedIndexInformer
			switch w {
			case "deployments":
				inf = f.Apps().V1().Deployments().Informer()
			case "statefulsets":
				inf = f.Apps().V1().StatefulSets().Informer()
			case "daemonsets":
				inf = f.Apps().V1().DaemonSets().Informer()
			case "configmaps":
				inf = f.Core().V1().ConfigMaps().Informer()
			default:
				continue
			}
			if _, err := inf.AddEventHandler(c.handler(ctx)); err != nil {
				return fmt.Errorf("add %s handler: %w", w, err)
			}
			syncs = append(syncs, inf.HasSynced)
		}
		if slices.Contains(c.cfg.Watch, "helm") && c.meta != nil {
			mf := metadatainformer.NewFilteredSharedInformerFactory(c.meta, 0, ns, func(o *metav1.ListOptions) {
				o.LabelSelector = "owner=helm"
			})
			inf := mf.ForResource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).Informer()
			if _, err := inf.AddEventHandler(c.helmHandler(ctx)); err != nil {
				return fmt.Errorf("add helm handler: %w", err)
			}
			syncs = append(syncs, inf.HasSynced)
			mf.Start(ctx.Done())
		}
		f.Start(ctx.Done())
	}
	if !cache.WaitForCacheSync(ctx.Done(), syncs...) {
		return ctx.Err()
	}
	c.ready.Store(true)
	c.log.Info("kubernetes collector watching", "namespaces", namespaces, "kinds", c.cfg.Watch, "env", c.cfg.Env)
	<-ctx.Done()
	return nil
}

func (c *Collector) handler(ctx context.Context) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj any, initial bool) {
			if !initial {
				c.emitLifecycle(ctx, obj, "created", event.RiskLow)
			}
		},
		UpdateFunc: func(oldObj, newObj any) { c.onUpdate(ctx, oldObj, newObj) },
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			c.emitLifecycle(ctx, obj, "deleted", event.RiskHigh)
		},
	}
}

func (c *Collector) skip(obj metav1.Object) bool {
	if obj.GetLabels()[LabelIgnore] == "true" || slices.Contains(c.cfg.IgnoreNamespaces, obj.GetNamespace()) {
		return true
	}
	if obj.GetName() == "kube-root-ca.crt" || obj.GetName() == "istio-ca-root-cert" {
		return true
	}
	// Leader-election records change every few seconds and are not changes.
	_, leader := obj.GetAnnotations()["control-plane.alpha.kubernetes.io/leader"]
	return leader
}

func (c *Collector) onUpdate(ctx context.Context, oldObj, newObj any) {
	switch n := newObj.(type) {
	case *appsv1.Deployment:
		o := oldObj.(*appsv1.Deployment)
		if o.Generation == n.Generation {
			return // status-only update
		}
		c.workload(ctx, "Deployment", n, o.Spec.Template, n.Spec.Template, o.Spec.Replicas, n.Spec.Replicas)
	case *appsv1.StatefulSet:
		o := oldObj.(*appsv1.StatefulSet)
		if o.Generation == n.Generation {
			return
		}
		c.workload(ctx, "StatefulSet", n, o.Spec.Template, n.Spec.Template, o.Spec.Replicas, n.Spec.Replicas)
	case *appsv1.DaemonSet:
		o := oldObj.(*appsv1.DaemonSet)
		if o.Generation == n.Generation {
			return
		}
		c.workload(ctx, "DaemonSet", n, o.Spec.Template, n.Spec.Template, nil, nil)
	case *corev1.ConfigMap:
		o := oldObj.(*corev1.ConfigMap)
		if c.skip(n) || equality.Semantic.DeepEqual(o.Data, n.Data) && equality.Semantic.DeepEqual(o.BinaryData, n.BinaryData) {
			return
		}
		lines := DiffConfigMap(o.Data, n.Data, o.BinaryData, n.BinaryData, c.cfg.ConfigMapValues)
		e := c.base(n, event.KindConfig, event.RiskMedium, fmt.Sprintf("ConfigMap %s changed (%s)", n.Name, plural(countKeys(lines), "key")))
		e.Diff = lines
		e.Revision = n.Annotations[AnnotationRevision]
		c.send(ctx, e)
	}
}

func countKeys(lines []string) int {
	seen := map[string]bool{}
	for _, l := range lines {
		k := strings.TrimLeft(l, "+-~ ")
		if i := strings.IndexAny(k, ": ("); i > 0 {
			k = k[:i]
		}
		seen[k] = true
	}
	return len(seen)
}

func (c *Collector) workload(ctx context.Context, kindName string, obj metav1.Object, oldT, newT corev1.PodTemplateSpec, oldR, newR *int32) {
	if c.skip(obj) {
		return
	}
	ch, ok := DiffWorkload(kindName, obj.GetName(), oldT, newT, oldR, newR)
	if !ok {
		return
	}
	e := c.base(obj, ch.Kind, ch.Risk, ch.Summary)
	e.Diff = ch.Diff
	if ch.Kind == event.KindDeploy {
		e.Revision = Revision(obj, newT)
	}
	c.send(ctx, e)
}

func (c *Collector) emitLifecycle(ctx context.Context, obj any, verb string, risk event.Risk) {
	m, ok := obj.(metav1.Object)
	if !ok || c.skip(m) {
		return
	}
	var kindName string
	kind := event.KindDeploy
	switch obj.(type) {
	case *appsv1.Deployment:
		kindName = "Deployment"
	case *appsv1.StatefulSet:
		kindName = "StatefulSet"
	case *appsv1.DaemonSet:
		kindName = "DaemonSet"
	case *corev1.ConfigMap:
		kindName, kind = "ConfigMap", event.KindConfig
		if verb == "deleted" {
			risk = event.RiskMedium
		}
	default:
		return
	}
	e := c.base(m, kind, risk, fmt.Sprintf("%s %s %s", kindName, m.GetName(), verb))
	if verb == "deleted" {
		e.Actor = "" // managed fields describe the last update, not the delete
	}
	c.send(ctx, e)
}

func (c *Collector) helmHandler(ctx context.Context) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj any, initial bool) {
			if !initial {
				c.helm(ctx, nil, obj)
			}
		},
		UpdateFunc: func(oldObj, newObj any) { c.helm(ctx, oldObj, newObj) },
	}
}

// helm records a release revision once it reaches a final status. Helm
// stores each revision as a Secret labeled name/version/status; only that
// metadata is read, never the Secret data.
func (c *Collector) helm(ctx context.Context, oldObj, newObj any) {
	n, ok := newObj.(*metav1.PartialObjectMetadata)
	if !ok {
		return
	}
	status := n.Labels["status"]
	if o, ok := oldObj.(*metav1.PartialObjectMetadata); ok && o.Labels["status"] == status {
		return
	}
	if status != "deployed" && status != "failed" {
		return // pending-install, pending-upgrade, superseded, uninstalling
	}
	release, rev := n.Labels["name"], n.Labels["version"]
	risk := event.RiskMedium
	if status == "failed" {
		risk = event.RiskHigh
	}
	e := &event.Event{
		Source: "helm", Origin: c.origin(n.Namespace), Kind: event.KindDeploy, Service: strings.ToLower(release),
		Env: c.cfg.Env, Actor: "helm", Risk: risk, Time: c.now(),
		Summary: fmt.Sprintf("helm release %s revision %s %s", release, rev, status),
		ID:      event.DeriveID("helm", c.cfg.Cluster, n.Namespace, release, rev, status),
	}
	c.send(ctx, e)
}

func (c *Collector) origin(ns string) string {
	return strings.Trim(c.cfg.Cluster+"/"+ns, "/")
}

func (c *Collector) base(obj metav1.Object, kind event.Kind, risk event.Risk, summary string) *event.Event {
	actor, human := Actor(obj)
	return &event.Event{
		ID:     event.DeriveID("kubernetes", c.cfg.Cluster, obj.GetNamespace(), summary, obj.GetName(), obj.GetResourceVersion()),
		Source: "kubernetes", Origin: c.origin(obj.GetNamespace()), Kind: kind, Risk: risk,
		Service: ServiceName(obj), Env: c.cfg.Env, Actor: actor, OutsideCI: human, Summary: summary, Time: c.now(),
		Labels: map[string]string{"k8s.namespace": obj.GetNamespace(), "k8s.name": obj.GetName()},
	}
}

func (c *Collector) send(ctx context.Context, e *event.Event) {
	if _, err := c.sink.Ingest(ctx, e); err != nil {
		c.log.Warn("kubernetes event not stored", "summary", e.Summary, "err", err)
	}
}
