package auth

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func sourceEnrollmentTestClaims(now time.Time, parent string) SourceEnrollmentClaims {
	claims := SourceEnrollmentClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: SourceEnrollmentTokenIssuer, Subject: "123e4567-e89b-42d3-a456-426614174000",
			Audience: jwt.ClaimStrings{SourceEnrollmentTokenAudience},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(SourceEnrollmentTokenTTL)),
		},
		Purpose: SourceEnrollmentTokenPurpose, WorkspaceID: "123e4567-e89b-42d3-a456-426614174001",
		RuntimeID: "123e4567-e89b-42d3-a456-426614174002", DaemonID: "opaque daemon/identity:unchanged",
		MemberID: "123e4567-e89b-42d3-a456-426614174003", ParentKind: parent,
		SourceID:     "123e4567-e89b-42d3-a456-426614174005",
		EnrollmentID: "123e4567-e89b-42d3-a456-426614174006", ConfigRevision: 7,
		ManifestHash: strings.Repeat("b", 64),
	}
	if parent == "pat" {
		claims.ParentPATID = "123e4567-e89b-42d3-a456-426614174004"
		claims.ParentPATHash = strings.Repeat("a", 64)
	}
	return claims
}

func signSourceEnrollmentFixture(t *testing.T, claims jwt.Claims, method jwt.SigningMethod, key any) string {
	t.Helper()
	token, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign fixture: %v", err)
	}
	return SourceEnrollmentTokenPrefix + token
}

func TestSourceEnrollmentTokenRoundTripAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 0, 0, 900_000_000, time.UTC)
	for _, tc := range []struct {
		name, parent string
		parentExpiry time.Time
	}{
		{"jwt-capped", "jwt", now.Add(time.Hour)},
		{"jwt-clipped", "jwt", now.Add(31 * time.Second)},
		{"pat-capped", "pat", now.Add(time.Hour)},
		{"pat-clipped", "pat", now.Add(31*time.Second + 400*time.Millisecond)},
		{"pat-nonexpiring", "pat", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := sourceEnrollmentTestClaims(now, tc.parent)
			// Signer fixes standard authority/dates rather than copying them.
			claims.Issuer, claims.Purpose, claims.ID = "untrusted-issuer", "other-purpose", "unwanted-jti"
			claims.Audience = jwt.ClaimStrings{"other-audience"}
			claims.ExpiresAt, claims.IssuedAt = nil, nil
			claims.NotBefore = jwt.NewNumericDate(now.Add(time.Hour))
			token, expiresAt, err := SignSourceEnrollmentToken(claims, tc.parentExpiry, now)
			if err != nil || !strings.HasPrefix(token, SourceEnrollmentTokenPrefix) {
				t.Fatalf("sign source-enrollment token: %v", err)
			}
			wantExpiry := now.Add(SourceEnrollmentTokenTTL)
			if !tc.parentExpiry.IsZero() && tc.parentExpiry.Before(wantExpiry) {
				wantExpiry = tc.parentExpiry
			}
			wantExpiry = wantExpiry.Truncate(time.Second)
			if !expiresAt.Equal(wantExpiry) {
				t.Fatalf("expiry=%v want=%v", expiresAt, wantExpiry)
			}
			got, err := ParseSourceEnrollmentToken(token, now)
			if err != nil {
				t.Fatalf("parse source-enrollment token: %v", err)
			}
			if got.Subject != claims.Subject || got.WorkspaceID != claims.WorkspaceID || got.RuntimeID != claims.RuntimeID ||
				got.DaemonID != claims.DaemonID || got.MemberID != claims.MemberID || got.ParentKind != claims.ParentKind ||
				got.ParentPATID != claims.ParentPATID || got.ParentPATHash != claims.ParentPATHash ||
				got.SourceID != claims.SourceID || got.EnrollmentID != claims.EnrollmentID ||
				got.ConfigRevision != claims.ConfigRevision || got.ManifestHash != claims.ManifestHash ||
				got.ID != "" || got.NotBefore != nil || got.Purpose != SourceEnrollmentTokenPurpose ||
				got.Issuer != SourceEnrollmentTokenIssuer || len(got.Audience) != 1 || got.Audience[0] != SourceEnrollmentTokenAudience ||
				!got.IssuedAt.Equal(now.Truncate(time.Second)) || !got.ExpiresAt.Equal(expiresAt) {
				t.Fatal("signed identity or fixed standard claims changed")
			}
			if _, err := ParseSourceEnrollmentToken(token, expiresAt.Add(-time.Nanosecond)); err != nil {
				t.Fatalf("token rejected just before expiry: %v", err)
			}
			if _, err := ParseSourceEnrollmentToken(token, expiresAt); err != ErrInvalidSourceEnrollmentToken {
				t.Fatal("token accepted at expiry with zero-skew policy")
			}
			if _, err := ParseSourceEnrollmentToken(token, got.IssuedAt.Add(-time.Nanosecond)); err != ErrInvalidSourceEnrollmentToken {
				t.Fatal("token accepted before issued-at with zero-skew policy")
			}
		})
	}
}

