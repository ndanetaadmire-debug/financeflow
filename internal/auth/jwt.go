// Package auth verifies HS256 JWTs with the standard library only.
// Algorithm is pinned (no "alg: none", no RS/HS confusion), exp/nbf/iss/aud are enforced.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

type Role string

const (
	RoleCustomer Role = "customer"
	RoleOps      Role = "ops"   // can view any account, reverse transactions
	RoleAdmin    Role = "admin" // can open system accounts
)

type Claims struct {
	Subject   string `json:"sub"`
	Role      Role   `json:"role"`
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	ExpiresAt int64  `json:"exp"`
	NotBefore int64  `json:"nbf,omitempty"`
}

func (c Claims) Can(roles ...Role) bool { return slices.Contains(roles, c.Role) }

var ErrInvalidToken = errors.New("invalid token")

type Verifier struct {
	Secret   []byte
	Issuer   string
	Audience string
	Now      func() time.Time
	Leeway   time.Duration
}

var b64 = base64.RawURLEncoding

func (v Verifier) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalidToken
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := decode(parts[0], &header); err != nil || header.Alg != "HS256" {
		return Claims{}, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, v.Secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return Claims{}, ErrInvalidToken
	}
	var c Claims
	if err := decode(parts[1], &c); err != nil {
		return Claims{}, ErrInvalidToken
	}
	now := v.now().Unix()
	leeway := int64(v.Leeway.Seconds())
	if c.ExpiresAt == 0 || now > c.ExpiresAt+leeway || (c.NotBefore != 0 && now+leeway < c.NotBefore) {
		return Claims{}, ErrInvalidToken
	}
	if c.Issuer != v.Issuer || c.Audience != v.Audience || c.Subject == "" {
		return Claims{}, ErrInvalidToken
	}
	switch c.Role {
	case RoleCustomer, RoleOps, RoleAdmin:
	default:
		return Claims{}, ErrInvalidToken
	}
	return c, nil
}

// Sign issues a token. Used by tests and the local dev token command.
func (v Verifier) Sign(c Claims) string {
	h, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	p, _ := json.Marshal(c)
	unsigned := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	mac := hmac.New(sha256.New, v.Secret)
	mac.Write([]byte(unsigned))
	return unsigned + "." + b64.EncodeToString(mac.Sum(nil))
}

func (v Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func decode(seg string, out any) error {
	b, err := b64.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
