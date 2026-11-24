// Package config loads the server configuration from YAML and the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Config is the whole server configuration. Secrets never live here: tokens
// are stored as SHA-256 hashes, so the file can sit in a ConfigMap.
type Config struct {
	Listen     string     `json:"listen"`
	PublicURL  string     `json:"publicURL"`
	Storage    Storage    `json:"storage"`
	Auth       Auth       `json:"auth"`
	Collectors Collectors `json:"collectors"`
	Hooks      Hooks      `json:"hooks"`
	// Services maps a service to the services it calls, for ranking likely causes.
	Services map[string]Service `json:"services"`
	// PlatformServices are shared by everything (e.g. "platform", "network").
	PlatformServices []string `json:"platformServices"`
	Slack            Slack    `json:"slack"`
	// Freezes are change freeze windows; deploys check them via /api/v1/freeze.
	Freezes []Freeze `json:"freezes"`

	// Secrets come only from the environment (a Kubernetes Secret), never the file.
	Secrets Secrets `json:"-"`
}

// Secrets are read from environment variables by Load.
type Secrets struct {
	DatabaseURL      string // CHANGEFEED_DATABASE_URL (postgres://…)
	SlackBotToken    string // CHANGEFEED_SLACK_BOT_TOKEN
	SlackSigning     string // CHANGEFEED_SLACK_SIGNING_SECRET
	LaunchDarkly     string // CHANGEFEED_LAUNCHDARKLY_SECRET
	Flagsmith        string // CHANGEFEED_FLAGSMITH_SECRET
	OIDCClientSecret string // CHANGEFEED_OIDC_CLIENT_SECRET
	SessionKey       string // CHANGEFEED_SESSION_KEY (32+ random bytes, base64 or raw)
}

// Storage configures the database. Path is a SQLite file; set
// CHANGEFEED_DATABASE_URL to a postgres:// URL to use Postgres instead.
type Storage struct {
	Path      string   `json:"path"`
	Retention Duration `json:"retention"`
}

// DSN is what store.Open receives.
func (c *Config) DSN() string {
	if c.Secrets.DatabaseURL != "" {
		return c.Secrets.DatabaseURL
	}
	return c.Storage.Path
}

// Slack posts likely causes when an alert fires and answers /changefeed.
type Slack struct {
	Enabled bool   `json:"enabled"`
	Channel string `json:"channel"` // channel ID or #name for incident posts
	// MinScore is the lowest top score worth posting; below it a quiet
	// "no likely change found" message is sent instead.
	MinScore int    `json:"minScore"`
	APIURL   string `json:"apiURL"` // override for tests or Slack GovSlack
}

// Freeze is a change freeze window, either one-off (start/end) or weekly.
type Freeze struct {
	Name     string   `json:"name"`
	Reason   string   `json:"reason"`
	Envs     []string `json:"envs"`     // empty = all envs
	Services []string `json:"services"` // empty = all services
	Start    string   `json:"start"`    // RFC 3339, with End
	End      string   `json:"end"`
	Weekly   *Weekly  `json:"weekly"`
}

// Weekly repeats every week, e.g. Friday 16:00 to Monday 08:00.
type Weekly struct {
	From     string `json:"from"`     // "fri 16:00"
	To       string `json:"to"`       // "mon 08:00"
	Timezone string `json:"timezone"` // IANA name, default UTC
}

// OIDC signs people in to the web UI with the company identity provider.
type OIDC struct {
	Enabled     bool     `json:"enabled"`
	Issuer      string   `json:"issuer"`
	ClientID    string   `json:"clientID"`
	RedirectURL string   `json:"redirectURL"` // default: publicURL + /auth/callback
	Scopes      []string `json:"scopes"`
	GroupsClaim string   `json:"groupsClaim"`
	SessionTTL  Duration `json:"sessionTTL"`
}

// Team limits what members of identity-provider groups can see.
type Team struct {
	Name     string   `json:"name"`
	Groups   []string `json:"groups"`
	Envs     []string `json:"envs"`     // empty = all
	Services []string `json:"services"` // empty = all
}

