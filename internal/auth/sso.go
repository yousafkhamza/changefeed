package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yousafkhamza/changefeed/internal/config"
)

const (
	sessionCookie = "cf_session"
	flowCookie    = "cf_oidc"
)

// SSO signs people in to the web UI with OpenID Connect (authorization code
// flow with PKCE) and keeps them signed in with an HMAC-signed cookie. Team
// membership comes from the identity provider's groups claim.
type SSO struct {
	cfg          config.OIDC
	teams        []config.Team
	clientSecret string
	key          []byte
	client       *http.Client
	now          func() time.Time

	mu   sync.Mutex
	meta *discovery
	keys *keySet
}

type discovery struct {
	Issuer        string `json:"issuer"`
	AuthEndpoint  string `json:"authorization_endpoint"`
	TokenEndpoint string `json:"token_endpoint"`
	JWKSURI       string `json:"jwks_uri"`
}

// NewSSO builds the SSO handler. sessionKey must be at least 32 bytes.
func NewSSO(cfg config.OIDC, teams []config.Team, clientSecret, sessionKey string) *SSO {
	return &SSO{cfg: cfg, teams: teams, clientSecret: clientSecret, key: []byte(sessionKey),
		client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
}

type session struct {
	Subject  string   `json:"sub"`
	Email    string   `json:"email"`
	Name     string   `json:"name"`
	Envs     []string `json:"envs,omitempty"`
	Services []string `json:"svc,omitempty"`
	Expires  int64    `json:"exp"`
}

type flowState struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Expires  int64  `json:"exp"`
}

// Session returns the principal for a valid session cookie.
func (s *SSO) Session(r *http.Request) (Principal, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Principal{}, false
	}
	var sess session
	if !s.open(c.Value, &sess) || s.now().Unix() > sess.Expires {
		return Principal{}, false
	}
	name := sess.Email
	if name == "" {
		name = sess.Subject
	}
	return Principal{Name: name, Type: "sso", Scopes: []string{ScopeRead}, Envs: sess.Envs, Services: sess.Services}, true
}

// Login starts the sign-in flow.
func (s *SSO) Login(w http.ResponseWriter, r *http.Request) {
	meta, err := s.discover(r.Context())
	if err != nil {
		http.Error(w, "Sign-in is unavailable: the identity provider could not be reached.", http.StatusBadGateway)
		return
	}
	fs := flowState{State: randomString(24), Nonce: randomString(24), Verifier: randomString(48), Expires: s.now().Add(10 * time.Minute).Unix()}
	challenge := sha256.Sum256([]byte(fs.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {s.cfg.ClientID},
		"redirect_uri":          {s.cfg.RedirectURL},
		"scope":                 {strings.Join(s.cfg.Scopes, " ")},
		"state":                 {fs.State},
		"nonce":                 {fs.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	http.SetCookie(w, s.cookie(flowCookie, s.seal(fs), "/auth", 10*time.Minute))
	http.Redirect(w, r, meta.AuthEndpoint+sepFor(meta.AuthEndpoint)+q.Encode(), http.StatusFound)
}

// Callback finishes sign-in and sets the session cookie.
func (s *SSO) Callback(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, msg string) {
		http.SetCookie(w, s.cookie(flowCookie, "", "/auth", -1))
		http.Error(w, msg, code)
	}
	c, err := r.Cookie(flowCookie)
	var fs flowState
	if err != nil || !s.open(c.Value, &fs) || s.now().Unix() > fs.Expires {
		fail(http.StatusBadRequest, "Sign-in expired or was started in another browser. Go back and sign in again.")
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail(http.StatusForbidden, "The identity provider refused sign-in: "+e)
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(fs.State)) != 1 || q.Get("code") == "" {
		fail(http.StatusBadRequest, "Sign-in response did not match this browser session. Sign in again.")
		return
	}
	claims, err := s.exchange(r.Context(), q.Get("code"), fs)
	if err != nil {
		fail(http.StatusUnauthorized, "Sign-in could not be verified. Sign in again, or ask an admin to check the changefeed logs.")
		return
	}
	sess, ok := s.sessionFor(claims)
	if !ok {
		fail(http.StatusForbidden, "Your account is not in a changefeed team. Ask an admin to add one of your groups to auth.teams.")
		return
	}
	http.SetCookie(w, s.cookie(flowCookie, "", "/auth", -1))
	http.SetCookie(w, s.cookie(sessionCookie, s.seal(sess), "/", s.cfg.SessionTTL.Duration))
	http.Redirect(w, r, "/", http.StatusFound)
}

// Logout clears the session.
func (s *SSO) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.cookie(sessionCookie, "", "/", -1))
	http.Redirect(w, r, "/", http.StatusFound)
}

