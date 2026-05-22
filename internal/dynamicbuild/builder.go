// Package dynamicbuild contains pure helpers shared by the runtime
// dynamic-consumer code path in internal/durable and the provision SDK's
// dynamic-consumer alignment builder. Every exported function is a pure
// transform — no NATS I/O, no goroutines, no global state.
//
// The package exists because two callers need identical subject generation,
// durable-name derivation, and jetstream.ConsumerConfig construction:
//
//   - The runtime worker_consumer in internal/durable, which owns the
//     producer of the JetStream consumer (calls jsutil.EnsureConsumer with
//     the result of ConsumerConfig).
//   - The provision SDK's PlanDynamicConsumers, which produces a deterministic
//     plan without touching NATS.
//
// Both callers must agree byte-for-byte on identity fields (Name, Durable,
// FilterSubject, AckPolicy, DeliverPolicy). The Defaults struct captures the
// runtime-tunable fields that vary by consumer option but do not affect
// identity; provision passes runtime defaults, and the runtime passes its
// configured values.
package dynamicbuild

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/zeebo/xxh3"

	"github.com/arloliu/parti/v2/types"
)

// partitionPlaceholder is the template token used to denote the partition id.
// Mirrors the constant in internal/durable so the helper is self-contained.
const partitionPlaceholder = "{{.PartitionID}}"

// errInvalidSubject is returned by validateSubjectTokens when a subject token
// is empty, contains whitespace, or carries a disallowed wildcard.
var errInvalidSubject = errors.New("invalid subject")

// Defaults captures the runtime-tunable fields that the canonical
// jetstream.ConsumerConfig literal in internal/durable reads off
// WorkerConsumerConfig. These are the fields that do NOT participate in
// the W4 v1 equality subset; provision passes runtime defaults, and the
// runtime passes its configured values.
//
// The struct mirrors worker_consumer.go:461-473 one-for-one. Any addition
// here must be matched in ConsumerConfig and in the runtime callsites.
type Defaults struct {
	AckPolicy             jetstream.AckPolicy
	AckWait               time.Duration
	MaxDeliver            int
	InactiveThreshold     time.Duration
	MaxWaiting            int
	MaxAckPending         int
	ConsumerMemoryStorage bool
	ConsumerReplicas      int
}

// DefaultDynamicDefaults returns the Defaults a default-configured
// consumer.Dynamic uses. The values mirror consumer.defaultOptions()
// (consumer/options.go) — the dynamic-consumer option defaults.
//
// The provision SDK's PlanDynamicConsumers builds its expected
// jetstream.ConsumerConfig from these so a consumer it precreates carries the
// same NATS-immutable fields (AckPolicy, MaxWaiting, MemoryStorage) the
// runtime will use. Those three cannot be changed by a later
// CreateOrUpdateConsumer (NATS rejects the update), so a precreated consumer
// must match the runtime on them or runtime startup fails. The mutable fields
// are included for completeness; the runtime overwrites them freely on start.
func DefaultDynamicDefaults() Defaults {
	return Defaults{
		AckPolicy:             jetstream.AckExplicitPolicy,
		AckWait:               30 * time.Second,
		MaxDeliver:            -1,
		InactiveThreshold:     24 * time.Hour,
		MaxWaiting:            2,
		MaxAckPending:         0,
		ConsumerMemoryStorage: false,
		ConsumerReplicas:      0,
	}
}

