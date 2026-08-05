package cryptox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHashKeyDeterministic(t *testing.T) {
	a := HashKey("gk-abc")
	b := HashKey("gk-abc")
	require.NotEqual(t, "gk-abc", a)
	require.Equal(t, a, b)
}

func TestNewGroupKey(t *testing.T) {
	raw, hash, prefix := NewGroupKey()
	require.Len(t, raw, 35) // gk- + 32 hex
	require.Equal(t, "gk-", raw[:3])
	require.Equal(t, HashKey(raw), hash)
	require.Len(t, prefix, 8)
}

func TestEqual(t *testing.T) {
	require.True(t, Equal("abc", "abc"))
	require.False(t, Equal("abc", "abd"))
}