// Auth configures who may read and write events.
type Auth struct {
	// AnonymousRead lets anyone who can reach the server read events. Use it
	// only behind an SSO proxy such as oauth2-proxy.
	AnonymousRead bool          `json:"anonymousRead"`
	Tokens        []Token       `json:"tokens"`
	GitHub        GitHubOIDC    `json:"github"`
	RateLimit     RateLimitSpec `json:"rateLimit"`
	OIDC          OIDC          `json:"oidc"`
	// Teams scope what SSO users see. Empty means every signed-in user sees everything.
	Teams []Team `json:"teams"`
}

// Token is a bearer token, stored as the hex SHA-256 of the secret.
type Token struct {
	Name     string   `json:"name"`
	SHA256   string   `json:"sha256"`
	Scopes   []string `json:"scopes"`   // read, write
	Sources  []string `json:"sources"`  // lanes this token may write; empty means any
	Envs     []string `json:"envs"`     // read scope: only these envs; empty means all
	Services []string `json:"services"` // read scope: only these services; empty means all
}

// GitHubOIDC trusts GitHub Actions OIDC tokens from the listed repositories.
type GitHubOIDC struct {
	Enabled      bool     `json:"enabled"`
	Audience     string   `json:"audience"`
	Repositories []string `json:"repositories"` // owner/repo, or owner/* for a whole org
	Environments []string `json:"environments"` // optional: only these GitHub environments
	Issuer       string   `json:"issuer"`       // override for GHES
}

// RateLimitSpec caps writes per client.
type RateLimitSpec struct {
	PerMinute int `json:"perMinute"`
}

// Collectors configures in-process pull collectors.
type Collectors struct {
	Kubernetes Kubernetes `json:"kubernetes"`
}

// Kubernetes configures the cluster collector.
type Kubernetes struct {
	Enabled    bool     `json:"enabled"`
	Env        string   `json:"env"`
	Cluster    string   `json:"cluster"`
	Namespaces []string `json:"namespaces"` // empty means all
	Watch      []string `json:"watch"`      // deployments, statefulsets, daemonsets, configmaps, helm
	// ConfigMapValues shows changed values (redacted). Off shows key names only.
	ConfigMapValues bool `json:"configMapValues"`
	// IgnoreNamespaces is skipped even when Namespaces is empty.
	IgnoreNamespaces []string `json:"ignoreNamespaces"`
}

// Hooks configures push sources.
type Hooks struct {
	ArgoCD       ArgoCD       `json:"argocd"`
	AWS          AWS          `json:"aws"`
	Alertmanager Alertmanager `json:"alertmanager"`
	Flags        Flags        `json:"flags"`
}

// Alertmanager turns firing alerts into incidents and ranks changes for them.
type Alertmanager struct {
	ServiceLabels []string `json:"serviceLabels"` // first label present names the service
	EnvLabels     []string `json:"envLabels"`
	DefaultEnv    string   `json:"defaultEnv"`
	Window        Duration `json:"window"` // how far back to look for causes
}

// Flags maps feature-flag provider names to changefeed services and envs.
type Flags struct {
	Services       map[string]string `json:"services"` // flag key -> service
	DefaultService string            `json:"defaultService"`
	Envs           map[string]string `json:"envs"` // provider env (e.g. "production") -> changefeed env
	DefaultEnv     string            `json:"defaultEnv"`
}

// Service returns the service a flag belongs to.
func (f Flags) Service(flag string) string {
	if s, ok := f.Services[flag]; ok {
		return s
	}
	return f.DefaultService
}

// Env maps a provider environment name.
func (f Flags) Env(providerEnv string) string {
	if e, ok := f.Envs[providerEnv]; ok {
		return e
	}
	if f.DefaultEnv != "" {
		return f.DefaultEnv
	}
	return providerEnv
}

// ArgoCD maps Argo CD applications to environments.
type ArgoCD struct {
	DefaultEnv string `json:"defaultEnv"`
}

// AWS maps account IDs to environments.
type AWS struct {
	Accounts   map[string]string `json:"accounts"`
	DefaultEnv string            `json:"defaultEnv"`
}

// Service lists dependencies of one service.
type Service struct {
	DependsOn []string `json:"dependsOn"`
}

// Duration is a time.Duration that reads "30d", "12h" or "90m" from YAML.
type Duration struct{ time.Duration }

