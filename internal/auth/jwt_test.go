package auth

import (
	"strings"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := Verifier{Secret: []byte(strings.Repeat("s", 32)), Issuer: "ff", Audience: "api", Now: func() time.Time { return now }}
	good := Claims{Subject: "alice", Role: RoleCustomer, Issuer: "ff", Audience: "api", ExpiresAt: now.Add(time.Hour).Unix()}

	if c, err := v.Verify(v.Sign(good)); err != nil || c.Subject != "alice" {
		t.Fatalf("valid token rejected: %v", err)
	}

	bad := map[string]string{
		"expired":   v.Sign(Claims{Subject: "a", Role: RoleCustomer, Issuer: "ff", Audience: "api", ExpiresAt: now.Add(-time.Minute).Unix()}),
		"wrong aud": v.Sign(Claims{Subject: "a", Role: RoleCustomer, Issuer: "ff", Audience: "other", ExpiresAt: good.ExpiresAt}),
		"bad role":  v.Sign(Claims{Subject: "a", Role: "god", Issuer: "ff", Audience: "api", ExpiresAt: good.ExpiresAt}),
		"other key": Verifier{Secret: []byte(strings.Repeat("x", 32))}.Sign(good),
		"garbage":   "a.b.c",
	}
	// alg=none attack: same payload, header swapped, empty signature
	parts := strings.Split(v.Sign(good), ".")
	bad["alg none"] = b64.EncodeToString([]byte(`{"alg":"none"}`)) + "." + parts[1] + "."
	// privilege escalation: tamper role in payload, keep signature
	bad["tampered"] = parts[0] + "." + b64.EncodeToString([]byte(`{"sub":"alice","role":"admin","iss":"ff","aud":"api","exp":9999999999}`)) + "." + parts[2]

	for name, tok := range bad {
		if _, err := v.Verify(tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
