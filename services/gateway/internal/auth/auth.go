// Package auth issues and verifies the (simulated) JWT access tokens of the gateway.
package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Issuer signs and verifies HS256 tokens.
type Issuer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// MinSecretLen is the shortest accepted signing secret (HS256 should have at least 256 bits).
const MinSecretLen = 32

func NewIssuer(secret string, ttl time.Duration) (*Issuer, error) {
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("JWT secret must be at least %d bytes", MinSecretLen)
	}
	return &Issuer{secret: []byte(secret), ttl: ttl, now: time.Now}, nil
}

// WithClock replaces the clock (tests).
func (i *Issuer) WithClock(now func() time.Time) *Issuer { c := *i; c.now = now; return &c }

func (i *Issuer) TTL() time.Duration { return i.ttl }

// Issue returns a token for the user that expires after the TTL.
func (i *Issuer) Issue(user string) (string, time.Time, error) {
	now := i.now()
	exp := now.Add(i.ttl)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   user,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(exp),
		Issuer:    "flashsale-gateway",
	})
	s, err := tok.SignedString(i.secret)
	return s, exp, err
}

// ErrInvalidToken is returned for every verification failure (the reason is not revealed to clients).
var ErrInvalidToken = errors.New("invalid or expired token")

// Verify checks signature, algorithm (HS256 only: "none" and RS/ES confusion are rejected), expiry and subject.
func (i *Issuer) Verify(token string) (string, error) {
	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return i.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer("flashsale-gateway"),
		jwt.WithTimeFunc(i.now),
	)
	if err != nil || claims.Subject == "" {
		return "", ErrInvalidToken
	}
	return claims.Subject, nil
}

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Credentials is the simulated user store: any well formed username with the demo password is accepted.
type Credentials struct{ password []byte }

func NewCredentials(demoPassword string) Credentials {
	return Credentials{password: []byte(demoPassword)}
}

// Check compares in constant time so the password cannot be guessed from response timing.
func (c Credentials) Check(username, password string) bool {
	okUser := usernamePattern.MatchString(username)
	okPass := subtle.ConstantTimeCompare([]byte(password), c.password) == 1
	return okUser && okPass
}
