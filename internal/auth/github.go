package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
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

// audience accepts the "aud" claim as a string or an array.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// GitHubVerifier validates GitHub Actions OIDC tokens against the issuer's JWKS.
type GitHubVerifier struct {
	Issuer       string
	JWKSURL      string
	Audience     string
	Repositories []string
	Environments []string
	Client       *http.Client
	Now          func() time.Time
	Leeway       time.Duration

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// NewGitHubVerifier returns a verifier for issuer (default GitHub.com).
func NewGitHubVerifier(issuer, aud string, repos, envs []string) *GitHubVerifier {
	issuer = strings.TrimRight(issuer, "/")
	return &GitHubVerifier{
		Issuer: issuer, JWKSURL: issuer + "/.well-known/jwks", Audience: aud, Repositories: repos, Environments: envs,
		Client: &http.Client{Timeout: 10 * time.Second}, Now: time.Now, Leeway: time.Minute,
	}
}

// Verify checks signature, issuer, audience, lifetime and repository policy.
func (v *GitHubVerifier) Verify(ctx context.Context, token string) (*GitHubClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	if hdr.Alg != "RS256" {
		return nil, fmt.Errorf("unsupported alg %q", hdr.Alg)
	}
	key, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("bad signature encoding")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, errors.New("bad signature")
	}
	var c GitHubClaims
	if err := decodeSegment(parts[1], &c); err != nil {
		return nil, fmt.Errorf("claims: %w", err)
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

// key returns the signing key, refreshing the JWKS when the kid is unknown
// (key rotation) or the cache is older than an hour. Refreshes are limited
// to one a minute so bad tokens cannot be used to hammer the issuer.
func (v *GitHubVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.Now()
	if k, ok := v.keys[kid]; ok && now.Sub(v.fetchedAt) < time.Hour {
		return k, nil
	}
	if now.Sub(v.fetchedAt) < time.Minute && v.keys != nil {
		if k, ok := v.keys[kid]; ok {
			return k, nil
		}
		return nil, errors.New("unknown signing key")
	}
	keys, err := v.fetch(ctx)
	if err != nil {
		if k, ok := v.keys[kid]; ok { // issuer down: keep using a key we already trust
			return k, nil
		}
		return nil, err
	}
	v.keys, v.fetchedAt = keys, now
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}

func (v *GitHubVerifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch jwks: status %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(out) == 0 {
		return nil, errors.New("jwks has no usable RSA keys")
	}
	return out, nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
