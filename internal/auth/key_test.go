package auth_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/EshkirenokCode/llm-gateway/internal/auth"
)

func TestGenerateProducesUniqueVerifiableKeys(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		key, err := auth.Generate("acme")
		require.NoError(t, err)

		require.True(t, strings.HasPrefix(key.Plaintext, "sk_acme_"))
		require.Equal(t, key.Plaintext[len(key.Plaintext)-4:], key.LastFour)
		require.Len(t, key.Hash, 32)
		require.True(t, auth.EqualHash(key.Hash, auth.HashKey(key.Plaintext)))

		_, dup := seen[key.Plaintext]
		require.False(t, dup, "generated a duplicate key")
		seen[key.Plaintext] = struct{}{}
	}
}

func TestGenerateRejectsBadPrefix(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{"", "ab", "UPPER", "with_underscore", strings.Repeat("a", 17)} {
		_, err := auth.Generate(prefix)
		require.Error(t, err, "prefix %q should be rejected", prefix)
	}
}

func TestHashIsStableAndDifferentPerKey(t *testing.T) {
	t.Parallel()

	a := auth.HashKey("sk_acme_one")
	b := auth.HashKey("sk_acme_one")
	c := auth.HashKey("sk_acme_two")

	require.True(t, auth.EqualHash(a, b))
	require.False(t, auth.EqualHash(a, c))
}

func TestParsePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key    string
		want   string
		wantOK bool
	}{
		{key: "sk_acme_abc123", want: "acme", wantOK: true},
		{key: "sk_acme", wantOK: false},
		{key: "pk_acme_abc", wantOK: false},
		{key: "sk_ACME_abc", wantOK: false},
		{key: "", wantOK: false},
	}

	for _, tt := range tests {
		got, ok := auth.ParsePrefix(tt.key)
		require.Equal(t, tt.wantOK, ok, "key %q", tt.key)
		require.Equal(t, tt.want, got, "key %q", tt.key)
	}
}

func TestFromHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		header string
		want   string
		wantOK bool
	}{
		{header: "Bearer sk_acme_x", want: "sk_acme_x", wantOK: true},
		{header: "bearer sk_acme_x", want: "sk_acme_x", wantOK: true},
		{header: "Bearer   sk_acme_x  ", want: "sk_acme_x", wantOK: true},
		{header: "Basic sk_acme_x", wantOK: false},
		{header: "Bearer ", wantOK: false},
		{header: "", wantOK: false},
	}

	for _, tt := range tests {
		got, ok := auth.FromHeader(tt.header)
		require.Equal(t, tt.wantOK, ok, "header %q", tt.header)
		require.Equal(t, tt.want, got, "header %q", tt.header)
	}
}
