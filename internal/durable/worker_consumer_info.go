package durable

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/nats-io/nats.go/jetstream"
)

// WorkerSubjects returns a sorted copy of the currently managed per-subject keys.
// Returns nil when no subjects are active.
func (wc *WorkerConsumer) WorkerSubjects() []string {
	wc.mu.RLock()
	if len(wc.subjects) == 0 {
		wc.mu.RUnlock()
		return nil
	}
	out := make([]string, 0, len(wc.subjects))
	for s := range wc.subjects {
		out = append(out, s)
	}
	wc.mu.RUnlock()
	slices.Sort(out)

	return out
}

// WorkerConsumerInfo returns the ConsumerInfo for a specific subject's durable.
// It manages one durable per subject, so callers must specify the subject.
// If the consumer isn't currently tracked in-memory, this will create/bind it to
// return info, matching contract that durables are never deleted proactively.
func (wc *WorkerConsumer) WorkerConsumerInfo(ctx context.Context, subject string) (*jetstream.ConsumerInfo, error) {
	if subject == "" {
		return nil, errors.New("subject is required")
	}

	// Try fast path: use in-memory consumer if present
	wc.mu.RLock()
	pc := wc.subjects[subject]
	wc.mu.RUnlock()

	if pc != nil {
		return pc.Info(ctx)
	}

	// Bind or create durable for info retrieval
	durable := wc.perSubjectDurableName(wc.config.ConsumerPrefix, subject)
	c, err := wc.ensurePerSubjectConsumer(ctx, durable, subject)
	if err != nil {
		return nil, err
	}

	return c.Info(ctx)
}

// SubjectConsumerInfos returns ConsumerInfo for each provided subject's durable.
// The call is fail-fast: if retrieving info for any subject fails, it returns the error
// and no partial map. Callers can split or retry specific subjects if needed.
func (wc *WorkerConsumer) SubjectConsumerInfos(ctx context.Context, subjects []string) (map[string]*jetstream.ConsumerInfo, error) {
	if len(subjects) == 0 {
		return map[string]*jetstream.ConsumerInfo{}, nil
	}

	// Deduplicate while preserving a deterministic processing order.
	uniq := make(map[string]struct{}, len(subjects))
	order := make([]string, 0, len(subjects))
	for _, s := range subjects {
		if s == "" {
			return nil, errors.New("subject cannot be empty")
		}
		if _, ok := uniq[s]; !ok {
			uniq[s] = struct{}{}
			order = append(order, s)
		}
	}

	out := make(map[string]*jetstream.ConsumerInfo, len(uniq))
	for _, subject := range order {
		// Use in-memory consumer if available
		wc.mu.RLock()
		pc := wc.subjects[subject]
		wc.mu.RUnlock()

		var info *jetstream.ConsumerInfo
		var err error

		if pc != nil {
			info, err = pc.Info(ctx)
		} else {
			durable := wc.perSubjectDurableName(wc.config.ConsumerPrefix, subject)
			c, bindErr := wc.ensurePerSubjectConsumer(ctx, durable, subject)
			if bindErr != nil {
				return nil, fmt.Errorf("bind durable for %q: %w", subject, bindErr)
			}
			info, err = c.Info(ctx)
		}

		if err != nil {
			return nil, fmt.Errorf("consumer info for %q: %w", subject, err)
		}
		out[subject] = info
	}

	return out, nil
}