type idClaims struct {
	Issuer   string         `json:"iss"`
	Subject  string         `json:"sub"`
	Audience audience       `json:"aud"`
	Expiry   int64          `json:"exp"`
	Nonce    string         `json:"nonce"`
	Email    string         `json:"email"`
	Name     string         `json:"name"`
	Extra    map[string]any `json:"-"`
}

func (s *SSO) exchange(ctx context.Context, code string, fs flowState) (idClaims, error) {
	meta, err := s.discover(ctx)
	if err != nil {
		return idClaims{}, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.cfg.RedirectURL},
		"client_id":     {s.cfg.ClientID},
		"code_verifier": {fs.Verifier},
	}
	if s.clientSecret != "" {
		form.Set("client_secret", s.clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return idClaims{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return idClaims{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return idClaims{}, fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.IDToken == "" {
		return idClaims{}, errors.New("token response has no id_token")
	}
	var c idClaims
	if err := verifyJWT(ctx, s.keys, tok.IDToken, &c); err != nil {
		return c, err
	}
	var raw map[string]any
	if err := decodeSegment(strings.Split(tok.IDToken, ".")[1], &raw); err != nil {
		return c, err
	}
	c.Extra = raw
	switch {
	case c.Issuer != meta.Issuer:
		return c, errors.New("issuer mismatch")
	case !slices.Contains(c.Audience, s.cfg.ClientID):
		return c, errors.New("audience mismatch")
	case s.now().After(time.Unix(c.Expiry, 0).Add(time.Minute)):
		return c, errors.New("id token expired")
	case subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(fs.Nonce)) != 1:
		return c, errors.New("nonce mismatch")
	}
	return c, nil
}

// sessionFor maps groups to teams. With no teams configured, everyone who
// can sign in sees everything; otherwise the scopes of matching teams are
// combined, and a team without limits grants everything.
func (s *SSO) sessionFor(c idClaims) (session, bool) {
	sess := session{Subject: c.Subject, Email: c.Email, Name: c.Name, Expires: s.now().Add(s.cfg.SessionTTL.Duration).Unix()}
	if len(s.teams) == 0 {
		return sess, true
	}
	groups := stringList(c.Extra[s.cfg.GroupsClaim])
	matched, allEnvs, allSvcs := false, false, false
	envs, svcs := map[string]bool{}, map[string]bool{}
	for _, t := range s.teams {
		if !slices.ContainsFunc(t.Groups, func(g string) bool { return slices.Contains(groups, g) }) {
			continue
		}
		matched = true
		allEnvs = allEnvs || len(t.Envs) == 0
		allSvcs = allSvcs || len(t.Services) == 0
		for _, e := range t.Envs {
			envs[e] = true
		}
		for _, sv := range t.Services {
			svcs[sv] = true
		}
	}
	if !matched {
		return sess, false
	}
	if !allEnvs {
		sess.Envs = sortedKeys(envs)
	}
	if !allSvcs {
		sess.Services = sortedKeys(svcs)
	}
	return sess, true
}

func stringList(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.Fields(strings.ReplaceAll(t, ",", " "))
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (s *SSO) discover(ctx context.Context) (*discovery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meta != nil {
		return s.meta, nil
	}
	u := strings.TrimRight(s.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery returned %d", resp.StatusCode)
	}
	var d discovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return nil, err
	}
	if d.AuthEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" || d.Issuer == "" {
		return nil, errors.New("discovery document is incomplete")
	}
	s.meta = &d
	s.keys = &keySet{url: d.JWKSURI, client: s.client, now: s.now}
	return s.meta, nil
}

// seal signs a value: base64(json).base64(hmac).
func (s *SSO) seal(v any) string {
	b, _ := json.Marshal(v)
	p := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *SSO) open(raw string, v any) bool {
	p, sig, ok := strings.Cut(raw, ".")
	if !ok {
		return false
	}
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(p))
	want := base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	return err == nil && json.Unmarshal(b, v) == nil
}

func (s *SSO) cookie(name, value, path string, ttl time.Duration) *http.Cookie {
	c := &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: true}
	if ttl < 0 {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(ttl.Seconds())
	}
	return c
}

func sepFor(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}
