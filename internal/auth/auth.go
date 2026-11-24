// Package auth decides who is calling and what they may do.
//
// Two credential types are supported:
//   - bearer tokens, stored only as SHA-256 hashes in the config
//   - GitHub Actions OIDC tokens, so CI pipelines need no stored secret
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
	Type    string // token, github, anonymous
	Scopes  []string
	Sources []string // lanes this principal may write; empty means any
	GitHub  *GitHubClaims
}

// Can reports whether the principal holds a scope.
func (p Principal) Can(scope string) bool { return slices.Contains(p.Scopes, scope) }

// CanWriteSource reports whether the principal may write events for a lane.
func (p Principal) CanWriteSource(lane string) bool {
	return p.Can(ScopeWrite) && (len(p.Sources) == 0 || slices.Contains(p.Sources, lane))
}

// GitHubLanes are the lanes a GitHub Actions workflow may write.
var GitHubLanes = []string{"github", "terraform", "flags", "helm"}

// Authenticator checks requests against the configured credentials.
type Authenticator struct {
	tokens        []config.Token
	github        *GitHubVerifier
	anonymousRead bool
}

// New builds an Authenticator. github may be nil when OIDC is disabled.
func New(cfg config.Auth, github *GitHubVerifier) *Authenticator {
	return &Authenticator{tokens: cfg.Tokens, github: github, anonymousRead: cfg.AnonymousRead}
}

// Authenticate identifies the caller. With no Authorization header it returns
// an anonymous read-only principal when anonymousRead is on.
func (a *Authenticator) Authenticate(r *http.Request) (Principal, error) {
	raw, ok := bearer(r)
	if !ok {
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
	return Principal{Name: match.Name, Type: "token", Scopes: match.Scopes, Sources: match.Sources}, nil
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
