package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
	"github.com/yousafkhamza/changefeed/internal/redact"
)

// ErrIgnored means the payload is valid but is not a change worth storing
// (a read-only or failed API call). The handler answers 202.
var ErrIgnored = errors.New("ignored")

// cloudTrailEvent is an EventBridge "AWS API Call via CloudTrail" event.
type cloudTrailEvent struct {
	ID         string    `json:"id"`
	DetailType string    `json:"detail-type"`
	Account    string    `json:"account"`
	Region     string    `json:"region"`
	Time       time.Time `json:"time"`
	Detail     struct {
		EventID      string `json:"eventID"`
		EventSource  string `json:"eventSource"`
		EventName    string `json:"eventName"`
		AWSRegion    string `json:"awsRegion"`
		ErrorCode    string `json:"errorCode"`
		ReadOnly     *bool  `json:"readOnly"`
		UserAgent    string `json:"userAgent"`
		FromConsole  string `json:"sessionCredentialFromConsole"`
		Recipient    string `json:"recipientAccountId"`
		UserIdentity struct {
			Type     string `json:"type"`
			ARN      string `json:"arn"`
			UserName string `json:"userName"`
		} `json:"userIdentity"`
		RequestParameters map[string]any `json:"requestParameters"`
	} `json:"detail"`
}

// identifierKeys are request parameters that name the resource changed, in priority order.
var identifierKeys = []string{
	"dBInstanceIdentifier", "dBClusterIdentifier", "dBParameterGroupName", "dBClusterParameterGroupName",
	"clusterName", "nodegroupName", "functionName", "bucketName", "tableName", "cacheClusterId",
	"replicationGroupId", "loadBalancerName", "loadBalancerArn", "targetGroupArn", "autoScalingGroupName",
	"roleName", "policyArn", "userName", "groupName", "groupId", "instanceId", "repositoryName", "name",
}

var (
	reHighRisk = regexp.MustCompile(`^(Delete|Terminate|Stop|Reboot|Disable|Revoke|Detach|Remove|Deregister|Failover|Restore)`)
	reLowRisk  = regexp.MustCompile(`^(Create|Tag|Untag|Add.*Tags|Put.*Tag|Start|Copy)`)
	reScale    = regexp.MustCompile(`(DesiredCapacity|AutoScalingGroup|ScalingConfig|UpdateNodegroupConfig|Capacity$|SetDesired)`)
	reNotName  = regexp.MustCompile(`[^a-z0-9._-]+`)
)

// AWS maps an EventBridge CloudTrail event. accounts maps account IDs to envs.
func AWS(raw []byte, accounts map[string]string, defaultEnv string) (event.Event, error) {
	var ct cloudTrailEvent
	if err := json.Unmarshal(raw, &ct); err != nil {
		return event.Event{}, fmt.Errorf("decode cloudtrail event: %w", err)
	}
	d := ct.Detail
	if ct.DetailType != "AWS API Call via CloudTrail" || d.EventName == "" {
		return event.Event{}, fmt.Errorf("not a CloudTrail API call event (detail-type %q)", ct.DetailType)
	}
	if (d.ReadOnly != nil && *d.ReadOnly) || d.ErrorCode != "" || strings.HasPrefix(d.EventName, "Describe") ||
		strings.HasPrefix(d.EventName, "Get") || strings.HasPrefix(d.EventName, "List") {
		return event.Event{}, ErrIgnored
	}

	svc := strings.TrimSuffix(d.EventSource, ".amazonaws.com")
	resource := resourceName(d.RequestParameters)
	account := first(d.Recipient, ct.Account)
	region := first(d.AWSRegion, ct.Region)

	e := event.Event{
		Source: "aws", Origin: strings.Trim(account+"/"+region, "/"), Kind: event.KindInfra,
		Service: serviceName(first(resource, svc)), Env: first(accounts[account], defaultEnv),
		Actor: actor(d.UserIdentity.Type, d.UserIdentity.ARN, d.UserIdentity.UserName), Time: ct.Time,
		OutsideCI: d.FromConsole == "true" || strings.Contains(d.UserAgent, "console.amazonaws.com") ||
			strings.Contains(d.UserAgent, "console.aws.amazon.com"),
		Risk: risk(svc, d.EventName),
	}
	switch {
	case svc == "iam" || svc == "sts" || svc == "sso":
		e.Kind = event.KindAccess
	case reScale.MatchString(d.EventName):
		e.Kind = event.KindScale
	}
	on := ""
	if resource != "" {
		on = " on " + resource
	}
	how := ""
	if e.OutsideCI {
		how = " (console)"
	}
	e.Summary = fmt.Sprintf("%s %s%s%s", svc, d.EventName, on, how)
	e.Diff = flatten("", d.RequestParameters, nil)
	if len(e.Diff) > 20 {
		e.Diff = append(e.Diff[:19], fmt.Sprintf("… %d more parameters", len(e.Diff)-19))
	}
	e.ID = "aws_" + event.DeriveID(first(d.EventID, ct.ID))[4:]
	e.Labels = map[string]string{"aws.account": account, "aws.region": region, "aws.event": d.EventName}
	return e, nil
}

func risk(svc, name string) event.Risk {
	switch {
	case reHighRisk.MatchString(name):
		return event.RiskHigh
	case svc == "iam" && !strings.HasPrefix(name, "Tag") && !strings.HasPrefix(name, "Untag"):
		return event.RiskHigh
	case strings.Contains(name, "SecurityGroupIngress") || strings.Contains(name, "SecurityGroupEgress"):
		return event.RiskHigh
	case svc == "rds" && strings.HasPrefix(name, "Modify"):
		return event.RiskHigh
	case reLowRisk.MatchString(name):
		return event.RiskLow
	}
	return event.RiskMedium
}

func resourceName(params map[string]any) string {
	for _, k := range identifierKeys {
		if s, ok := params[k].(string); ok && s != "" {
			if i := strings.LastIndexAny(s, "/:"); i >= 0 && strings.HasPrefix(s, "arn:") {
				s = s[i+1:]
			}
			return s
		}
	}
	return ""
}

func serviceName(s string) string {
	s = strings.Trim(reNotName.ReplaceAllString(strings.ToLower(s), "-"), "-._")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-._")
	}
	if s == "" {
		return "aws"
	}
	return s
}

// actor gives a readable name without exposing access key IDs.
func actor(typ, arn, user string) string {
	switch typ {
	case "Root":
		return "root"
	case "IAMUser":
		return first(user, lastSegment(arn))
	case "AssumedRole":
		// arn:aws:sts::123:assumed-role/RoleName/session
		if _, rest, ok := strings.Cut(arn, ":assumed-role/"); ok {
			return rest
		}
	case "AWSService":
		return "aws-service"
	}
	return first(lastSegment(arn), strings.ToLower(typ))
}

func lastSegment(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return ""
}

// flatten turns request parameters into sorted "key: value" lines with
// sensitive values masked.
func flatten(prefix string, v any, out []string) []string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			out = flatten(p, t[k], out)
		}
	case []any:
		for i, item := range t {
			out = flatten(fmt.Sprintf("%s[%d]", prefix, i), item, out)
		}
	default:
		if prefix != "" {
			out = append(out, "+ "+prefix+": "+redact.Value(prefix, fmt.Sprint(t)))
		}
	}
	return out
}
