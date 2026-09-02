package config_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCheckMasterKEKAcceptsThirtyTwoBytes(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	require.NoError(t, config.CheckMasterKEK(key))
}

func TestCheckMasterKEKRejectsAbsentMalformedAndShortKeys(t *testing.T) {
	tests := map[string]string{
		"empty":         "",
		"not base64":    "not-base-64-!!!",
		"sixteen bytes": base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"sixty-four":    base64.StdEncoding.EncodeToString(make([]byte, 64)),
	}
	for name, key := range tests {
		t.Run(name, func(t *testing.T) {
			require.Error(t, config.CheckMasterKEK(key))
		})
	}
}

// L13: the key is a secret, so nothing derived from it may appear in the error a failing
// process prints -- not the value, not a prefix, not the decoded bytes.
func TestCheckMasterKEKErrorNeverEchoesTheKey(t *testing.T) {
	const secret = "c3VwZXItc2VjcmV0LW1hc3Rlci1rZXktdGhhdC1pcy10b28tbG9uZy10by1iZS1hLWtlaw=="
	err := config.CheckMasterKEK(secret)
	require.Error(t, err, "this fixture is the wrong length on purpose")

	require.NotContains(t, err.Error(), secret)
	require.NotContains(t, err.Error(), "super-secret")
	for _, fragment := range []string{"c3VwZXI", "bWFzdGVy", "kek"} {
		require.NotContains(t, err.Error(), fragment)
	}
	require.True(t, strings.Contains(err.Error(), "PLIMSOLL_MASTER_KEK"),
		"the error must still name the variable an operator has to fix")
}
