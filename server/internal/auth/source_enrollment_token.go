package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	SourceEnrollmentTokenPrefix   = "mse_"
	SourceEnrollmentTokenTTL      = 120 * time.Second
	SourceEnrollmentTokenIssuer   = "multica:source-enrollment"
	SourceEnrollmentTokenAudience = "multica:daemon:source-enrollment"
	SourceEnrollmentTokenPurpose  = "source:enroll"
)

var ErrInvalidSourceEnrollmentToken = errors.New("invalid source-enrollment token")

// SourceEnrollmentClaims authenticates signed selectors, not current
// authorization. Actual consumption must revalidate live: exact runtime
// ownership, admin member incarnation, parent PAT (when pat), the source
// enrollment row, and config revision under locks, and recheck exp after any
// lock waits, before treating a replayed token as usable.
type SourceEnrollmentClaims struct {
	jwt.RegisteredClaims
	Purpose        string `json:"purpose"`
	WorkspaceID    string `json:"workspace_id"`
	RuntimeID      string `json:"runtime_id"`
	DaemonID       string `json:"daemon_id"`
	MemberID       string `json:"member_id"`
	ParentKind     string `json:"parent_kind"`
	ParentPATID    string `json:"parent_pat_id,omitempty"`
	ParentPATHash  string `json:"parent_pat_hash,omitempty"`
	SourceID       string `json:"source_id"`
	EnrollmentID   string `json:"enrollment_id"`
	ConfigRevision int64  `json:"config_revision"`
	ManifestHash   string `json:"manifest_hash"`
}

// SignSourceEnrollmentToken fixes purpose and dates, ignoring caller-supplied
// standard claims except subject. A zero parent expiry is allowed only for
// nonexpiring PATs. now must be sampled after authorization locks. Dates round
// down to whole seconds, never extending the parent lifetime, and may leave no
// usable lifetime.
func SignSourceEnrollmentToken(claims SourceEnrollmentClaims, parentExpiresAt, now time.Time) (string, time.Time, error) {
	if now.IsZero() || (claims.ParentKind == "jwt" && parentExpiresAt.IsZero()) {
		return "", time.Time{}, ErrInvalidSourceEnrollmentToken
	}
	expiresAt := now.Add(SourceEnrollmentTokenTTL)
	if !parentExpiresAt.IsZero() && parentExpiresAt.Before(expiresAt) {
		expiresAt = parentExpiresAt
	}
	expiresAt = expiresAt.Truncate(time.Second)
	claims.RegisteredClaims = jwt.RegisteredClaims{
		Issuer: SourceEnrollmentTokenIssuer, Subject: claims.Subject,
		Audience: jwt.ClaimStrings{SourceEnrollmentTokenAudience},
		IssuedAt: jwt.NewNumericDate(now.Truncate(time.Second)), ExpiresAt: jwt.NewNumericDate(expiresAt),
	}
	claims.Purpose = SourceEnrollmentTokenPurpose
	if !validSourceEnrollmentClaims(claims, now) {
		return "", time.Time{}, ErrInvalidSourceEnrollmentToken
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(sourceEnrollmentSigningKey())
	if err != nil {
		return "", time.Time{}, ErrInvalidSourceEnrollmentToken
	}
	return SourceEnrollmentTokenPrefix + token, expiresAt, nil
}

// ParseSourceEnrollmentToken requires the prefix and purpose-derived signing
// key. There is no clock-skew leeway: now >= exp or now < iat rejects the
// token. Callers must check time again after any blocking authorization/receipt
// locks.
func ParseSourceEnrollmentToken(tokenString string, now time.Time) (SourceEnrollmentClaims, error) {
	var claims SourceEnrollmentClaims
	if now.IsZero() || !strings.HasPrefix(tokenString, SourceEnrollmentTokenPrefix) {
		return claims, ErrInvalidSourceEnrollmentToken
	}
	token, err := jwt.ParseWithClaims(strings.TrimPrefix(tokenString, SourceEnrollmentTokenPrefix), &claims,
		func(t *jwt.Token) (any, error) { return sourceEnrollmentSigningKey(), nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(SourceEnrollmentTokenIssuer), jwt.WithAudience(SourceEnrollmentTokenAudience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil || !token.Valid || !validSourceEnrollmentClaims(claims, now) {
		return SourceEnrollmentClaims{}, ErrInvalidSourceEnrollmentToken
	}
	return claims, nil
}

func sourceEnrollmentSigningKey() []byte {
	mac := hmac.New(sha256.New, JWTSecret())
	mac.Write([]byte("multica/source-enrollment-token/v1"))
	return mac.Sum(nil)
}

func validSourceEnrollmentClaims(claims SourceEnrollmentClaims, now time.Time) bool {
	if claims.Issuer != SourceEnrollmentTokenIssuer || len(claims.Audience) != 1 ||
		claims.Audience[0] != SourceEnrollmentTokenAudience || claims.Purpose != SourceEnrollmentTokenPurpose ||
		claims.ID != "" || claims.NotBefore != nil || strings.TrimSpace(claims.DaemonID) == "" {
		return false
	}
	for _, value := range []string{claims.Subject, claims.WorkspaceID, claims.RuntimeID, claims.MemberID,
		claims.SourceID, claims.EnrollmentID} {
		if !canonicalSourceEnrollmentUUID(value) {
			return false
		}
	}
	hash, err := hex.DecodeString(claims.ManifestHash)
	if claims.ConfigRevision <= 0 || err != nil || len(hash) != sha256.Size ||
		claims.ManifestHash != hex.EncodeToString(hash) {
		return false
	}
	if claims.IssuedAt == nil || claims.ExpiresAt == nil || claims.IssuedAt.Unix() <= 0 ||
		claims.IssuedAt.Nanosecond() != 0 || claims.ExpiresAt.Nanosecond() != 0 ||
		claims.IssuedAt.After(now) || !claims.ExpiresAt.After(now) || !claims.ExpiresAt.After(claims.IssuedAt.Time) ||
		claims.ExpiresAt.Sub(claims.IssuedAt.Time) > SourceEnrollmentTokenTTL {
		return false
	}
	switch claims.ParentKind {
	case "jwt":
		return claims.ParentPATID == "" && claims.ParentPATHash == ""
	case "pat":
		hash, err := hex.DecodeString(claims.ParentPATHash)
		return canonicalSourceEnrollmentUUID(claims.ParentPATID) && err == nil && len(hash) == sha256.Size &&
			claims.ParentPATHash == hex.EncodeToString(hash)
	default:
		return false
	}
}

func canonicalSourceEnrollmentUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}