// UnmarshalJSON parses Go durations plus a "d" (days) suffix.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalJSON writes the duration in Go syntax.
func (d Duration) MarshalJSON() ([]byte, error) { return []byte(`"` + d.String() + `"`), nil }

// ParseDuration accepts Go durations and whole days such as "30d".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(days, "%d", &n); err != nil || n < 0 || fmt.Sprint(n) != days {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return v, nil
}

// Default returns a configuration that is safe to start with.
func Default() Config {
	return Config{
		Listen:  ":8080",
		Storage: Storage{Path: "changefeed.db", Retention: Duration{30 * 24 * time.Hour}},
		Auth: Auth{
			GitHub:    GitHubOIDC{Audience: "changefeed", Issuer: "https://token.actions.githubusercontent.com"},
			RateLimit: RateLimitSpec{PerMinute: 600},
		},
		Collectors: Collectors{Kubernetes: Kubernetes{
			Env: "prod", Watch: []string{"deployments", "statefulsets", "daemonsets", "configmaps"}, ConfigMapValues: true,
			IgnoreNamespaces: []string{"kube-system", "kube-public", "kube-node-lease"},
		}},
		Hooks: Hooks{
			ArgoCD: ArgoCD{DefaultEnv: "prod"}, AWS: AWS{DefaultEnv: "prod"},
			Alertmanager: Alertmanager{
				ServiceLabels: []string{"service", "app_kubernetes_io_name", "app", "deployment", "job"},
				EnvLabels:     []string{"env", "environment"}, DefaultEnv: "prod", Window: Duration{time.Hour},
			},
			Flags: Flags{DefaultService: "flags"},
		},
		Services: map[string]Service{},
		Slack:    Slack{Channel: "#incidents", MinScore: 40, APIURL: "https://slack.com/api"},
	}
}

// secretEnv lists environment variables that hold secrets.
var secretEnv = map[string]func(*Secrets) *string{
	"CHANGEFEED_DATABASE_URL":         func(s *Secrets) *string { return &s.DatabaseURL },
	"CHANGEFEED_SLACK_BOT_TOKEN":      func(s *Secrets) *string { return &s.SlackBotToken },
	"CHANGEFEED_SLACK_SIGNING_SECRET": func(s *Secrets) *string { return &s.SlackSigning },
	"CHANGEFEED_LAUNCHDARKLY_SECRET":  func(s *Secrets) *string { return &s.LaunchDarkly },
	"CHANGEFEED_FLAGSMITH_SECRET":     func(s *Secrets) *string { return &s.Flagsmith },
	"CHANGEFEED_OIDC_CLIENT_SECRET":   func(s *Secrets) *string { return &s.OIDCClientSecret },
	"CHANGEFEED_SESSION_KEY":          func(s *Secrets) *string { return &s.SessionKey },
}

// Load reads YAML from path (optional) over the defaults and applies env overrides.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	if v := os.Getenv("CHANGEFEED_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("CHANGEFEED_DB"); v != "" {
		cfg.Storage.Path = v
	}
	for name, field := range secretEnv {
		*field(&cfg.Secrets) = strings.TrimSpace(os.Getenv(name))
	}
	if cfg.Auth.OIDC.Enabled {
		o := &cfg.Auth.OIDC
		if o.RedirectURL == "" && cfg.PublicURL != "" {
			o.RedirectURL = strings.TrimRight(cfg.PublicURL, "/") + "/auth/callback"
		}
		if len(o.Scopes) == 0 {
			o.Scopes = []string{"openid", "email", "profile", "groups"}
		}
		if o.GroupsClaim == "" {
			o.GroupsClaim = "groups"
		}
		if o.SessionTTL.Duration == 0 {
			o.SessionTTL.Duration = 12 * time.Hour
		}
	}
	return cfg, cfg.Validate()
}

