package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousafkhamza/changefeed/internal/config"
)

const issuer = "https://token.actions.githubusercontent.com"

type fakeIssuer struct {
	key     *rsa.PrivateKey
	kid     string
	srv     *httptest.Server
	fetches atomic.Int32
}

func newIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: k, kid: "k1"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.fetches.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": f.kid, "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
		}}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) sign(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	head := enc(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	body := enc(claims)
	d := sha256.Sum256([]byte(head + "." + body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

var now = time.Date(2026, 11, 24, 14, 0, 0, 0, time.UTC)

func claims(mutate ...func(map[string]any)) map[string]any {
	c := map[string]any{
		"iss": issuer, "aud": "changefeed", "exp": now.Add(5 * time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(),
		"repository": "shop/checkout", "repository_owner": "shop", "actor": "n.rahman", "sha": "9c2b7e1f00aa",
		"run_id": "42", "environment": "prod",
	}
	for _, m := range mutate {
		m(c)
	}
	return c
}

func verifier(f *fakeIssuer, repos ...string) *GitHubVerifier {
	v := NewGitHubVerifier(issuer, "changefeed", repos, nil)
	v.SetJWKSURL(f.srv.URL)
	v.Now = func() time.Time { return now }
	return v
}

func TestGitHubVerifyAccepts(t *testing.T) {
	f := newIssuer(t)
	v := verifier(f, "shop/checkout")
	c, err := v.Verify(context.Background(), f.sign(t, "k1", claims()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Actor != "n.rahman" || c.RunURL() != "https://github.com/shop/checkout/actions/runs/42" {
		t.Errorf("claims: %+v", c)
	}
	// Second call uses the cached JWKS.
	if _, err := v.Verify(context.Background(), f.sign(t, "k1", claims())); err != nil {
		t.Fatal(err)
	}
	if n := f.fetches.Load(); n != 1 {
		t.Errorf("jwks fetched %d times, want 1", n)
	}
}

func TestGitHubVerifyRejects(t *testing.T) {
	f := newIssuer(t)
	other := newIssuer(t)
	cases := map[string]string{
		"wrong audience": f.sign(t, "k1", claims(func(c map[string]any) { c["aud"] = "sigstore" })),
		"expired":        f.sign(t, "k1", claims(func(c map[string]any) { c["exp"] = now.Add(-2 * time.Minute).Unix() })),
		"not yet valid":  f.sign(t, "k1", claims(func(c map[string]any) { c["nbf"] = now.Add(5 * time.Minute).Unix() })),
		"wrong issuer":   f.sign(t, "k1", claims(func(c map[string]any) { c["iss"] = "https://evil.example.com" })),
		"other repo":     f.sign(t, "k1", claims(func(c map[string]any) { c["repository"] = "attacker/fork" })),
		"forged":         other.sign(t, "k1", claims()),
		"unknown kid":    f.sign(t, "k9", claims()),
		"tampered":       strings.Replace(f.sign(t, "k1", claims()), ".", ".x", 1),
		"alg none":       base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1"}`)) + ".e30.",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier(f, "shop/checkout").Verify(context.Background(), tok); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestGitHubOrgWildcardAndEnvironment(t *testing.T) {
	f := newIssuer(t)
	v := verifier(f, "shop/*")
	if _, err := v.Verify(context.Background(), f.sign(t, "k1", claims(func(c map[string]any) { c["repository"] = "shop/cart" }))); err != nil {
		t.Fatalf("org wildcard: %v", err)
	}
	v.Environments = []string{"prod"}
	if _, err := v.Verify(context.Background(), f.sign(t, "k1", claims(func(c map[string]any) { c["environment"] = "dev" }))); err == nil {
		t.Fatal("environment policy not enforced")
	}
	// aud as an array is valid JWT.
	if _, err := v.Verify(context.Background(), f.sign(t, "k1", claims(func(c map[string]any) { c["aud"] = []string{"x", "changefeed"} }))); err != nil {
		t.Fatalf("array aud: %v", err)
	}
}

func TestAuthenticateTokens(t *testing.T) {
	tok, hash, err := NewToken()
	if err != nil || !strings.HasPrefix(tok, "cf_") || len(hash) != 64 {
		t.Fatalf("NewToken: %q %q %v", tok, hash, err)
	}
	a := New(config.Auth{Tokens: []config.Token{{Name: "argocd", SHA256: strings.ToUpper(hash), Scopes: []string{"write"}, Sources: []string{"argocd"}}}}, nil, nil)

	req := func(h string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if h != "" {
			r.Header.Set("Authorization", h)
		}
		return r
	}
	p, err := a.Authenticate(req("Bearer " + tok))
	if err != nil || p.Name != "argocd" {
		t.Fatalf("valid token rejected: %v", err)
	}
	if !p.CanWriteSource("argocd") || p.CanWriteSource("aws") || p.Can(ScopeRead) {
		t.Error("scopes/sources not enforced")
	}
	if _, err := a.Authenticate(req("Bearer wrong")); !errors.Is(err, ErrInvalid) {
		t.Errorf("wrong token: %v", err)
	}
	if _, err := a.Authenticate(req("")); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("no token: %v", err)
	}
	if _, err := a.Authenticate(req("Basic " + tok)); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("basic scheme must not be accepted: %v", err)
	}
	anon := New(config.Auth{AnonymousRead: true}, nil, nil)
	p, err = anon.Authenticate(req(""))
	if err != nil || !p.Can(ScopeRead) || p.Can(ScopeWrite) {
		t.Errorf("anonymous read: %+v %v", p, err)
	}
}

func TestAuthenticateGitHub(t *testing.T) {
	f := newIssuer(t)
	a := New(config.Auth{}, verifier(f, "shop/checkout"), nil)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer "+f.sign(t, "k1", claims()))
	p, err := a.Authenticate(r)
	if err != nil || p.Type != "github" || !p.CanWriteSource("github") || p.CanWriteSource("aws") || p.Can(ScopeRead) {
		t.Fatalf("github principal: %+v %v", p, err)
	}
}
