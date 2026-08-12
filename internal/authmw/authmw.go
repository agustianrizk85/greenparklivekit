// Package authmw is a self-contained, dependency-free verifier that department
// backends use to authenticate requests against the Greenpark master auth
// service. It verifies Ed25519-signed access tokens locally using the public
// key published at the auth service's JWKS endpoint — no per-request call back
// to auth is needed.
//
// HOW TO USE in a department backend (e.g. finance):
//
//	v, err := authmw.New(authmw.Options{
//	    JWKSURL:    "http://localhost:8090/.well-known/jwks.json",
//	    Department: "finance",  // this service's department code
//	    Issuer:     "greenpark-auth",
//	})
//	// protect a route (any role in the department):
//	mux.Handle("GET /api/data", v.RequireAuth(dataHandler))
//	// protect a write (admin role in the department, or a super user):
//	mux.Handle("POST /api/data", v.RequireAdmin(saveHandler))
//	// read the caller inside a handler:
//	claims := authmw.From(r.Context())
//
// Copy this file into the department module (adjust the package path) — it
// imports only the standard library.
package authmw

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

var b64 = base64.RawURLEncoding

// Claims mirrors the access-token payload issued by the auth service.
type Claims struct {
	Issuer    string            `json:"iss"`
	Subject   string            `json:"sub"`
	Username  string            `json:"username"`
	Email     string            `json:"email,omitempty"`
	Name      string            `json:"name"`
	Super     bool              `json:"super,omitempty"`
	Roles     map[string]string `json:"roles,omitempty"`
	IssuedAt  int64             `json:"iat"`
	ExpiresAt int64             `json:"exp"`
	JTI       string            `json:"jti"`
}

// CanAccess reports whether the caller may use the given department at all.
func (c Claims) CanAccess(dept string) bool {
	if c.Super {
		return true
	}
	_, ok := c.Roles[dept]
	return ok
}

// IsAdmin reports whether the caller may write in the given department.
func (c Claims) IsAdmin(dept string) bool {
	if c.Super {
		return true
	}
	return c.Roles[dept] == "admin"
}

// Role returns the caller's role string in the given department (empty if none).
func (c Claims) Role(dept string) string { return c.Roles[dept] }

// HasRole reports whether the caller's role in the department is one of the
// accepted values. Super users always pass. Use this for departments with their
// own role vocabulary (e.g. legalpermit: kadep/dirops), where "admin" is not the
// gate. Example: claims.HasRole("legalpermit", "kadep", "dirops").
func (c Claims) HasRole(dept string, accepted ...string) bool {
	if c.Super {
		return true
	}
	r := c.Roles[dept]
	for _, a := range accepted {
		if r == a {
			return true
		}
	}
	return false
}

// Options configures a Verifier.
type Options struct {
	// JWKSURL is the auth service's JWKS endpoint. The key set is fetched on
	// demand and cached; it is re-fetched when an unknown key id is seen.
	JWKSURL string
	// PublicKey is an alternative to JWKSURL: a pinned Ed25519 public key. If
	// set, no network fetch is performed.
	PublicKey ed25519.PublicKey
	// Department is this service's code; RequireAuth/RequireAdmin enforce it.
	Department string
	// Issuer, if set, is checked against the token's iss claim.
	Issuer string
	// HTTPClient is used for JWKS fetches (defaults to a 5s-timeout client).
	HTTPClient *http.Client
}

// Verifier authenticates requests using auth-service access tokens.
type Verifier struct {
	opts   Options
	client *http.Client

	mu   sync.RWMutex
	keys map[string]ed25519.PublicKey // kid -> key
}

// New builds a Verifier. It does not fetch keys eagerly; the first protected
// request triggers a lazy fetch (unless a PublicKey was pinned).
func New(opts Options) (*Verifier, error) {
	if opts.Department == "" {
		return nil, errors.New("authmw: Department is required")
	}
	if opts.JWKSURL == "" && opts.PublicKey == nil {
		return nil, errors.New("authmw: set JWKSURL or PublicKey")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	v := &Verifier{opts: opts, client: client, keys: make(map[string]ed25519.PublicKey)}
	if opts.PublicKey != nil {
		v.keys[""] = opts.PublicKey // "" matches when no kid is provided
	}
	return v, nil
}

type ctxKey int

const claimsKey ctxKey = 0

// From returns the verified claims stored in the request context (zero value if
// the request did not pass through RequireAuth/RequireAdmin).
func From(ctx context.Context) Claims {
	c, _ := ctx.Value(claimsKey).(Claims)
	return c
}

// RequireAuth wraps a handler, allowing any user with access to this department.
func (v *Verifier) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return v.guard(next, false)
}

