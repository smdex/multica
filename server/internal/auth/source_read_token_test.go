package auth

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func sourceReadTestClaims(now time.Time, parent string) SourceReadClaims {
	claims := SourceReadClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: SourceReadTokenIssuer, Subject: "123e4567-e89b-42d3-a456-426614174000",
			Audience: jwt.ClaimStrings{SourceReadTokenAudience},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(SourceReadTokenTTL)),
		},
		Purpose: SourceReadTokenPurpose, WorkspaceID: "123e4567-e89b-42d3-a456-426614174001",
		RuntimeID: "123e4567-e89b-42d3-a456-426614174002", DaemonID: "opaque daemon/identity:unchanged",
		MemberID: "123e4567-e89b-42d3-a456-426614174003", ParentKind: parent,
	}
	if parent == "pat" {
		claims.ParentPATID = "123e4567-e89b-42d3-a456-426614174004"
		claims.ParentPATHash = strings.Repeat("a", 64)
	}
	return claims
}

func signSourceReadFixture(t *testing.T, claims jwt.Claims, method jwt.SigningMethod, key any) string {
	t.Helper()
	token, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign fixture: %v", err)
	}
	return SourceReadTokenPrefix + token
}

func TestSourceReadTokenRoundTripAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 0, 0, 900_000_000, time.UTC)
	for _, tc := range []struct {
		name, parent string
		parentExpiry time.Time
	}{
		{"jwt-capped", "jwt", now.Add(time.Hour)},
		{"jwt-clipped", "jwt", now.Add(31 * time.Second)},
		{"pat-capped", "pat", now.Add(time.Hour)},
		{"pat-clipped", "pat", now.Add(31 * time.Second)},
		{"pat-nonexpiring", "pat", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := sourceReadTestClaims(now, tc.parent)
			// Signer fixes standard authority/dates rather than copying them.
			claims.Issuer, claims.Purpose, claims.ID = "untrusted-issuer", "other-purpose", "unwanted-jti"
			claims.Audience = jwt.ClaimStrings{"other-audience"}
			claims.ExpiresAt, claims.IssuedAt = nil, nil
			claims.NotBefore = jwt.NewNumericDate(now.Add(time.Hour))
			token, expiresAt, err := SignSourceReadToken(claims, tc.parentExpiry, now)
			if err != nil || !strings.HasPrefix(token, SourceReadTokenPrefix) {
				t.Fatalf("sign source-read token: %v", err)
			}
			wantExpiry := now.Add(SourceReadTokenTTL)
			if !tc.parentExpiry.IsZero() && tc.parentExpiry.Before(wantExpiry) {
				wantExpiry = tc.parentExpiry
			}
			wantExpiry = wantExpiry.Truncate(time.Second)
			if !expiresAt.Equal(wantExpiry) {
				t.Fatalf("expiry=%v want=%v", expiresAt, wantExpiry)
			}
			got, err := ParseSourceReadToken(token, now)
			if err != nil {
				t.Fatalf("parse source-read token: %v", err)
			}
			if got.Subject != claims.Subject || got.WorkspaceID != claims.WorkspaceID || got.RuntimeID != claims.RuntimeID ||
				got.DaemonID != claims.DaemonID || got.MemberID != claims.MemberID || got.ParentKind != claims.ParentKind ||
				got.ParentPATID != claims.ParentPATID || got.ParentPATHash != claims.ParentPATHash ||
				got.ID != "" || got.NotBefore != nil || got.Purpose != SourceReadTokenPurpose ||
				got.Issuer != SourceReadTokenIssuer || len(got.Audience) != 1 || got.Audience[0] != SourceReadTokenAudience ||
				!got.IssuedAt.Equal(now.Truncate(time.Second)) || !got.ExpiresAt.Equal(expiresAt) {
				t.Fatal("signed identity or fixed standard claims changed")
			}
			if _, err := ParseSourceReadToken(token, expiresAt.Add(-time.Nanosecond)); err != nil {
				t.Fatalf("token rejected just before expiry: %v", err)
			}
			if _, err := ParseSourceReadToken(token, expiresAt); err != ErrInvalidSourceReadToken {
				t.Fatal("token accepted at expiry with zero-skew policy")
			}
			if _, err := ParseSourceReadToken(token, got.IssuedAt.Add(-time.Nanosecond)); err != ErrInvalidSourceReadToken {
				t.Fatal("token accepted before issued-at with zero-skew policy")
			}
		})
	}
}

