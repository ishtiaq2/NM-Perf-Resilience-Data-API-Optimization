package proto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"sync/atomic"
)

// Token kinds.
const (
	KindSite = "site" // a Master Unit
	KindUser = "user" // a NOC operator or a customer's user
)

// AllTenants in a user token grants the whole fleet (NOC staff).
const AllTenants = "*"

// Claims identified by a token. The site id and tenant of a device come from its
// token, never from what the device says about itself.
type Claims struct {
	Kind    string
	Subject string // site id or user name
	Tenant  string // tenant id, or "*" for NOC staff (users only)
}

// Admin reports whether the claims cover every tenant.
func (c Claims) Admin() bool { return c.Kind == KindUser && c.Tenant == AllTenants }

// Sees reports whether the claims may see data of tenant t.
func (c Claims) Sees(t string) bool { return c.Admin() || c.Tenant == t }

var (
	ErrBadToken = errors.New("proto: malformed token")
	ErrBadSig   = errors.New("proto: token signature does not match")
)

func validPart(s string, allowStar bool) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	if allowStar && s == AllTenants {
		return true
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func sign(secret []byte, payload string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Mint returns "v1.<kind>.<subject>.<tenant>.<signature>". The characters are all
// valid in an HTTP header and in a WebSocket subprotocol ("bearer.<token>"),
// which is how browsers, which cannot set headers on a WebSocket, present it.
func Mint(secret []byte, c Claims) (string, error) {
	if (c.Kind != KindSite && c.Kind != KindUser) || !validPart(c.Subject, false) || !validPart(c.Tenant, c.Kind == KindUser) {
		return "", ErrBadToken
	}
	payload := "v1." + c.Kind + "." + c.Subject + "." + c.Tenant
	return payload + "." + sign(secret, payload), nil
}

// Verifier checks tokens against the current secret and, during a rotation, the
// previous one. Revoked subjects (lost or replaced devices, people who left) are
// refused; the list can be replaced while the NOC runs (SetRevoked).
type Verifier struct {
	Secrets [][]byte
	revoked atomic.Pointer[map[string]bool]
}

// SetRevoked replaces the revoked subjects: "site:<id>" or "user:<name>".
// Safe to call while other goroutines verify tokens.
func (v *Verifier) SetRevoked(subjects map[string]bool) { v.revoked.Store(&subjects) }

// IsRevoked reports whether the subject of that kind has been revoked.
func (v *Verifier) IsRevoked(kind, subject string) bool {
	m := v.revoked.Load()
	return m != nil && (*m)[kind+":"+subject]
}

// Verify returns the claims of a valid token.
func (v *Verifier) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 5 || parts[0] != "v1" {
		return Claims{}, ErrBadToken
	}
	c := Claims{Kind: parts[1], Subject: parts[2], Tenant: parts[3]}
	if (c.Kind != KindSite && c.Kind != KindUser) || !validPart(c.Subject, false) || !validPart(c.Tenant, c.Kind == KindUser) {
		return Claims{}, ErrBadToken
	}
	payload := strings.Join(parts[:4], ".")
	ok := false
	for _, s := range v.Secrets {
		if hmac.Equal([]byte(sign(s, payload)), []byte(parts[4])) {
			ok = true
		}
	}
	if !ok {
		return Claims{}, ErrBadSig
	}
	if v.IsRevoked(c.Kind, c.Subject) {
		return Claims{}, errors.New("proto: token revoked")
	}
	return c, nil
}

// TokenFrom extracts a bearer token from an Authorization header value or from
// the offered WebSocket subprotocols ("bearer.<token>").
func TokenFrom(authorization string, subprotocols []string) string {
	if t, ok := strings.CutPrefix(authorization, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	for _, p := range subprotocols {
		if t, ok := strings.CutPrefix(p, "bearer."); ok {
			return t
		}
	}
	return ""
}