// RequireAdmin wraps a handler, requiring admin role in this department (or a
// super user).
func (v *Verifier) RequireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return v.guard(next, true)
}

// RequireRole wraps a handler, requiring the caller's role in this department to
// be one of the accepted values (super users always pass). Use for departments
// whose write gate is not "admin" — e.g. RequireRole("kadep", "dirops").
func (v *Verifier) RequireRole(accepted ...string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			claims, err := v.Verify(bearer(r))
			if err != nil {
				writeErr(w, http.StatusUnauthorized, err.Error())
				return
			}
			if !claims.CanAccess(v.opts.Department) {
				writeErr(w, http.StatusForbidden, "tidak punya akses ke departemen "+v.opts.Department)
				return
			}
			if !claims.HasRole(v.opts.Department, accepted...) {
				writeErr(w, http.StatusForbidden, "role tidak mencukupi")
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), claimsKey, claims)))
		}
	}
}

func (v *Verifier) guard(next http.HandlerFunc, admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := v.Verify(bearer(r))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, err.Error())
			return
		}
		if !claims.CanAccess(v.opts.Department) {
			writeErr(w, http.StatusForbidden, "tidak punya akses ke departemen "+v.opts.Department)
			return
		}
		if admin && !claims.IsAdmin(v.opts.Department) {
			writeErr(w, http.StatusForbidden, "butuh akses admin")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), claimsKey, claims)))
	}
}

// Verify validates a compact EdDSA JWT and returns its claims.
func (v *Verifier) Verify(tok string) (Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("token tidak valid")
	}
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &h) != nil {
		return Claims{}, errors.New("token tidak valid")
	}
	if h.Alg != "EdDSA" {
		return Claims{}, fmt.Errorf("alg %q tidak didukung", h.Alg)
	}
	key, err := v.keyFor(h.Kid)
	if err != nil {
		return Claims{}, err
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return Claims{}, errors.New("token tidak valid")
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, errors.New("tanda tangan token tidak cocok")
	}
	cb, err := b64.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("token tidak valid")
	}
	var c Claims
	if err := json.Unmarshal(cb, &c); err != nil {
		return Claims{}, errors.New("token tidak valid")
	}
	if c.ExpiresAt > 0 && time.Now().Unix() > c.ExpiresAt {
		return Claims{}, errors.New("token kedaluwarsa")
	}
	if v.opts.Issuer != "" && c.Issuer != v.opts.Issuer {
		return Claims{}, errors.New("issuer token tidak dikenal")
	}
	return c, nil
}

// keyFor returns the public key for a key id, fetching the JWKS if needed.
func (v *Verifier) keyFor(kid string) (ed25519.PublicKey, error) {
	v.mu.RLock()
	if k, ok := v.keys[kid]; ok {
		v.mu.RUnlock()
		return k, nil
	}
	if k, ok := v.keys[""]; ok { // pinned key matches any kid
		v.mu.RUnlock()
		return k, nil
	}
	v.mu.RUnlock()

	if v.opts.JWKSURL == "" {
		return nil, errors.New("kunci verifikasi tidak ditemukan")
	}
	if err := v.fetchJWKS(); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("kunci verifikasi (kid) tidak dikenal")
}

// fetchJWKS downloads the key set and replaces the cache.
func (v *Verifier) fetchJWKS() error {
	resp, err := v.client.Get(v.opts.JWKSURL)
	if err != nil {
		return fmt.Errorf("ambil JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ambil JWKS: status %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			Kid string `json:"kid"`
			X   string `json:"x"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return fmt.Errorf("decode JWKS: %w", err)
	}
	fresh := make(map[string]ed25519.PublicKey)
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			continue
		}
		raw, err := b64.DecodeString(k.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		fresh[k.Kid] = ed25519.PublicKey(raw)
	}
	if len(fresh) == 0 {
		return errors.New("JWKS tidak berisi kunci Ed25519")
	}
	v.mu.Lock()
	v.keys = fresh
	v.mu.Unlock()
	return nil
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
