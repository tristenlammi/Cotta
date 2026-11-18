package auth

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// CFAccess validates the Cf-Access-Jwt-Assertion header that Cloudflare
// Access adds to every request, so reaching the container without going
// through the tunnel yields nothing.
type CFAccess struct {
	issuer  string // https://<team>.cloudflareaccess.com
	aud     string
	certURL string
	client  *http.Client

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// NewCFAccess accepts the team domain as "team", "team.cloudflareaccess.com"
// or a full https URL.
func NewCFAccess(teamDomain, aud string) *CFAccess {
	d := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(teamDomain), "https://"), "/")
	if !strings.Contains(d, ".") {
		d += ".cloudflareaccess.com"
	}
	return &CFAccess{
		issuer:  "https://" + d,
		aud:     aud,
		certURL: "https://" + d + "/cdn-cgi/access/certs",
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *CFAccess) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if err := c.Verify(r.Header.Get("Cf-Access-Jwt-Assertion")); err != nil {
			http.Error(w, "Cloudflare Access token missing or invalid", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (c *CFAccess) Verify(token string) error {
	if token == "" {
		return errors.New("no token")
	}
	_, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return c.key(kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithAudience(c.aud),
		jwt.WithIssuer(c.issuer),
		jwt.WithExpirationRequired(),
	)
	return err
}

func (c *CFAccess) key(kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.keys[kid]
	stale := time.Since(c.fetched) > time.Hour
	// Refetch when the cache is old, or for an unknown kid (key rotation),
	// but at most once a minute so bad tokens cannot trigger a fetch storm.
	if (!ok || stale) && time.Since(c.fetched) > time.Minute {
		if err := c.refresh(); err != nil && !ok {
			return nil, err
		}
		k, ok = c.keys[kid]
	}
	if !ok {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	return k, nil
}

func (c *CFAccess) refresh() error {
	c.fetched = time.Now()
	resp, err := c.client.Get(c.certURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching Access certs: %s", resp.Status)
	}
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("Access certs response held no usable keys")
	}
	c.keys = keys
	return nil
}