var (
	reHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reRepo  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/([A-Za-z0-9_.-]+|\*)$`)
)

// Validate reports every configuration problem at once.
func (c *Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen is required"))
	}
	if c.Storage.Path == "" {
		errs = append(errs, errors.New("storage.path is required"))
	}
	if c.Storage.Retention.Duration < time.Hour {
		errs = append(errs, errors.New("storage.retention must be at least 1h"))
	}
	names := map[string]bool{}
	for i, t := range c.Auth.Tokens {
		if t.Name == "" || names[t.Name] {
			errs = append(errs, fmt.Errorf("auth.tokens[%d]: name is required and must be unique", i))
		}
		names[t.Name] = true
		if !reHex64.MatchString(strings.ToLower(t.SHA256)) {
			errs = append(errs, fmt.Errorf("auth.tokens[%d] (%s): sha256 must be 64 hex chars; create one with `changefeed token`", i, t.Name))
		}
		if len(t.Scopes) == 0 {
			errs = append(errs, fmt.Errorf("auth.tokens[%d] (%s): scopes must include read and/or write", i, t.Name))
		}
		for _, s := range t.Scopes {
			if s != "read" && s != "write" {
				errs = append(errs, fmt.Errorf("auth.tokens[%d] (%s): unknown scope %q", i, t.Name, s))
			}
		}
	}
	if o := c.Auth.OIDC; o.Enabled {
		if !strings.HasPrefix(o.Issuer, "https://") {
			errs = append(errs, errors.New("auth.oidc.issuer must be an https:// URL"))
		}
		if o.ClientID == "" {
			errs = append(errs, errors.New("auth.oidc.clientID is required"))
		}
		if o.RedirectURL == "" {
			errs = append(errs, errors.New("auth.oidc.redirectURL (or publicURL) is required"))
		}
		if len(c.Secrets.SessionKey) < 32 {
			errs = append(errs, errors.New("auth.oidc needs CHANGEFEED_SESSION_KEY with at least 32 characters (e.g. openssl rand -base64 48)"))
		}
	}
	for i, tm := range c.Auth.Teams {
		if tm.Name == "" || len(tm.Groups) == 0 {
			errs = append(errs, fmt.Errorf("auth.teams[%d]: name and at least one group are required", i))
		}
	}
	if c.Slack.Enabled && (c.Secrets.SlackBotToken == "" || c.Slack.Channel == "") {
		errs = append(errs, errors.New("slack.enabled needs slack.channel and CHANGEFEED_SLACK_BOT_TOKEN"))
	}
	if c.Secrets.DatabaseURL != "" && !strings.HasPrefix(c.Secrets.DatabaseURL, "postgres://") && !strings.HasPrefix(c.Secrets.DatabaseURL, "postgresql://") {
		errs = append(errs, errors.New("CHANGEFEED_DATABASE_URL must start with postgres://"))
	}
	for i, f := range c.Freezes {
		if err := f.check(); err != nil {
			errs = append(errs, fmt.Errorf("freezes[%d] (%s): %w", i, f.Name, err))
		}
	}
	if c.Auth.GitHub.Enabled {
		if c.Auth.GitHub.Audience == "" {
			errs = append(errs, errors.New("auth.github.audience is required"))
		}
		if len(c.Auth.GitHub.Repositories) == 0 {
			errs = append(errs, errors.New("auth.github.repositories must list at least one owner/repo"))
		}
		for _, r := range c.Auth.GitHub.Repositories {
			if !reRepo.MatchString(r) {
				errs = append(errs, fmt.Errorf("auth.github.repositories: %q must be owner/repo or owner/*", r))
			}
		}
	}
	if c.Auth.RateLimit.PerMinute < 0 {
		errs = append(errs, errors.New("auth.rateLimit.perMinute must be >= 0"))
	}
	known := []string{"deployments", "statefulsets", "daemonsets", "configmaps", "helm"}
	for _, w := range c.Collectors.Kubernetes.Watch {
		if !slices.Contains(known, w) {
			errs = append(errs, fmt.Errorf("collectors.kubernetes.watch: unknown kind %q (want one of %v)", w, known))
		}
	}
	if c.Collectors.Kubernetes.Enabled && c.Collectors.Kubernetes.Env == "" {
		errs = append(errs, errors.New("collectors.kubernetes.env is required"))
	}
	return errors.Join(errs...)
}

// Dependencies returns the configured dependencies of a service.
func (c *Config) Dependencies(service string) []string { return c.Services[service].DependsOn }