// ConsumerConfig builds the per-subject jetstream.ConsumerConfig used by
// both the runtime (internal/durable.WorkerConsumer.ensurePerSubjectConsumer)
// and the provision SDK's PlanDynamicConsumers.
//
// The five identity fields — Name, Durable, FilterSubject, AckPolicy,
// DeliverPolicy — are deterministic from the inputs. AckPolicy and
// DeliverPolicy are explicitly assigned from def.AckPolicy and the
// jetstream zero value DeliverAllPolicy, so the result is robust to any
// future iota reordering in nats.go.
//
// Note: the runtime literal at worker_consumer.go:461-473 currently relies
// on the jetstream.AckPolicy zero value AckExplicitPolicy when AckPolicy
// is unset. Provision passes jetstream.AckExplicitPolicy explicitly in
// def.AckPolicy; the runtime passes wc.config.AckPolicy which defaults to
// AckExplicitPolicy via fuda. Both code paths therefore yield the same
// effective value.
func ConsumerConfig(durable, subject string, def Defaults) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Name:              durable,
		Durable:           durable,
		FilterSubject:     subject,
		AckPolicy:         def.AckPolicy,
		DeliverPolicy:     jetstream.DeliverAllPolicy,
		AckWait:           def.AckWait,
		MaxDeliver:        def.MaxDeliver,
		InactiveThreshold: def.InactiveThreshold,
		MaxWaiting:        def.MaxWaiting,
		MaxAckPending:     def.MaxAckPending,
		MemoryStorage:     def.ConsumerMemoryStorage,
		Replicas:          def.ConsumerReplicas,
	}
}

// PerSubjectDurableName returns a stable, sanitized durable name for a
// given subject. The format is "<consumerPrefix>_<sanitizedPartitionID>_<hash>",
// where the hash is the xxh3 of the subject (16 hex characters), the
// sanitized partition id is the partition id with disallowed runes replaced
// by underscores, and the partition id portion is truncated to 50 characters.
//
// The xxh3 suffix ensures uniqueness even when sanitization causes the
// partition id portion to collide.
//
// partitionPrefix and partitionSuffix are the parts of the subject template
// surrounding the {{.PartitionID}} placeholder. When the subject does not
// match those framing parts, the partition id falls back to the full subject.
func PerSubjectDurableName(consumerPrefix, subject, partitionPrefix, partitionSuffix string) string {
	partID, ok := ExtractPartitionIDFromSubject(subject, partitionPrefix, partitionSuffix)
	if !ok || partID == "" {
		partID = subject
	}

	// Hash the subject to ensure uniqueness even if sanitization causes collisions.
	h := xxh3.HashString(subject)

	sanitizedPartID := SanitizeConsumerName(partID)
	if len(sanitizedPartID) > 50 {
		sanitizedPartID = sanitizedPartID[:50]
	}

	return fmt.Sprintf("%s_%s_%016x", consumerPrefix, sanitizedPartID, h)
}

// BuildSubjects generates a sorted, deduplicated list of subjects from
// partitions using the provided subject template. The template must contain
// the {{.PartitionID}} placeholder.
//
// The output ordering is deterministic via slices.Sort.
//
// Errors:
//   - parse failure: wraps the text/template parse error.
//   - empty partition Keys: returns "partition has no keys".
//   - render failure: wraps the text/template execute error.
func BuildSubjects(subjectTemplate string, partitions []types.Partition) ([]string, error) {
	if len(partitions) == 0 {
		return []string{}, nil
	}

	tmpl, err := template.New("subject").Parse(subjectTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse subject template: %w", err)
	}

	m := make(map[string]struct{}, len(partitions))
	for _, p := range partitions {
		subj, err := renderSubject(tmpl, p)
		if err != nil {
			return nil, err
		}
		m[subj] = struct{}{}
	}

	subjects := make([]string, 0, len(m))
	for s := range m {
		subjects = append(subjects, s)
	}
	slices.Sort(subjects)

	return subjects, nil
}

// GenerateSubject renders the subject template for a single partition. The
// template must already be parsed by the caller path, but GenerateSubject
// accepts the raw template string and parses it itself so it can be called
// from non-runtime code paths (provision builder, tests).
//
// Returns "partition has no keys" if partition.Keys is empty.
func GenerateSubject(subjectTemplate string, partition types.Partition) (string, error) {
	tmpl, err := template.New("subject").Parse(subjectTemplate)
	if err != nil {
		return "", fmt.Errorf("parse subject template: %w", err)
	}
	return renderSubject(tmpl, partition)
}