func TestSourceEnrollmentTokenPurposeKeySeparation(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)
	claims := sourceEnrollmentTestClaims(now, "jwt")
	token, _, err := SignSourceEnrollmentToken(claims, now.Add(time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimPrefix(token, SourceEnrollmentTokenPrefix)
	_, err = jwt.Parse(raw, func(*jwt.Token) (any, error) { return JWTSecret(), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithTimeFunc(func() time.Time { return now }))
	if !errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		t.Fatalf("stripped enrollment token did not fail ordinary JWT-secret signature verification: %v", err)
	}
	if _, err := ParseSourceEnrollmentToken(raw, now); err != ErrInvalidSourceEnrollmentToken {
		t.Fatal("enrollment parser accepted missing prefix")
	}
	for _, tc := range []struct {
		name   string
		method jwt.SigningMethod
		key    any
	}{
		{"ordinary-jwt-key", jwt.SigningMethodHS256, JWTSecret()},
		{"wrong-key", jwt.SigningMethodHS256, []byte("fixture-wrong-key")},
		{"wrong-hmac-algorithm", jwt.SigningMethodHS512, sourceEnrollmentSigningKey()},
		{"unsigned", jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forged := signSourceEnrollmentFixture(t, claims, tc.method, tc.key)
			if _, err := ParseSourceEnrollmentToken(forged, now); err != ErrInvalidSourceEnrollmentToken {
				t.Fatal("enrollment parser accepted another signing domain/algorithm")
			}
		})
	}
	// Tamper with the payload while keeping the signature domain.
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("unexpected token shape")
	}
	parts[1] = parts[1][:len(parts[1])-2] + "XX"
	if _, err := ParseSourceEnrollmentToken(strings.Join(parts, "."), now); err != ErrInvalidSourceEnrollmentToken {
		t.Fatal("enrollment parser accepted a tampered payload")
	}
}

func TestSourceEnrollmentTokenRejectsInvalidClaims(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)
	cases := map[string]func(*SourceEnrollmentClaims){
		"issuer":            func(c *SourceEnrollmentClaims) { c.Issuer = "wrong" },
		"audience":          func(c *SourceEnrollmentClaims) { c.Audience = jwt.ClaimStrings{"wrong"} },
		"extra-audience":    func(c *SourceEnrollmentClaims) { c.Audience = append(c.Audience, "wrong") },
		"missing-audience":  func(c *SourceEnrollmentClaims) { c.Audience = nil },
		"purpose":           func(c *SourceEnrollmentClaims) { c.Purpose = SourceReadTokenPurpose },
		"missing-expiry":    func(c *SourceEnrollmentClaims) { c.ExpiresAt = nil },
		"expired":           func(c *SourceEnrollmentClaims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Second)) },
		"expiry-now":        func(c *SourceEnrollmentClaims) { c.ExpiresAt = jwt.NewNumericDate(now) },
		"missing-issued-at": func(c *SourceEnrollmentClaims) { c.IssuedAt = nil },
		"zero-issued-at":    func(c *SourceEnrollmentClaims) { c.IssuedAt = jwt.NewNumericDate(time.Unix(0, 0)) },
		"future-issued-at":  func(c *SourceEnrollmentClaims) { c.IssuedAt = jwt.NewNumericDate(now.Add(time.Second)) },
		"reversed-dates":    func(c *SourceEnrollmentClaims) { c.IssuedAt = c.ExpiresAt },
		"ttl-over-limit": func(c *SourceEnrollmentClaims) {
			c.ExpiresAt = jwt.NewNumericDate(now.Add(SourceEnrollmentTokenTTL + time.Second))
		},
		"old-issued-at": func(c *SourceEnrollmentClaims) { c.IssuedAt = jwt.NewNumericDate(now.Add(-time.Hour)) },
		"jti":           func(c *SourceEnrollmentClaims) { c.ID = "not-supported" },
		"not-before":    func(c *SourceEnrollmentClaims) { c.NotBefore = jwt.NewNumericDate(now) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			claims := sourceEnrollmentTestClaims(now, "jwt")
			mutate(&claims)
			token := signSourceEnrollmentFixture(t, claims, jwt.SigningMethodHS256, sourceEnrollmentSigningKey())
			if _, err := ParseSourceEnrollmentToken(token, now); err != ErrInvalidSourceEnrollmentToken {
				t.Fatal("enrollment parser accepted invalid standard/purpose claims")
			}
		})
	}
}