func TestSourceReadTokenPurposeKeySeparation(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)
	claims := sourceReadTestClaims(now, "jwt")
	token, _, err := SignSourceReadToken(claims, now.Add(time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimPrefix(token, SourceReadTokenPrefix)
	_, err = jwt.Parse(raw, func(*jwt.Token) (any, error) { return JWTSecret(), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithTimeFunc(func() time.Time { return now }))
	if !errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		t.Fatalf("stripped source token did not fail ordinary JWT-secret signature verification: %v", err)
	}
	if _, err := ParseSourceReadToken(raw, now); err != ErrInvalidSourceReadToken {
		t.Fatal("source parser accepted missing prefix")
	}
	for _, tc := range []struct {
		name   string
		method jwt.SigningMethod
		key    any
	}{
		{"ordinary-jwt-key", jwt.SigningMethodHS256, JWTSecret()},
		{"wrong-key", jwt.SigningMethodHS256, []byte("fixture-wrong-key")},
		{"wrong-hmac-algorithm", jwt.SigningMethodHS384, sourceReadSigningKey()},
		{"unsigned", jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forged := signSourceReadFixture(t, claims, tc.method, tc.key)
			if _, err := ParseSourceReadToken(forged, now); err != ErrInvalidSourceReadToken {
				t.Fatal("source parser accepted another signing domain/algorithm")
			}
		})
	}
}

func TestSourceReadTokenRejectsInvalidClaims(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)
	cases := map[string]func(*SourceReadClaims){
		"issuer":            func(c *SourceReadClaims) { c.Issuer = "wrong" },
		"audience":          func(c *SourceReadClaims) { c.Audience = jwt.ClaimStrings{"wrong"} },
		"extra-audience":    func(c *SourceReadClaims) { c.Audience = append(c.Audience, "wrong") },
		"missing-audience":  func(c *SourceReadClaims) { c.Audience = nil },
		"purpose":           func(c *SourceReadClaims) { c.Purpose = "task:mcp" },
		"missing-expiry":    func(c *SourceReadClaims) { c.ExpiresAt = nil },
		"expired":           func(c *SourceReadClaims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Second)) },
		"expiry-now":        func(c *SourceReadClaims) { c.ExpiresAt = jwt.NewNumericDate(now) },
		"missing-issued-at": func(c *SourceReadClaims) { c.IssuedAt = nil },
		"zero-issued-at":    func(c *SourceReadClaims) { c.IssuedAt = jwt.NewNumericDate(time.Unix(0, 0)) },
		"future-issued-at":  func(c *SourceReadClaims) { c.IssuedAt = jwt.NewNumericDate(now.Add(time.Second)) },
		"reversed-dates":    func(c *SourceReadClaims) { c.IssuedAt = c.ExpiresAt },
		"ttl-over-limit":    func(c *SourceReadClaims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(SourceReadTokenTTL + time.Second)) },
		"old-issued-at":     func(c *SourceReadClaims) { c.IssuedAt = jwt.NewNumericDate(now.Add(-time.Hour)) },
		"jti":               func(c *SourceReadClaims) { c.ID = "not-supported" },
		"not-before":        func(c *SourceReadClaims) { c.NotBefore = jwt.NewNumericDate(now) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			claims := sourceReadTestClaims(now, "jwt")
			mutate(&claims)
			token := signSourceReadFixture(t, claims, jwt.SigningMethodHS256, sourceReadSigningKey())
			if _, err := ParseSourceReadToken(token, now); err != ErrInvalidSourceReadToken {
				t.Fatal("source parser accepted invalid standard/purpose claims")
			}
		})
	}
}

