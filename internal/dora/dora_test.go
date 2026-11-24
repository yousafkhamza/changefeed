package dora

import (
	"testing"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
	"github.com/yousafkhamza/changefeed/internal/store"
)

func TestCompute(t *testing.T) {
	ev := func(id, svc, env, cs string, k event.Kind) event.Event {
		return event.Event{ID: id, Service: svc, Env: env, ChangeSet: cs, Kind: k}
	}
	events := []event.Event{
		// PR #412 is one deployment even though it has three deploy events.
		ev("1", "checkout-api", "prod", "gh:shop/checkout#412", event.KindDeploy),
		ev("2", "checkout-api", "prod", "gh:shop/checkout#412", event.KindDeploy),
		ev("3", "checkout-api", "prod", "gh:shop/checkout#412", event.KindConfig),
		ev("4", "checkout-api", "prod", "gh:shop/checkout#413", event.KindDeploy),
		ev("5", "checkout-api", "prod", "", event.KindDeploy),
		ev("6", "checkout-api", "prod", "", event.KindDeploy),
		ev("7", "payments-api", "prod", "gh:shop/payments#407", event.KindDeploy),
		ev("8", "cart", "prod", "", event.KindScale), // not a deployment
		ev("9", "checkout-api", "staging", "gh:shop/checkout#412", event.KindDeploy),
	}
	incidents := []store.Incident{
		{Service: "checkout-api", Env: "prod", TopChangeSet: "gh:shop/checkout#412", TopScore: 80},
		{Service: "checkout-api", Env: "prod", TopChangeSet: "gh:shop/payments#407", TopScore: 70}, // dependency caused it
		{Service: "checkout-api", Env: "prod", TopChangeSet: "gh:shop/checkout#413", TopScore: 20}, // too weak to count
		{Service: "cart", Env: "prod"},
	}
	got := Compute(events, incidents, 30*24*time.Hour, 50)
	byKey := map[string]Stats{}
	for _, s := range got {
		byKey[s.Env+"/"+s.Service] = s
	}
	c := byKey["prod/checkout-api"]
	if c.Deployments != 4 || c.FailedDeployments != 1 || c.ChangeFailureRate != 0.25 || c.Incidents != 3 || c.DeploysPerDay != 0.13 {
		t.Errorf("checkout-api: %+v", c)
	}
	p := byKey["prod/payments-api"]
	if p.Deployments != 1 || p.FailedDeployments != 1 || p.ChangeFailureRate != 1 || p.Incidents != 0 {
		t.Errorf("payments-api: %+v", p)
	}
	if s := byKey["staging/checkout-api"]; s.FailedDeployments != 0 || s.Deployments != 1 {
		t.Errorf("incidents in prod must not fail staging deploys: %+v", s)
	}
	if cart := byKey["prod/cart"]; cart.Deployments != 0 || cart.Incidents != 1 || cart.ChangeFailureRate != 0 {
		t.Errorf("cart: %+v", cart)
	}
	if got[0].Env != "prod" || got[len(got)-1].Env != "staging" {
		t.Error("results should be sorted by env then service")
	}
}