func TestSourceEnrollmentTokenRejectsInvalidIdentityAndParent(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)
	cases := map[string]func(*SourceEnrollmentClaims){
		"missing-subject":    func(c *SourceEnrollmentClaims) { c.Subject = "" },
		"uppercase-subject":  func(c *SourceEnrollmentClaims) { c.Subject = strings.ToUpper(c.Subject) },
		"workspace":          func(c *SourceEnrollmentClaims) { c.WorkspaceID = "invalid" },
		"compact-runtime":    func(c *SourceEnrollmentClaims) { c.RuntimeID = strings.ReplaceAll(c.RuntimeID, "-", "") },
		"nil-member":         func(c *SourceEnrollmentClaims) { c.MemberID = "00000000-0000-0000-0000-000000000000" },
		"blank-daemon":       func(c *SourceEnrollmentClaims) { c.DaemonID = " \t\n" },
		"invalid-source":     func(c *SourceEnrollmentClaims) { c.SourceID = "foreign-resource" },
		"nil-enrollment":     func(c *SourceEnrollmentClaims) { c.EnrollmentID = "00000000-0000-0000-0000-000000000000" },
		"zero-config":        func(c *SourceEnrollmentClaims) { c.ConfigRevision = 0 },
		"negative-config":    func(c *SourceEnrollmentClaims) { c.ConfigRevision = -1 },
		"manifest-short":     func(c *SourceEnrollmentClaims) { c.ManifestHash = "bb" },
		"manifest-invalid":   func(c *SourceEnrollmentClaims) { c.ManifestHash = strings.Repeat("z", 64) },
		"manifest-uppercase": func(c *SourceEnrollmentClaims) { c.ManifestHash = strings.Repeat("B", 64) },
		"manifest-missing":   func(c *SourceEnrollmentClaims) { c.ManifestHash = "" },
		"unknown-parent":     func(c *SourceEnrollmentClaims) { c.ParentKind = "mdt" },
		"pat-missing-id":     func(c *SourceEnrollmentClaims) { c.ParentPATID = "" },
		"pat-invalid-id":     func(c *SourceEnrollmentClaims) { c.ParentPATID = "foreign-resource" },
		"pat-missing-hash":   func(c *SourceEnrollmentClaims) { c.ParentPATHash = "" },
		"pat-short-hash":     func(c *SourceEnrollmentClaims) { c.ParentPATHash = "aa" },
		"pat-invalid-hash":   func(c *SourceEnrollmentClaims) { c.ParentPATHash = strings.Repeat("z", 64) },
		"pat-uppercase-hash": func(c *SourceEnrollmentClaims) { c.ParentPATHash = strings.Repeat("A", 64) },
		"jwt-with-pat":       func(c *SourceEnrollmentClaims) { c.ParentKind = "jwt" },
		"jwt-with-pat-id":    func(c *SourceEnrollmentClaims) { c.ParentKind, c.ParentPATHash = "jwt", "" },
		"jwt-with-pat-hash":  func(c *SourceEnrollmentClaims) { c.ParentKind, c.ParentPATID = "jwt", "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			claims := sourceEnrollmentTestClaims(now, "pat")
			mutate(&claims)
			if token, exp, err := SignSourceEnrollmentToken(claims, now.Add(time.Hour), now); err != ErrInvalidSourceEnrollmentToken || token != "" || !exp.IsZero() {
				t.Fatal("signer accepted invalid identity/parent or returned partial credential")
			}
			forged := signSourceEnrollmentFixture(t, claims, jwt.SigningMethodHS256, sourceEnrollmentSigningKey())
			if _, err := ParseSourceEnrollmentToken(forged, now); err != ErrInvalidSourceEnrollmentToken {
				t.Fatal("enrollment parser accepted invalid identity/parent")
			}
		})
	}
}

