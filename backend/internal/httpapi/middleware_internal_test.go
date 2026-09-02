package httpapi

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// Struct tags cannot reference a constant, so the cookie name is spelled twice: once in
// SessionCookieName and once in logoutInput's tag. If they ever drift, logout stops
// finding the token and silently revokes nothing -- the browser cookie is cleared, the
// server session lives on, and nothing reports an error. This is the check that makes the
// duplication safe.
func TestLogoutReadsTheCookieThisPackageSets(t *testing.T) {
	field, ok := reflect.TypeFor[logoutInput]().FieldByName("Session")
	require.True(t, ok, "logoutInput has no Session field")
	require.Equal(t, SessionCookieName, field.Tag.Get("cookie"))
}
