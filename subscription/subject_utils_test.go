package subscription

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSubjectTemplateParts(t *testing.T) {
	tests := []struct {
		name   string
		tmpl   string
		prefix string
		suffix string
		ok     bool
	}{
		{
			name:   "prefix_only",
			tmpl:   "events." + partitionPlaceholder,
			prefix: "events.",
			suffix: "",
			ok:     true,
		},
		{
			name:   "prefix_and_suffix",
			tmpl:   "a." + partitionPlaceholder + ".b",
			prefix: "a.",
			suffix: ".b",
			ok:     true,
		},
		{
			name:   "only_placeholder",
			tmpl:   partitionPlaceholder,
			prefix: "",
			suffix: "",
			ok:     true,
		},
		{
			name:   "no_placeholder",
			tmpl:   "events.*",
			prefix: "",
			suffix: "",
			ok:     false,
		},
		{
			name:   "multiple_placeholders_first_wins",
			tmpl:   "x-" + partitionPlaceholder + "-y-" + partitionPlaceholder + "-z",
			prefix: "x-",
			suffix: "-y-" + partitionPlaceholder + "-z",
			ok:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, s, ok := parseSubjectTemplateParts(tt.tmpl)
			require.Equal(t, tt.ok, ok)
			if tt.ok {
				require.Equal(t, tt.prefix, p)
				require.Equal(t, tt.suffix, s)
			}
		})
	}
}

func TestExtractPartitionIDFromSubject(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		prefix  string
		suffix  string
		wantPID string
		wantOK  bool
	}{
		{
			name:    "basic_prefix_only",
			subject: "events.orders",
			prefix:  "events.",
			suffix:  "",
			wantPID: "orders",
			wantOK:  true,
		},
		{
			name:    "prefix_and_suffix",
			subject: "a.123.b",
			prefix:  "a.",
			suffix:  ".b",
			wantPID: "123",
			wantOK:  true,
		},
		{
			name:    "suffix_only",
			subject: "alpha.tail",
			prefix:  "",
			suffix:  ".tail",
			wantPID: "alpha",
			wantOK:  true,
		},
		{
			name:    "both_empty_not_allowed",
			subject: "anything",
			prefix:  "",
			suffix:  "",
			wantPID: "",
			wantOK:  false,
		},
		{
			name:    "mismatch_prefix",
			subject: "x.1",
			prefix:  "y.",
			suffix:  "",
			wantPID: "",
			wantOK:  false,
		},
		{
			name:    "mismatch_suffix",
			subject: "a.1.b",
			prefix:  "a.",
			suffix:  ".c",
			wantPID: "",
			wantOK:  false,
		},
		{
			name:    "empty_partition_between_prefix_suffix",
			subject: "a..b",
			prefix:  "a.",
			suffix:  ".b",
			wantPID: "",
			wantOK:  true, // framing matches; PID is empty
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid, ok := extractPartitionIDFromSubject(tt.subject, tt.prefix, tt.suffix)
			require.Equal(t, tt.wantOK, ok)
			if ok {
				require.Equal(t, tt.wantPID, pid)
			}
		})
	}
}
