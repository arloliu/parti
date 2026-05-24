package parti_test

import (
	"errors"
	"fmt"
	"testing"

	parti "github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestRootAliases_StreamMissing pins the v10 spec's exported-symbol audit
// for the root package (docs/plans/self-healing/09-pr9-spec.md § Verification
// gates): callers MUST be able to write
//
//	errors.Is(err, parti.ErrStreamMissing)
//
// and
//
//	var hook parti.StreamMissingHook = func(string) error { return nil }
//
// using only the root parti package. Without the aliases below, those
// snippets would not compile and the spec's "branching via errors.Is is
// the documented public shape" contract would be unmet.
func TestRootAliases_StreamMissing(t *testing.T) {
	// Identity: the root alias and the types value must be the same
	// sentinel — equality, not just errors.Is — so a caller that imports
	// only parti can interoperate with libraries that import types.
	require.Same(t, types.ErrStreamMissing, parti.ErrStreamMissing,
		"parti.ErrStreamMissing must be the same sentinel value as types.ErrStreamMissing")

	// errors.Is: a wrapped types.ErrStreamMissing must satisfy
	// errors.Is(err, parti.ErrStreamMissing) — the documented user
	// branching shape from types.ErrStreamMissing's godoc.
	wrapped := fmt.Errorf("stream %q: %w", "TEST_STREAM", types.ErrStreamMissing)
	require.True(t, errors.Is(wrapped, parti.ErrStreamMissing),
		"errors.Is must match the root alias against a wrapped types.ErrStreamMissing")

	// Hook type alias: parti.StreamMissingHook and types.StreamMissingHook
	// must be the same underlying type. Compile-time assertion via the
	// blank identifier proves bidirectional assignability — the explicit
	// type on the LHS is intentional documentation that survives a
	// future refactor that drops the alias (which would re-introduce
	// the spec violation).
	var hook parti.StreamMissingHook = func(stream string) error {
		if stream == "" {
			return errors.New("empty stream")
		}

		return nil
	}
	// parti → types and types → parti bidirectional assignability.
	// Without an alias these declarations would not compile, so the
	// explicit LHS type IS the assertion — staticcheck's QF1011 ("omit
	// the type") would defeat the test if followed.
	var _ types.StreamMissingHook = hook //nolint:staticcheck // QF1011: explicit type is the alias assertion
	var typesHook types.StreamMissingHook = func(string) error { return nil }
	var _ parti.StreamMissingHook = typesHook //nolint:staticcheck // QF1011: explicit type is the alias assertion
	require.NoError(t, hook("ok"))
	require.Error(t, hook(""), "hook should still be callable through the alias")
}
