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
	"strings"
	"sync"
	"time"
)

// keySet fetches and caches an issuer's RSA signing keys (JWKS).
// Unknown key IDs trigger a refresh (key rotation), limited to one a minute
// so bad tokens cannot be used to hammer the issuer.
type keySet struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func (s *keySet) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if k, ok := s.keys[kid]; ok && now.Sub(s.fetchedAt) < time.Hour {
		return k, nil
	}
	if now.Sub(s.fetchedAt) < time.Minute && s.keys != nil {
		if k, ok := s.keys[kid]; ok {
			return k, nil
		}
		return nil, errors.New("unknown signing key")
	}
	keys, err := s.fetch(ctx)
	if err != nil {
		if k, ok := s.keys[kid]; ok { // issuer down: keep using a key we already trust
			return k, nil
		}
		return nil, err
	}
	s.keys, s.fetchedAt = keys, now
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}

func (s *keySet) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
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
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
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

// verifyJWT checks an RS256 signature and decodes the claims into v.
func verifyJWT(ctx context.Context, ks *keySet, token string, v any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("malformed token")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return fmt.Errorf("header: %w", err)
	}
	if hdr.Alg != "RS256" {
		return fmt.Errorf("unsupported alg %q", hdr.Alg)
	}
	key, err := ks.key(ctx, hdr.Kid)
	if err != nil {
		return err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return errors.New("bad signature encoding")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return errors.New("bad signature")
	}
	if err := decodeSegment(parts[1], v); err != nil {
		return fmt.Errorf("claims: %w", err)
	}
	return nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
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
