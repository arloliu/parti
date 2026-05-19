package provision_test

import (
	"encoding/json"
	"testing"

	"github.com/arloliu/parti/v2/provision"
	"github.com/stretchr/testify/require"
)

// TestEnvelopes_CarryAPIVersionAndKind verifies that every JSON-emitted
// output envelope carries the v1 provision apiVersion and the documented
// kind field. Operator CI keys on these.
func TestEnvelopes_CarryAPIVersionAndKind(t *testing.T) {
	t.Parallel()

	t.Run("Snapshot", func(t *testing.T) {
		t.Parallel()
		s := provision.Snapshot{
			APIVersion: provision.APIVersionProvisionV1,
			Kind:       provision.KindSnapshot,
		}
		b, err := json.Marshal(s)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(b, &m))
		require.Equal(t, "parti.io/provision/v1", m["apiVersion"])
		require.Equal(t, "Snapshot", m["kind"])
	})

	t.Run("PlanResult", func(t *testing.T) {
		t.Parallel()
		p := provision.PlanResult{
			APIVersion: provision.APIVersionProvisionV1,
			Kind:       provision.KindPlan,
		}
		b, err := json.Marshal(p)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(b, &m))
		require.Equal(t, "parti.io/provision/v1", m["apiVersion"])
		require.Equal(t, "Plan", m["kind"])
	})

	t.Run("Report", func(t *testing.T) {
		t.Parallel()
		r := provision.Report{
			APIVersion: provision.APIVersionProvisionV1,
			Kind:       provision.KindReport,
		}
		b, err := json.Marshal(r)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(b, &m))
		require.Equal(t, "parti.io/provision/v1", m["apiVersion"])
		require.Equal(t, "Report", m["kind"])
	})
}
