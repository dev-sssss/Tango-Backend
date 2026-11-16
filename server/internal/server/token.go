package server

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/golang-jwt/jwt/v5"
)

// SessionClaims is byte-for-byte compatible with Nakama's session token
// claims so existing client SDKs can decode them (uid/usn/vrs/exp) and tokens
// issued by Nakama remain valid during cutover when the same key is used.
type SessionClaims struct {
	TokenID   string            `json:"tid,omitempty"`
	UserID    string            `json:"uid,omitempty"`
	Username  string            `json:"usn,omitempty"`
	Vars      map[string]string `json:"vrs,omitempty"`
	ExpiresAt int64             `json:"exp,omitempty"`
	IssuedAt  int64             `json:"iat,omitempty"`
}

func (s *SessionClaims) GetExpirationTime() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(s.ExpiresAt, 0)), nil
}
func (s *SessionClaims) GetNotBefore() (*jwt.NumericDate, error) { return nil, nil }
func (s *SessionClaims) GetIssuedAt() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(s.IssuedAt, 0)), nil
}
func (s *SessionClaims) GetAudience() (jwt.ClaimStrings, error) { return []string{}, nil }
func (s *SessionClaims) GetIssuer() (string, error)             { return "", nil }
func (s *SessionClaims) GetSubject() (string, error)            { return "", nil }

type TokenService struct {
	cfg *Config
}

func NewTokenService(cfg *Config) *TokenService { return &TokenService{cfg: cfg} }

// Issue creates a session token and refresh token pair.
func (t *TokenService) Issue(userID, username string, vars map[string]string) (token, refresh string, exp int64) {
	tid := uuid.Must(uuid.NewV4()).String()
	now := time.Now().UTC()
	exp = now.Add(time.Duration(t.cfg.Session.TokenExpirySec) * time.Second).Unix()
	token = sign(t.cfg.Session.EncryptionKey, &SessionClaims{TokenID: tid, UserID: userID, Username: username, Vars: vars, ExpiresAt: exp, IssuedAt: now.Unix()})
	rexp := now.Add(time.Duration(t.cfg.Session.RefreshTokenExpirySec) * time.Second).Unix()
	refresh = sign(t.cfg.Session.RefreshEncryptionKey, &SessionClaims{TokenID: tid, UserID: userID, Username: username, Vars: vars, ExpiresAt: rexp, IssuedAt: now.Unix()})
	return token, refresh, exp
}

func sign(key string, c *SessionClaims) string {
	s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(key))
	return s
}

var errInvalidToken = errors.New("invalid token")

// Verify validates a session token, trying the current key then any legacy
// keys configured for rotation.
func (t *TokenService) Verify(token string) (*SessionClaims, error) {
	keys := append([]string{t.cfg.Session.EncryptionKey}, t.cfg.Session.LegacyEncryptionKeys...)
	return verifyWithKeys(token, keys)
}

func (t *TokenService) VerifyRefresh(token string) (*SessionClaims, error) {
	keys := append([]string{t.cfg.Session.RefreshEncryptionKey}, t.cfg.Session.LegacyRefreshEncryptionKeys...)
	return verifyWithKeys(token, keys)
}

func verifyWithKeys(token string, keys []string) (*SessionClaims, error) {
	for _, k := range keys {
		if k == "" {
			continue
		}
		key := []byte(k)
		claims := &SessionClaims{}
		parsed, err := jwt.ParseWithClaims(token, claims, func(tk *jwt.Token) (any, error) {
			if tk.Method.Alg() != jwt.SigningMethodHS256.Alg() {
				return nil, errInvalidToken
			}
			return key, nil
		}, jwt.WithExpirationRequired())
		if err != nil || !parsed.Valid {
			if errors.Is(err, jwt.ErrTokenExpired) {
				return nil, err
			}
			continue
		}
		if _, err := uuid.FromString(claims.UserID); err != nil {
			return nil, errInvalidToken
		}
		return claims, nil
	}
	return nil, errInvalidToken
}

// parseBearer extracts a bearer token from an Authorization header value.
func parseBearer(h string) (string, bool) {
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return h[len(prefix):], true
}

// parseBasic extracts the username part of a Basic auth header (the server
// key in Nakama's convention).
func parseBasic(h string) (string, bool) {
	const prefix = "Basic "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	b, err := base64.StdEncoding.DecodeString(h[len(prefix):])
	if err != nil {
		return "", false
	}
	user, _, _ := strings.Cut(string(b), ":")
	return user, true
}
