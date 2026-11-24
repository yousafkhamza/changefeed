package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yousafkhamza/changefeed/internal/config"
)

type fakeIDP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	nonce  string
	groups []string
	aud    string
}

func newIDP(t *testing.T) *fakeIDP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIDP{key: k, aud: "changefeed-ui"}
	mux := http.NewServeMux()
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": idp.srv.URL, "authorization_endpoint": idp.srv.URL + "/authorize",
			"token_endpoint": idp.srv.URL + "/token", "jwks_uri": idp.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "idp1", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes())}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f := &fakeIssuer{key: k}
		tok := f.sign(t, "idp1", map[string]any{"iss": idp.srv.URL, "sub": "u1", "aud": idp.aud, "email": "ykh@example.com",
			"exp": time.Now().Add(time.Hour).Unix(), "nonce": idp.nonce, "groups": idp.groups})
		json.NewEncoder(w).Encode(map[string]string{"id_token": tok, "access_token": "x", "token_type": "Bearer"})
	})
	return idp
}

func ssoFor(idp *fakeIDP, teams []config.Team) *SSO {
	return NewSSO(config.OIDC{Enabled: true, Issuer: idp.srv.URL, ClientID: "changefeed-ui", RedirectURL: "https://cf.example.com/auth/callback",
		Scopes: []string{"openid", "email", "groups"}, GroupsClaim: "groups", SessionTTL: config.Duration{Duration: time.Hour}},
		teams, "secret", strings.Repeat("k", 32))
}

// signIn runs Login and Callback and returns the response of the callback.
func signIn(t *testing.T, s *SSO, idp *fakeIDP, code string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "changefeed-ui" || q.Get("state") == "" {
		t.Fatalf("authorize url: %s", loc)
	}
	idp.nonce = q.Get("nonce")
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), nil)
	for _, c := range rec.Result().Cookies() {
		cb.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	s.Callback(rec2, cb)
	return rec2.Result()
}

func sessionFrom(t *testing.T, s *SSO, res *http.Response) (Principal, bool) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie && c.MaxAge > 0 {
			if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
				t.Errorf("session cookie flags: %+v", c)
			}
			r.AddCookie(c)
		}
	}
	return s.Session(r)
}

func TestSSOFlowWithTeams(t *testing.T) {
	idp := newIDP(t)
	idp.groups = []string{"payments-oncall", "everyone"}
	s := ssoFor(idp, []config.Team{
		{Name: "payments", Groups: []string{"payments-oncall"}, Envs: []string{"prod"}, Services: []string{"payments-api", "checkout-api"}},
		{Name: "search", Groups: []string{"search-team"}, Services: []string{"search"}},
	})
	res := signIn(t, s, idp, "good-code")
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/" {
		t.Fatalf("callback: %d", res.StatusCode)
	}
	p, ok := sessionFrom(t, s, res)
	if !ok || p.Type != "sso" || p.Name != "ykh@example.com" || !p.Can(ScopeRead) || p.Can(ScopeWrite) {
		t.Fatalf("principal: %+v %v", p, ok)
	}
	if !p.Sees("prod", "checkout-api") || p.Sees("prod", "search") || p.Sees("staging", "checkout-api") {
		t.Errorf("team scope wrong: %+v", p)
	}
}

func TestSSOUnrestrictedAndDenied(t *testing.T) {
	idp := newIDP(t)
	idp.groups = []string{"platform"}
	s := ssoFor(idp, nil)
	p, ok := sessionFrom(t, s, signIn(t, s, idp, "good-code"))
	if !ok || p.Scoped() {
		t.Errorf("no teams configured should mean unrestricted: %+v %v", p, ok)
	}
	s2 := ssoFor(idp, []config.Team{{Name: "payments", Groups: []string{"payments-oncall"}}})
	if res := signIn(t, s2, idp, "good-code"); res.StatusCode != http.StatusForbidden {
		t.Errorf("user in no team: %d", res.StatusCode)
	}
}

func TestSSORejectsBadResponses(t *testing.T) {
	idp := newIDP(t)
	s := ssoFor(idp, nil)
	if res := signIn(t, s, idp, "bad-code"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad code: %d", res.StatusCode)
	}
	idp.aud = "someone-else"
	if res := signIn(t, s, idp, "good-code"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong audience: %d", res.StatusCode)
	}
	// Callback without the flow cookie (CSRF / another browser).
	rec := httptest.NewRecorder()
	s.Callback(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?code=good-code&state=x", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing flow cookie: %d", rec.Code)
	}
	// A tampered session cookie is ignored.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.seal(session{Email: "x", Expires: time.Now().Add(time.Hour).Unix()}) + "x"})
	if _, ok := s.Session(r); ok {
		t.Error("tampered cookie accepted")
	}
	// An expired session is ignored.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.seal(session{Email: "x", Expires: time.Now().Add(-time.Minute).Unix()})})
	if _, ok := s.Session(r); ok {
		t.Error("expired cookie accepted")
	}
}
