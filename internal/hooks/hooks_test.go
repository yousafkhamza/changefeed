package hooks

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
)

var now = time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)

func TestArgoCDSucceeded(t *testing.T) {
	raw := `{"app":"checkout-api","project":"shop","revision":"9c2b7e1f00aa11","phase":"Succeeded","health":"Healthy",
	  "destServer":"https://kubernetes.default.svc","destNamespace":"checkout","repoURL":"https://github.com/shop/deploy.git",
	  "initiatedBy":"","automated":true,"finishedAt":"2026-11-24T14:11:07Z","url":"https://argocd.example.com/applications/checkout-api",
	  "labels":{"changefeed/env":"prod"}}`
	e, err := ArgoCD([]byte(raw), "staging")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Normalize(now); err != nil {
		t.Fatal(err)
	}
	if e.Summary != "Synced checkout-api to 9c2b7e1" || e.Env != "prod" || e.Risk != event.RiskMedium || e.Actor != "argocd (auto-sync)" {
		t.Errorf("mapping: %+v", e)
	}
	if e.Links["commit"] != "https://github.com/shop/deploy/commit/9c2b7e1f00aa11" || e.Links["argocd"] == "" {
		t.Errorf("links: %v", e.Links)
	}
	if e.Time.Format(time.RFC3339) != "2026-11-24T14:11:07Z" {
		t.Errorf("time: %v", e.Time)
	}
	again, _ := ArgoCD([]byte(raw), "staging")
	if again.ID != e.ID {
		t.Error("same notification must produce the same ID")
	}
}

func TestArgoCDFailedAndDefaults(t *testing.T) {
	e, err := ArgoCD([]byte(`{"app":"cart","phase":"Failed","initiatedBy":"a.okafor","message":"one or more objects failed","url":"javascript:alert(1)",
	  "labels":{"app.kubernetes.io/name":"cart-api"}}`), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if e.Risk != event.RiskHigh || e.Service != "cart-api" || e.Env != "prod" || e.Actor != "a.okafor" || !strings.HasPrefix(e.Summary, "Sync failed for cart") {
		t.Errorf("mapping: %+v", e)
	}
	if _, ok := e.Links["argocd"]; ok {
		t.Error("non-http URL must be dropped")
	}
	if _, err := ArgoCD([]byte(`{"phase":"Succeeded"}`), "prod"); err == nil {
		t.Error("missing app must fail")
	}
}

const rdsEvent = `{
  "version":"0","id":"6a7e8feb-b491-4cf7-a9f1-bf3703467718","detail-type":"AWS API Call via CloudTrail","source":"aws.rds",
  "account":"111122223333","time":"2026-11-24T14:19:03Z","region":"eu-west-1",
  "detail":{"eventVersion":"1.09","eventID":"e1d2c3","eventSource":"rds.amazonaws.com","eventName":"ModifyDBParameterGroup",
    "awsRegion":"eu-west-1","readOnly":false,"sessionCredentialFromConsole":"true","recipientAccountId":"111122223333",
    "userAgent":"console.amazonaws.com",
    "userIdentity":{"type":"AssumedRole","arn":"arn:aws:sts::111122223333:assumed-role/PlatformAdmin/a.okafor","principalId":"AROAEXAMPLEID:a.okafor"},
    "requestParameters":{"dBParameterGroupName":"orders-db","parameters":[{"parameterName":"max_connections","parameterValue":"400","applyMethod":"immediate"}],
      "masterUserPassword":"hunter2"}}}`

func TestAWSRDSConsoleChange(t *testing.T) {
	e, err := AWS([]byte(rdsEvent), map[string]string{"111122223333": "prod"}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Normalize(now); err != nil {
		t.Fatal(err)
	}
	if e.Service != "orders-db" || e.Env != "prod" || e.Kind != event.KindInfra || e.Risk != event.RiskHigh || !e.OutsideCI {
		t.Errorf("mapping: %+v", e)
	}
	if e.Actor != "PlatformAdmin/a.okafor" || e.Summary != "rds ModifyDBParameterGroup on orders-db (console)" {
		t.Errorf("actor/summary: %q / %q", e.Actor, e.Summary)
	}
	joined := strings.Join(e.Diff, "\n")
	if !strings.Contains(joined, "parameters[0].parameterValue: 400") || strings.Contains(joined, "hunter2") {
		t.Errorf("diff: %s", joined)
	}
	if !strings.HasPrefix(e.ID, "aws_") {
		t.Errorf("id: %s", e.ID)
	}
}

func TestAWSIgnoresReadOnlyAndFailed(t *testing.T) {
	ro := strings.Replace(rdsEvent, `"readOnly":false`, `"readOnly":true`, 1)
	if _, err := AWS([]byte(ro), nil, "prod"); !errors.Is(err, ErrIgnored) {
		t.Errorf("read-only: %v", err)
	}
	failed := strings.Replace(rdsEvent, `"readOnly":false`, `"readOnly":false,"errorCode":"AccessDenied"`, 1)
	if _, err := AWS([]byte(failed), nil, "prod"); !errors.Is(err, ErrIgnored) {
		t.Errorf("failed call: %v", err)
	}
	if _, err := AWS([]byte(`{"detail-type":"EC2 Instance State-change Notification","detail":{}}`), nil, "prod"); err == nil || errors.Is(err, ErrIgnored) {
		t.Errorf("wrong detail-type must be a client error: %v", err)
	}
}

func TestAWSKindsAndRisk(t *testing.T) {
	cases := []struct {
		svc, name string
		kind      event.Kind
		risk      event.Risk
	}{
		{"iam", "AttachRolePolicy", event.KindAccess, event.RiskHigh},
		{"ec2", "AuthorizeSecurityGroupIngress", event.KindInfra, event.RiskHigh},
		{"autoscaling", "SetDesiredCapacity", event.KindScale, event.RiskMedium},
		{"ec2", "TerminateInstances", event.KindInfra, event.RiskHigh},
		{"s3", "CreateBucket", event.KindInfra, event.RiskLow},
	}
	for _, c := range cases {
		raw := strings.NewReplacer("rds.amazonaws.com", c.svc+".amazonaws.com", "ModifyDBParameterGroup", c.name).Replace(rdsEvent)
		e, err := AWS([]byte(raw), nil, "prod")
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind != c.kind || e.Risk != c.risk {
			t.Errorf("%s %s: got %s/%s want %s/%s", c.svc, c.name, e.Kind, e.Risk, c.kind, c.risk)
		}
	}
}

func TestActorNames(t *testing.T) {
	if got := actor("IAMUser", "arn:aws:iam::1:user/deploy-bot", ""); got != "deploy-bot" {
		t.Error(got)
	}
	if got := actor("Root", "arn:aws:iam::1:root", ""); got != "root" {
		t.Error(got)
	}
	if got := serviceName("Orders DB (Primary)"); got != "orders-db-primary" {
		t.Error(got)
	}
}
