// Package auth decides who is calling and what they may do.
//
// Credential types:
//   - bearer tokens, stored only as SHA-256 hashes in the config
//   - GitHub Actions OIDC tokens, so CI pipelines need no stored secret
//   - SSO sessions for people using the web UI (see sso.go)
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/yousafkhamza/changefeed/internal/config"
)

// Scopes.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
)

// Errors returned by Authenticate. Messages are safe to show to clients.
var (
	ErrNoCredentials = errors.New("missing bearer token")
	ErrInvalid       = errors.New("invalid or expired credentials")
)

// Principal is an authenticated caller.
type Principal struct {
	Name    string
	Type    string // token, github, sso, anonymous
	Scopes  []string
	Sources []string // lanes this principal may write; empty means any
	// Envs and Services limit what the principal can read; nil means all.
	Envs     []string
	Services []string
	GitHub   *GitHubClaims
}

// Can reports whether the principal holds a scope.
func (p Principal) Can(scope string) bool { return slices.Contains(p.Scopes, scope) }

// CanWriteSource reports whether the principal may write events for a lane.
func (p Principal) CanWriteSource(lane string) bool {
	return p.Can(ScopeWrite) && (len(p.Sources) == 0 || slices.Contains(p.Sources, lane))
}

// SeesEnv reports whether the principal may read an env.
func (p Principal) SeesEnv(env string) bool { return len(p.Envs) == 0 || slices.Contains(p.Envs, env) }

// SeesService reports whether the principal may read a service.
func (p Principal) SeesService(s string) bool {
	return len(p.Services) == 0 || slices.Contains(p.Services, s)
}

// Sees reports whether the principal may read something in env/service.
func (p Principal) Sees(env, service string) bool { return p.SeesEnv(env) && p.SeesService(service) }

// Scoped reports whether reads are limited at all.
func (p Principal) Scoped() bool { return len(p.Envs) > 0 || len(p.Services) > 0 }

// GitHubLanes are the lanes a GitHub Actions workflow may write.
var GitHubLanes = []string{"github", "terraform", "flags", "helm"}

// Authenticator checks requests against the configured credentials.
type Authenticator struct {
	tokens        []config.Token
	github        *GitHubVerifier
	sso           *SSO
	anonymousRead bool
}

// New builds an Authenticator. github and sso may be nil when disabled.
func New(cfg config.Auth, github *GitHubVerifier, sso *SSO) *Authenticator {
	return &Authenticator{tokens: cfg.Tokens, github: github, sso: sso, anonymousRead: cfg.AnonymousRead}
}

// SSOEnabled reports whether people can sign in through the identity provider.
func (a *Authenticator) SSOEnabled() bool { return a.sso != nil }

// SSO returns the single sign-on handler, or nil.
func (a *Authenticator) SSO() *SSO { return a.sso }

// Authenticate identifies the caller: bearer credentials first, then an SSO
// session cookie, then anonymous read access if enabled.
func (a *Authenticator) Authenticate(r *http.Request) (Principal, error) {
	raw, ok := bearer(r)
	if !ok {
		if a.sso != nil {
			if p, ok := a.sso.Session(r); ok {
				return p, nil
			}
		}
		if a.anonymousRead {
			return Principal{Name: "anonymous", Type: "anonymous", Scopes: []string{ScopeRead}}, nil
		}
		return Principal{}, ErrNoCredentials
	}
	if a.github != nil && looksLikeJWT(raw) {
		claims, err := a.github.Verify(r.Context(), raw)
		if err != nil {
			return Principal{}, ErrInvalid
		}
		return Principal{Name: "github:" + claims.Repository, Type: "github", Scopes: []string{ScopeWrite},
			Sources: GitHubLanes, GitHub: claims}, nil
	}
	sum := sha256.Sum256([]byte(raw))
	got := hex.EncodeToString(sum[:])
	var match *config.Token
	for i := range a.tokens { // compare every token so timing does not reveal which one matched
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(a.tokens[i].SHA256)), []byte(got)) == 1 {
			match = &a.tokens[i]
		}
	}
	if match == nil {
		return Principal{}, ErrInvalid
	}
	return Principal{Name: match.Name, Type: "token", Scopes: match.Scopes, Sources: match.Sources,
		Envs: match.Envs, Services: match.Services}, nil
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

func looksLikeJWT(s string) bool { return strings.Count(s, ".") == 2 && strings.HasPrefix(s, "eyJ") }

// NewToken returns a random token and the SHA-256 hash to put in the config.
func NewToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = "cf_" + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
