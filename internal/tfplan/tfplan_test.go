package tfplan

import (
	"errors"
	"testing"

	"github.com/yousafkhamza/changefeed/internal/event"
)

func TestParseTerraformPlan(t *testing.T) {
	raw := `{"format_version":"1.2","resource_changes":[
	 {"address":"aws_s3_bucket.logs","change":{"actions":["create"]}},
	 {"address":"aws_db_instance.orders","change":{"actions":["delete","create"]}},
	 {"address":"aws_security_group.api","change":{"actions":["update"]}},
	 {"address":"aws_iam_role.old","change":{"actions":["delete"]}},
	 {"address":"data.aws_caller_identity.me","change":{"actions":["read"]}},
	 {"address":"aws_vpc.main","change":{"actions":["no-op"]}}]}`
	s, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if s.Create != 1 || s.Replace != 1 || s.Update != 1 || s.Delete != 1 || s.Risk != event.RiskHigh {
		t.Errorf("summary: %+v", s)
	}
	if s.Lines[0] != "-/+ aws_db_instance.orders" || s.Lines[1] != "- aws_iam_role.old" || len(s.Lines) != 4 {
		t.Errorf("lines must list destructive changes first: %v", s.Lines)
	}
	if s.Text() != "Terraform apply: 1 to add, 1 to change, 1 to replace, 1 to destroy" {
		t.Errorf("text: %s", s.Text())
	}
}

func TestParseTerraformRiskLevels(t *testing.T) {
	upd, _ := Parse([]byte(`{"format_version":"1.2","resource_changes":[{"address":"a.b","change":{"actions":["update"]}}]}`))
	add, _ := Parse([]byte(`{"format_version":"1.2","resource_changes":[{"address":"a.b","change":{"actions":["create"]}}]}`))
	if upd.Risk != event.RiskMedium || add.Risk != event.RiskLow {
		t.Errorf("risks: %s %s", upd.Risk, add.Risk)
	}
	if _, err := Parse([]byte(`{"format_version":"1.2","resource_changes":[]}`)); !errors.Is(err, ErrNoChanges) {
		t.Errorf("empty plan: %v", err)
	}
}

func TestParsePlanlens(t *testing.T) {
	raw := `{"schemaVersion":1,"tool":{"name":"planlens","version":"1.0.0"},"passed":false,"maxScore":82,
	 "reports":[{"name":"plan.json","level":"critical","score":82,"summary":{"create":3,"update":4,"delete":2,"replace":1},
	  "findings":[{"severity":"critical","title":"Database will be replaced","address":"aws_db_instance.payments"}]}]}`
	s, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !s.FromPlanlens || s.Score != 82 || s.Risk != event.RiskHigh || s.Total() != 10 {
		t.Errorf("summary: %+v", s)
	}
	if s.Lines[0] != "! CRITICAL aws_db_instance.payments: Database will be replaced" {
		t.Errorf("lines: %v", s.Lines)
	}
	if s.Text() != "Terraform apply: 3 to add, 4 to change, 1 to replace, 2 to destroy (planlens 82/100)" {
		t.Errorf("text: %s", s.Text())
	}
}

func TestParseRejectsOtherJSON(t *testing.T) {
	for _, raw := range []string{`not json`, `{"hello":"world"}`} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%q should fail", raw)
		}
	}
}