func TestSourceReadTokenRejectsInvalidIdentityAndParent(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)
	cases := map[string]func(*SourceReadClaims){
		"missing-subject":    func(c *SourceReadClaims) { c.Subject = "" },
		"uppercase-subject":  func(c *SourceReadClaims) { c.Subject = strings.ToUpper(c.Subject) },
		"workspace":          func(c *SourceReadClaims) { c.WorkspaceID = "invalid" },
		"compact-runtime":    func(c *SourceReadClaims) { c.RuntimeID = strings.ReplaceAll(c.RuntimeID, "-", "") },
		"nil-member":         func(c *SourceReadClaims) { c.MemberID = "00000000-0000-0000-0000-000000000000" },
		"blank-daemon":       func(c *SourceReadClaims) { c.DaemonID = " \t\n" },
		"unknown-parent":     func(c *SourceReadClaims) { c.ParentKind = "mdt" },
		"pat-missing-id":     func(c *SourceReadClaims) { c.ParentPATID = "" },
		"pat-invalid-id":     func(c *SourceReadClaims) { c.ParentPATID = "foreign-resource" },
		"pat-missing-hash":   func(c *SourceReadClaims) { c.ParentPATHash = "" },
		"pat-short-hash":     func(c *SourceReadClaims) { c.ParentPATHash = "aa" },
		"pat-invalid-hash":   func(c *SourceReadClaims) { c.ParentPATHash = strings.Repeat("z", 64) },
		"pat-uppercase-hash": func(c *SourceReadClaims) { c.ParentPATHash = strings.Repeat("A", 64) },
		"jwt-with-pat":       func(c *SourceReadClaims) { c.ParentKind = "jwt" },
		"jwt-with-pat-id":    func(c *SourceReadClaims) { c.ParentKind, c.ParentPATHash = "jwt", "" },
		"jwt-with-pat-hash":  func(c *SourceReadClaims) { c.ParentKind, c.ParentPATID = "jwt", "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			claims := sourceReadTestClaims(now, "pat")
			mutate(&claims)
			if token, exp, err := SignSourceReadToken(claims, now.Add(time.Hour), now); err != ErrInvalidSourceReadToken || token != "" || !exp.IsZero() {
				t.Fatal("signer accepted invalid identity/parent or returned partial credential")
			}
			forged := signSourceReadFixture(t, claims, jwt.SigningMethodHS256, sourceReadSigningKey())
			if _, err := ParseSourceReadToken(forged, now); err != ErrInvalidSourceReadToken {
				t.Fatal("source parser accepted invalid identity/parent")
			}
		})
	}
}

func TestSourceReadTokenRejectsElapsedParentAndClock(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 0, 0, 900_000_000, time.UTC)
	for name, parentExpiry := range map[string]time.Time{
		"missing-jwt-expiry": {}, "expired-parent": now.Add(-time.Second),
		"elapsed-parent": now, "rounded-away-lifetime": now.Add(50 * time.Millisecond),
	} {
		t.Run(name, func(t *testing.T) {
			for _, parent := range []string{"jwt", "pat"} {
				if parentExpiry.IsZero() && parent == "pat" {
					continue // Nonexpiring PAT is covered by the successful round-trip.
				}
				if token, exp, err := SignSourceReadToken(sourceReadTestClaims(now, parent), parentExpiry, now); err != ErrInvalidSourceReadToken || token != "" || !exp.IsZero() {
					t.Fatal("signer accepted missing/elapsed parent lifetime")
				}
			}
		})
	}
	if _, _, err := SignSourceReadToken(sourceReadTestClaims(now, "pat"), time.Time{}, time.Time{}); err != ErrInvalidSourceReadToken {
		t.Fatal("signer accepted zero clock")
	}
	for _, token := range []string{"", "msr_", "msr_not-a-jwt", "mul_fixture", "mdt_fixture", "mat_fixture"} {
		if _, err := ParseSourceReadToken(token, now); err != ErrInvalidSourceReadToken {
			t.Fatal("parser accepted malformed or another credential prefix")
		}
	}
	valid, _, err := SignSourceReadToken(sourceReadTestClaims(now, "pat"), time.Time{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSourceReadToken(valid, time.Time{}); err != ErrInvalidSourceReadToken {
		t.Fatal("parser accepted zero clock")
	}
}

func TestSourceReadTokenRejectsMalformedClaimTypes(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)
	encoded, err := json.Marshal(sourceReadTestClaims(now, "pat"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"sub", "workspace_id", "runtime_id", "daemon_id", "member_id", "parent_kind", "parent_pat_id", "parent_pat_hash", "purpose", "iss", "aud", "exp", "iat"} {
		t.Run(field, func(t *testing.T) {
			var claims jwt.MapClaims
			if err := json.Unmarshal(encoded, &claims); err != nil {
				t.Fatal(err)
			}
			claims[field] = map[string]any{"malformed": true}
			forged := signSourceReadFixture(t, claims, jwt.SigningMethodHS256, sourceReadSigningKey())
			if _, err := ParseSourceReadToken(forged, now); err != ErrInvalidSourceReadToken {
				t.Fatal("source parser accepted incorrectly typed claim")
			}
		})
	}
}