func TestSourceEnrollmentTokenRejectsElapsedParentAndClock(t *testing.T) {
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
				if token, exp, err := SignSourceEnrollmentToken(sourceEnrollmentTestClaims(now, parent), parentExpiry, now); err != ErrInvalidSourceEnrollmentToken || token != "" || !exp.IsZero() {
					t.Fatal("signer accepted missing/elapsed parent lifetime")
				}
			}
		})
	}
	if _, _, err := SignSourceEnrollmentToken(sourceEnrollmentTestClaims(now, "pat"), time.Time{}, time.Time{}); err != ErrInvalidSourceEnrollmentToken {
		t.Fatal("signer accepted zero clock")
	}
	for _, token := range []string{"", "mse_", "mse_not-a-jwt", "msr_fixture", "mul_fixture", "mdt_fixture", "mat_fixture"} {
		if _, err := ParseSourceEnrollmentToken(token, now); err != ErrInvalidSourceEnrollmentToken {
			t.Fatal("parser accepted malformed or another credential prefix")
		}
	}
	valid, _, err := SignSourceEnrollmentToken(sourceEnrollmentTestClaims(now, "pat"), time.Time{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSourceEnrollmentToken(valid, time.Time{}); err != ErrInvalidSourceEnrollmentToken {
		t.Fatal("parser accepted zero clock")
	}
}

func TestSourceEnrollmentTokenRejectsMalformedClaimTypes(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)
	encoded, err := json.Marshal(sourceEnrollmentTestClaims(now, "pat"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"sub", "workspace_id", "runtime_id", "daemon_id", "member_id", "parent_kind",
		"parent_pat_id", "parent_pat_hash", "source_id", "enrollment_id", "config_revision", "manifest_hash",
		"purpose", "iss", "aud", "exp", "iat"} {
		t.Run(field, func(t *testing.T) {
			var claims jwt.MapClaims
			if err := json.Unmarshal(encoded, &claims); err != nil {
				t.Fatal(err)
			}
			claims[field] = map[string]any{"malformed": true}
			forged := signSourceEnrollmentFixture(t, claims, jwt.SigningMethodHS256, sourceEnrollmentSigningKey())
			if _, err := ParseSourceEnrollmentToken(forged, now); err != ErrInvalidSourceEnrollmentToken {
				t.Fatal("enrollment parser accepted incorrectly typed claim")
			}
		})
	}
}

func TestSourceEnrollmentTokenCrossParserSeparation(t *testing.T) {
	now := time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)
	readClaims := sourceReadTestClaims(now, "jwt")
	readToken, _, err := SignSourceReadToken(readClaims, now.Add(time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSourceEnrollmentToken(readToken, now); err != ErrInvalidSourceEnrollmentToken {
		t.Fatal("enrollment parser accepted a source-read token")
	}
	enrollClaims := sourceEnrollmentTestClaims(now, "jwt")
	enrollToken, _, err := SignSourceEnrollmentToken(enrollClaims, now.Add(time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSourceReadToken(enrollToken, now); err != ErrInvalidSourceReadToken {
		t.Fatal("source-read parser accepted an enrollment token")
	}
	if _, err := ParseSessionToken(strings.TrimPrefix(enrollToken, SourceEnrollmentTokenPrefix)); err == nil {
		t.Fatal("session parser accepted an enrollment token")
	}
}
