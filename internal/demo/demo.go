// Package demo loads a realistic sample incident so people can try
// changefeed with one command: `changefeed serve --demo`.
package demo

import (
	"context"
	"fmt"
	"time"

	"github.com/yousafkhamza/changefeed/internal/config"
	"github.com/yousafkhamza/changefeed/internal/event"
)

// Sink stores events (the server's Ingest).
type Sink interface {
	Ingest(ctx context.Context, e *event.Event) (bool, error)
}

type row struct {
	minsBefore                                       int
	source, kind, service, env, actor, summary, risk string
	rev, changeset                                   string
	outside                                          bool
	diff                                             []string
}

// IncidentOffset is how long ago the sample alert fired.
const IncidentOffset = 8 * time.Minute

var rows = []row{
	{111, "github", "deploy", "search", "prod", "d.ivanova", "PR #398 merged: tune search ranking", "low", "3f9a1c2d5e", "gh:shop/search#398", false, nil},
	{105, "argocd", "deploy", "search", "prod", "argocd (auto-sync)", "Synced search to 3f9a1c2", "low", "3f9a1c2d5e", "", false, nil},
	{94, "terraform", "infra", "platform", "prod", "terraform-ci", "Terraform apply: 0 to add, 1 to change, 0 to replace, 0 to destroy", "medium", "", "", false, []string{"~ module.eks.aws_eks_node_group.general"}},
	{87, "flags", "flag", "web", "prod", "m.silva", "Flag new-nav enabled for 25% of users", "low", "", "", false, []string{"~ rollout: 0% → 25%"}},
	{80, "kubernetes", "scale", "cart", "prod", "horizontal-pod-autoscaler", "Scaled cart 4 → 7 replicas", "low", "", "", false, []string{"~ replicas: 4 → 7"}},
	{72, "aws", "access", "ci-deployer", "prod", "PlatformAdmin/a.okafor", "iam AttachRolePolicy on ci-deployer (console)", "high", "", "", true, []string{"+ policyArn: arn:aws:iam::aws:policy/AmazonRDSFullAccess", "+ roleName: ci-deployer"}},
	{58, "github", "deploy", "payments-api", "prod", "j.mensah", "PR #407 merged: retry budget for PSP calls", "medium", "a17e0d4b9c", "gh:shop/payments#407", false, nil},
	{51, "argocd", "deploy", "payments-api", "prod", "argocd (auto-sync)", "Synced payments-api to a17e0d4", "medium", "a17e0d4b9c", "", false, nil},
	{49, "kubernetes", "deploy", "payments-api", "prod", "argocd-controller", "Rollout payments-api: payments:a17e0d4", "medium", "a17e0d4", "", false, []string{"~ image app: ghcr.io/shop/payments:5be21aa → ghcr.io/shop/payments:a17e0d4"}},
	{40, "flags", "flag", "checkout-api", "prod", "m.silva", "Flag fast-checkout ramped 10% → 50%", "medium", "", "", true, []string{"~ rollout: 10% → 50%"}},
	{30, "kubernetes", "config", "checkout-api", "prod", "kubectl-edit", "ConfigMap checkout-config changed (2 keys)", "high", "9c2b7e1f00", "", true, []string{"- PAYMENT_TIMEOUT_MS: 3000", "+ PAYMENT_TIMEOUT_MS: 800", "+ PSP_BREAKER_THRESHOLD: 5"}},
	{26, "github", "deploy", "checkout-api", "prod", "n.rahman", "PR #412 merged: lower PSP timeout, add circuit breaker", "high", "9c2b7e1f00", "gh:shop/checkout#412", false, nil},
	{21, "argocd", "deploy", "checkout-api", "prod", "argocd (auto-sync)", "Synced checkout-api to 9c2b7e1", "high", "9c2b7e1f00", "", false, []string{"health: Progressing", "destination: https://kubernetes.default.svc checkout"}},
	{18, "kubernetes", "deploy", "checkout-api", "prod", "argocd-controller", "Rollout checkout-api: checkout:9c2b7e1", "medium", "9c2b7e1", "", false, []string{"~ image app: ghcr.io/shop/checkout:4d81f30 → ghcr.io/shop/checkout:9c2b7e1"}},
	{13, "aws", "infra", "orders-db", "prod", "PlatformAdmin/a.okafor", "rds ModifyDBParameterGroup on orders-db (console)", "high", "", "", true, []string{"+ dBParameterGroupName: orders-db", "+ parameters[0].parameterName: max_connections", "+ parameters[0].parameterValue: 400"}},
	{7, "terraform", "infra", "platform", "staging", "terraform-ci", "Terraform apply: 0 to add, 1 to change, 0 to replace, 0 to destroy", "medium", "", "", false, []string{"~ aws_elasticache_replication_group.cache"}},
	{4, "helm", "deploy", "web", "prod", "helm", "helm release web revision 31 deployed", "medium", "", "", false, nil},
	{-4, "kubernetes", "scale", "checkout-api", "prod", "horizontal-pod-autoscaler", "Scaled checkout-api 8 → 14 replicas", "low", "", "", false, []string{"~ replicas: 8 → 14"}},
	{-6, "flags", "flag", "checkout-api", "prod", "m.silva", "Flag fast-checkout rolled back to 10%", "low", "", "", false, []string{"~ rollout: 50% → 10%"}},
}

// Config returns dependencies that match the sample data.
func Config(cfg *config.Config) {
	if cfg.Services == nil {
		cfg.Services = map[string]config.Service{}
	}
	cfg.Services["checkout-api"] = config.Service{DependsOn: []string{"payments-api", "cart", "orders-db"}}
	cfg.PlatformServices = append(cfg.PlatformServices, "platform", "ci-deployer")
}

// Seed stores the sample events, timed so the alert fired IncidentOffset ago.
func Seed(ctx context.Context, sink Sink, now time.Time) error {
	at := now.Add(-IncidentOffset).Truncate(time.Minute)
	for i, r := range rows {
		e := &event.Event{
			ID: fmt.Sprintf("demo_%02d", i), Time: at.Add(-time.Duration(r.minsBefore) * time.Minute),
			Source: r.source, Origin: "demo", Kind: event.Kind(r.kind), Service: r.service, Env: r.env, Actor: r.actor,
			Summary: r.summary, Risk: event.Risk(r.risk), Revision: r.rev, ChangeSet: r.changeset, OutsideCI: r.outside, Diff: r.diff,
		}
		if r.changeset != "" {
			e.Links = map[string]string{"pr": "https://github.com/yousafkhamza/changefeed#demo"}
		}
		if _, err := sink.Ingest(ctx, e); err != nil {
			return fmt.Errorf("seed %s: %w", e.ID, err)
		}
	}
	return nil
}
