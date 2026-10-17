package auth_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/auth"
)

const secret = "test-secret"

func sign(t *testing.T, method jwt.SigningMethod, key string, claims jwt.MapClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(method, claims).SignedString([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + token
}

// claims returns valid claims with key set to value, or removed when value is nil.
func claims(key string, value any) jwt.MapClaims {
	now := time.Now().Unix()
	c := jwt.MapClaims{"sub": "42", "username": "token-name", "iat": now, "exp": now + 3600}
	if key != "" {
		if value == nil {
			delete(c, key)
		} else {
			c[key] = value
		}
	}
	return c
}

func TestAuthenticateAcceptsValidToken(t *testing.T) {
	user, err := auth.New([]byte(secret)).Authenticate(sign(t, jwt.SigningMethodHS256, secret, claims("", nil)))
	if err != nil || user != (auth.User{ID: 42, Name: "token-name"}) {
		t.Errorf("Authenticate = %+v, %v", user, err)
	}
}

func TestAuthenticateRejects(t *testing.T) {
	valid := strings.TrimPrefix(sign(t, jwt.SigningMethodHS256, secret, claims("", nil)), "Bearer ")
	parts := strings.Split(valid, ".")
	encode := base64.RawURLEncoding.EncodeToString

	cases := []struct {
		name, authorization string
		want                error
	}{
		{"no header", "", auth.ErrMissingToken},
		{"basic scheme", "Basic abc", auth.ErrMissingToken},
		{"lowercase scheme", "bearer " + valid, auth.ErrMissingToken},
		{"garbage", "Bearer abc", auth.ErrInvalidToken},
		{"wrong secret", sign(t, jwt.SigningMethodHS256, "other", claims("", nil)), auth.ErrInvalidToken},
		{"bad signature", "Bearer " + parts[0] + "." + parts[1] + "." + encode([]byte("wrong")), auth.ErrInvalidToken},
		{"alg none", "Bearer " + encode([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + parts[1] + ".", auth.ErrInvalidToken},
		{"HS384", sign(t, jwt.SigningMethodHS384, secret, claims("", nil)), auth.ErrInvalidToken},
		{"expired", sign(t, jwt.SigningMethodHS256, secret, claims("exp", time.Now().Unix()-10)), auth.ErrInvalidToken},
		{"no exp", sign(t, jwt.SigningMethodHS256, secret, claims("exp", nil)), auth.ErrInvalidToken},
		{"sub not an integer", sign(t, jwt.SigningMethodHS256, secret, claims("sub", "abc")), auth.ErrInvalidPayload},
		{"sub zero", sign(t, jwt.SigningMethodHS256, secret, claims("sub", "0")), auth.ErrInvalidPayload},
		{"sub signed", sign(t, jwt.SigningMethodHS256, secret, claims("sub", "+1")), auth.ErrInvalidPayload},
		{"sub a number", sign(t, jwt.SigningMethodHS256, secret, claims("sub", 1)), auth.ErrInvalidPayload},
		{"no sub", sign(t, jwt.SigningMethodHS256, secret, claims("sub", nil)), auth.ErrInvalidPayload},
		{"no username", sign(t, jwt.SigningMethodHS256, secret, claims("username", nil)), auth.ErrInvalidPayload},
		{"username a number", sign(t, jwt.SigningMethodHS256, secret, claims("username", 7)), auth.ErrInvalidPayload},
	}
	authenticator := auth.New([]byte(secret))
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := authenticator.Authenticate(c.authorization); !errors.Is(err, c.want) {
				t.Errorf("error = %v, want %v", err, c.want)
			}
		})
	}
}
