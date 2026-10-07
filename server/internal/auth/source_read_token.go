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
	SourceReadTokenPrefix   = "msr_"
	SourceReadTokenTTL      = 120 * time.Second
	SourceReadTokenIssuer   = "multica:source-read"
	SourceReadTokenAudience = "multica:daemon:source-read"
	SourceReadTokenPurpose  = "source:read"
)

var ErrInvalidSourceReadToken = errors.New("invalid source-read token")

// SourceReadClaims authenticates signed selectors, not current authorization.
// Consumers must fence the exact route and recheck runtime, membership and
// parent PAT in the same transaction as the source operation.
type SourceReadClaims struct {
	jwt.RegisteredClaims
	Purpose       string `json:"purpose"`
	WorkspaceID   string `json:"workspace_id"`
	RuntimeID     string `json:"runtime_id"`
	DaemonID      string `json:"daemon_id"`
	MemberID      string `json:"member_id"`
	ParentKind    string `json:"parent_kind"`
	ParentPATID   string `json:"parent_pat_id,omitempty"`
	ParentPATHash string `json:"parent_pat_hash,omitempty"`
}

// SignSourceReadToken fixes purpose and dates, ignoring caller-supplied standard
// claims except subject. A zero parent expiry is allowed only for nonexpiring
// PATs. now must be sampled after authorization locks. Dates round down to whole
// seconds, never extending the parent lifetime, and may leave no usable lifetime.
func SignSourceReadToken(claims SourceReadClaims, parentExpiresAt, now time.Time) (string, time.Time, error) {
	if now.IsZero() || (claims.ParentKind == "jwt" && parentExpiresAt.IsZero()) {
		return "", time.Time{}, ErrInvalidSourceReadToken
	}
	expiresAt := now.Add(SourceReadTokenTTL)
	if !parentExpiresAt.IsZero() && parentExpiresAt.Before(expiresAt) {
		expiresAt = parentExpiresAt
	}
	expiresAt = expiresAt.Truncate(time.Second)
	claims.RegisteredClaims = jwt.RegisteredClaims{
		Issuer: SourceReadTokenIssuer, Subject: claims.Subject,
		Audience: jwt.ClaimStrings{SourceReadTokenAudience},
		IssuedAt: jwt.NewNumericDate(now.Truncate(time.Second)), ExpiresAt: jwt.NewNumericDate(expiresAt),
	}
	claims.Purpose = SourceReadTokenPurpose
	if !validSourceReadClaims(claims, now) {
		return "", time.Time{}, ErrInvalidSourceReadToken
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(sourceReadSigningKey())
	if err != nil {
		return "", time.Time{}, ErrInvalidSourceReadToken
	}
	return SourceReadTokenPrefix + token, expiresAt, nil
}

// ParseSourceReadToken requires the prefix and purpose-derived signing key.
// There is no clock-skew leeway: now >= exp or now < iat rejects the token.
// Callers must check time again after any blocking authorization/receipt locks.
func ParseSourceReadToken(tokenString string, now time.Time) (SourceReadClaims, error) {
	var claims SourceReadClaims
	if now.IsZero() || !strings.HasPrefix(tokenString, SourceReadTokenPrefix) {
		return claims, ErrInvalidSourceReadToken
	}
	token, err := jwt.ParseWithClaims(strings.TrimPrefix(tokenString, SourceReadTokenPrefix), &claims,
		func(t *jwt.Token) (any, error) { return sourceReadSigningKey(), nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(SourceReadTokenIssuer), jwt.WithAudience(SourceReadTokenAudience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil || !token.Valid || !validSourceReadClaims(claims, now) {
		return SourceReadClaims{}, ErrInvalidSourceReadToken
	}
	return claims, nil
}

func sourceReadSigningKey() []byte {
	mac := hmac.New(sha256.New, JWTSecret())
	mac.Write([]byte("multica/source-read-token/v1"))
	return mac.Sum(nil)
}

func validSourceReadClaims(claims SourceReadClaims, now time.Time) bool {
	if claims.Issuer != SourceReadTokenIssuer || len(claims.Audience) != 1 ||
		claims.Audience[0] != SourceReadTokenAudience || claims.Purpose != SourceReadTokenPurpose ||
		claims.ID != "" || claims.NotBefore != nil || strings.TrimSpace(claims.DaemonID) == "" {
		return false
	}
	for _, value := range []string{claims.Subject, claims.WorkspaceID, claims.RuntimeID, claims.MemberID} {
		if !canonicalSourceReadUUID(value) {
			return false
		}
	}
	if claims.IssuedAt == nil || claims.ExpiresAt == nil || claims.IssuedAt.Unix() <= 0 ||
		claims.IssuedAt.Nanosecond() != 0 || claims.ExpiresAt.Nanosecond() != 0 ||
		claims.IssuedAt.After(now) || !claims.ExpiresAt.After(now) || !claims.ExpiresAt.After(claims.IssuedAt.Time) ||
		claims.ExpiresAt.Sub(claims.IssuedAt.Time) > SourceReadTokenTTL {
		return false
	}
	switch claims.ParentKind {
	case "jwt":
		return claims.ParentPATID == "" && claims.ParentPATHash == ""
	case "pat":
		hash, err := hex.DecodeString(claims.ParentPATHash)
		return canonicalSourceReadUUID(claims.ParentPATID) && err == nil && len(hash) == sha256.Size &&
			claims.ParentPATHash == hex.EncodeToString(hash)
	default:
		return false
	}
}

func canonicalSourceReadUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}
