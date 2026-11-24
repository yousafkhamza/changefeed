package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// GitHubClaims are the OIDC claims changefeed uses.
// See https://docs.github.com/actions/security-for-github-actions/security-hardening-your-deployments/about-security-hardening-with-openid-connect
type GitHubClaims struct {
	Issuer          string   `json:"iss"`
	Audience        audience `json:"aud"`
	Expiry          int64    `json:"exp"`
	NotBefore       int64    `json:"nbf"`
	IssuedAt        int64    `json:"iat"`
	Repository      string   `json:"repository"`
	RepositoryOwner string   `json:"repository_owner"`
	Actor           string   `json:"actor"`
	Ref             string   `json:"ref"`
	SHA             string   `json:"sha"`
	Workflow        string   `json:"workflow"`
	EventName       string   `json:"event_name"`
	Environment     string   `json:"environment"`
	RunID           string   `json:"run_id"`
	RunAttempt      string   `json:"run_attempt"`
}

// RunURL links to the workflow run that sent the event.
func (c GitHubClaims) RunURL() string {
	if c.Repository == "" || c.RunID == "" {
		return ""
	}
	return "https://github.com/" + c.Repository + "/actions/runs/" + c.RunID
}

// GitHubVerifier validates GitHub Actions OIDC tokens against the issuer's JWKS.
type GitHubVerifier struct {
	Issuer       string
	Audience     string
	Repositories []string
	Environments []string
	Now          func() time.Time
	Leeway       time.Duration
	keys         *keySet
}

// NewGitHubVerifier returns a verifier for issuer (default GitHub.com).
func NewGitHubVerifier(issuer, aud string, repos, envs []string) *GitHubVerifier {
	issuer = strings.TrimRight(issuer, "/")
	v := &GitHubVerifier{Issuer: issuer, Audience: aud, Repositories: repos, Environments: envs, Now: time.Now, Leeway: time.Minute}
	v.keys = &keySet{url: issuer + "/.well-known/jwks", client: &http.Client{Timeout: 10 * time.Second}, now: func() time.Time { return v.Now() }}
	return v
}

// SetJWKSURL overrides where keys are fetched (tests, GHES).
func (v *GitHubVerifier) SetJWKSURL(u string) { v.keys.url = u }

// Verify checks signature, issuer, audience, lifetime and repository policy.
func (v *GitHubVerifier) Verify(ctx context.Context, token string) (*GitHubClaims, error) {
	var c GitHubClaims
	if err := verifyJWT(ctx, v.keys, token, &c); err != nil {
		return nil, err
	}
	now := v.Now()
	switch {
	case c.Issuer != v.Issuer:
		return nil, fmt.Errorf("issuer %q not trusted", c.Issuer)
	case !slices.Contains(c.Audience, v.Audience):
		return nil, errors.New("audience mismatch")
	case c.Expiry == 0 || now.After(time.Unix(c.Expiry, 0).Add(v.Leeway)):
		return nil, errors.New("token expired")
	case c.NotBefore != 0 && now.Add(v.Leeway).Before(time.Unix(c.NotBefore, 0)):
		return nil, errors.New("token not yet valid")
	case !repoAllowed(v.Repositories, c.Repository):
		return nil, fmt.Errorf("repository %q not allowed", c.Repository)
	case len(v.Environments) > 0 && !slices.Contains(v.Environments, c.Environment):
		return nil, fmt.Errorf("environment %q not allowed", c.Environment)
	}
	return &c, nil
}

func repoAllowed(allowed []string, repo string) bool {
	owner, _, ok := strings.Cut(repo, "/")
	if !ok {
		return false
	}
	for _, a := range allowed {
		if strings.EqualFold(a, repo) || strings.EqualFold(a, owner+"/*") {
			return true
		}
	}
	return false
}
