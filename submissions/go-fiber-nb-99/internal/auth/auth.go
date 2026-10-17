// Package auth verifies the HS256 bearer tokens of authenticated requests.
package auth

import (
	"errors"
	"strconv"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// The errors' messages are the API's 401 messages.
var (
	ErrMissingToken   = errors.New("missing bearer token")
	ErrInvalidToken   = errors.New("invalid or expired token")
	ErrInvalidPayload = errors.New("invalid token payload")
)

// User is the caller named by a verified token. The name is not looked up.
type User struct {
	ID   int64
	Name string
}

type Authenticator struct {
	secret []byte
	parser *jwt.Parser
}

func New(secret []byte) *Authenticator {
	return &Authenticator{
		secret: secret,
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithExpirationRequired(),
		),
	}
}

// claims decodes the registered claims, which the parser validates, and keeps
// sub and username untyped so a wrong type is an invalid payload rather than a
// malformed token. The outer sub field takes precedence over the embedded one.
type claims struct {
	jwt.RegisteredClaims
	Subject  any `json:"sub"`
	Username any `json:"username"`
}

// Authenticate verifies the HS256 signature and expiry of the bearer token in
// an Authorization header value on every call and returns its user.
func (a *Authenticator) Authenticate(authorization string) (User, error) {
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok {
		return User{}, ErrMissingToken
	}
	var c claims
	_, err := a.parser.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) {
		return a.secret, nil
	})
	if err != nil {
		return User{}, ErrInvalidToken
	}
	subject, _ := c.Subject.(string)
	name, isString := c.Username.(string)
	id, isID := parseUserID(subject)
	if !isString || !isID {
		return User{}, ErrInvalidPayload
	}
	return User{ID: id, Name: name}, nil
}

// parseUserID accepts only decimal digits, so signs, spaces, and fractions
// are rejected.
func parseUserID(text string) (int64, bool) {
	if text == "" || strings.Trim(text, "0123456789") != "" {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	return value, err == nil && value > 0
}
