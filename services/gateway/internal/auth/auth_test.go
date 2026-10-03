package auth_test

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/gateway/internal/auth"
)

const secret = "0123456789abcdef0123456789abcdef"

func issuer(t *testing.T) *auth.Issuer {
	t.Helper()
	i, err := auth.NewIssuer(secret, 15*time.Minute)
	require.NoError(t, err)
	return i
}

func TestIssueThenVerify(t *testing.T) {
	i := issuer(t)
	tok, exp, err := i.Issue("alice")
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(15*time.Minute), exp, 5*time.Second)

	user, err := i.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, "alice", user)
}

func TestShortSecretIsRejected(t *testing.T) {
	_, err := auth.NewIssuer("too-short", time.Minute)
	assert.Error(t, err)
}

func TestExpiredTokenIsRejected(t *testing.T) {
	now := time.Now()
	past := issuer(t).WithClock(func() time.Time { return now.Add(-time.Hour) })
	tok, _, err := past.Issue("alice")
	require.NoError(t, err)

	_, err = issuer(t).Verify(tok)
	assert.ErrorIs(t, err, auth.ErrInvalidToken)
}

func TestTokenSignedWithAnotherSecretIsRejected(t *testing.T) {
	other, _ := auth.NewIssuer("another-secret-another-secret-12345", time.Minute)
	tok, _, _ := other.Issue("mallory")
	_, err := issuer(t).Verify(tok)
	assert.ErrorIs(t, err, auth.ErrInvalidToken)
}

func sign(t *testing.T, method jwt.SigningMethod, key any, claims jwt.Claims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	require.NoError(t, err)
	return s
}

func TestAlgorithmConfusionIsRejected(t *testing.T) {
	good := jwt.RegisteredClaims{Subject: "mallory", Issuer: "flashsale-gateway", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}

	none := sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, good)
	_, err := issuer(t).Verify(none)
	assert.ErrorIs(t, err, auth.ErrInvalidToken, `"alg": "none" must never be accepted`)

	hs512 := sign(t, jwt.SigningMethodHS512, []byte(secret), good)
	_, err = issuer(t).Verify(hs512)
	assert.ErrorIs(t, err, auth.ErrInvalidToken, "only HS256 is accepted, even with the right secret")
}

func TestRequiredClaims(t *testing.T) {
	exp := jwt.NewNumericDate(time.Now().Add(time.Hour))
	cases := map[string]jwt.RegisteredClaims{
		"no expiry":     {Subject: "a", Issuer: "flashsale-gateway"},
		"no subject":    {Issuer: "flashsale-gateway", ExpiresAt: exp},
		"wrong issuer":  {Subject: "a", Issuer: "someone-else", ExpiresAt: exp},
		"empty issuer":  {Subject: "a", ExpiresAt: exp},
		"empty subject": {Subject: "", Issuer: "flashsale-gateway", ExpiresAt: exp},
	}
	for name, claims := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := issuer(t).Verify(sign(t, jwt.SigningMethodHS256, []byte(secret), claims))
			assert.ErrorIs(t, err, auth.ErrInvalidToken)
		})
	}
}

func TestGarbageTokens(t *testing.T) {
	for _, tok := range []string{"", "abc", "a.b.c", strings.Repeat("x", 10000)} {
		_, err := issuer(t).Verify(tok)
		assert.ErrorIs(t, err, auth.ErrInvalidToken)
	}
}

func TestCredentials(t *testing.T) {
	c := auth.NewCredentials("s3cret-demo")
	assert.True(t, c.Check("alice", "s3cret-demo"))
	assert.True(t, c.Check("a.b-c_9", "s3cret-demo"))
	assert.False(t, c.Check("alice", "wrong"))
	assert.False(t, c.Check("alice", ""))
	assert.False(t, c.Check("", "s3cret-demo"))
	assert.False(t, c.Check("al ice", "s3cret-demo"), "usernames are restricted to a safe alphabet")
	assert.False(t, c.Check(strings.Repeat("a", 65), "s3cret-demo"))
	assert.False(t, c.Check("alice\n", "s3cret-demo"))
}