// renderSubject is the shared rendering routine used by BuildSubjects
// and GenerateSubject. It accepts an already-parsed template so callers
// that render many partitions parse exactly once.
func renderSubject(tmpl *template.Template, partition types.Partition) (string, error) {
	if len(partition.Keys) == 0 {
		return "", errors.New("partition has no keys")
	}

	type subjectContext struct {
		PartitionID string
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, subjectContext{PartitionID: partition.SubjectKey()}); err != nil {
		return "", fmt.Errorf("failed to execute subject template: %w", err)
	}

	return buf.String(), nil
}

// ParseSubjectTemplateParts extracts the prefix and suffix surrounding the
// partition placeholder. Returns ok=false if the placeholder is not present.
func ParseSubjectTemplateParts(tmpl string) (prefix, suffix string, ok bool) {
	before, after, ok0 := strings.Cut(tmpl, partitionPlaceholder)
	if !ok0 {
		return "", "", false
	}
	return before, after, true
}

// ValidateSubjectTemplate verifies that a subject template contains the
// {{.PartitionID}} placeholder, parses successfully (with missing-key
// detection), and renders to a valid subject. allowWildcard controls
// whether wildcard tokens "*" and ">" are allowed in the rendered subject.
//
// Used by both the runtime (internal/durable) and the provision SDK's
// PlanDynamicConsumers static validation.
func ValidateSubjectTemplate(tmpl string, allowWildcard bool) error {
	if _, _, ok := ParseSubjectTemplateParts(tmpl); !ok {
		return fmt.Errorf("invalid subject template: %q (must contain %s)", tmpl, partitionPlaceholder)
	}

	parsed, err := template.New("subject").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return err
	}

	var builder strings.Builder
	if err := parsed.Execute(&builder, struct{ PartitionID string }{PartitionID: "p1"}); err != nil {
		return err
	}

	return ValidateSubjectTokens(builder.String(), allowWildcard)
}

// ValidateSubjectTokens checks every dot-separated token of subject for
// validity. allowWildcard controls whether "*" and ">" tokens are accepted.
// Mirrors the validation in internal/durable.subject_utils.go so the two
// packages stay aligned.
func ValidateSubjectTokens(subject string, allowWildcard bool) error {
	if subject == "" {
		return errInvalidSubject
	}

	tokens := strings.Split(subject, ".")
	for i, token := range tokens {
		if token == "" {
			return errInvalidSubject
		}
		if strings.IndexFunc(token, unicode.IsSpace) >= 0 {
			return errInvalidSubject
		}
		if strings.ContainsAny(token, "*>") {
			if !allowWildcard {
				return errInvalidSubject
			}
			if token == ">" {
				if i != len(tokens)-1 {
					return errInvalidSubject
				}
				continue
			}
			if token == "*" {
				continue
			}

			return errInvalidSubject
		}
	}

	return nil
}

// ExtractPartitionIDFromSubject returns the partition id embedded in subject
// using the given prefix/suffix. Returns ok=false when the subject doesn't
// match the expected prefix/suffix framing.
func ExtractPartitionIDFromSubject(subject, prefix, suffix string) (pid string, ok bool) {
	if prefix == "" && suffix == "" {
		return "", false
	}
	if !strings.HasPrefix(subject, prefix) || !strings.HasSuffix(subject, suffix) {
		return "", false
	}
	start := len(prefix)
	end := len(subject) - len(suffix)
	if start > end || start < 0 || end > len(subject) {
		return "", false
	}

	return subject[start:end], true
}

// SanitizeConsumerName replaces any rune in name that is not in the allowed
// consumer-name charset (a-z, A-Z, 0-9, '-', '_') with '_'.
func SanitizeConsumerName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if IsAllowedConsumerRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}

	return b.String()
}

// IsAllowedConsumerRune reports whether r is allowed in a NATS consumer
// name (a-z, A-Z, 0-9, '-', '_').
func IsAllowedConsumerRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '-' || r == '_':
		return true
	default:
		return false
	}
}
