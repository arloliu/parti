package provision

// driftBuilder accumulates per-field drift detail for one resource and emits
// the immutable / mutable DriftFinding pair. It replaces the per-classifier
// addImmutable / addMutable lazy-map-init closures and the duplicated findings
// assembly tail shared by classifyControlPlaneDrift, classifyPartitionSourceDrift,
// and classifyStreamDrift.
//
// Both maps are lazily allocated, so a builder with no recorded drift emits no
// findings.
type driftBuilder struct {
	mutable   map[string]any
	immutable map[string]any
}

// addImmutable records a drift on an immutable field (repaired by
// delete/recreate).
func (b *driftBuilder) addImmutable(field string, detail map[string]any) {
	if b.immutable == nil {
		b.immutable = map[string]any{}
	}
	b.immutable[field] = detail
}

// addMutable records a drift on a mutable field (reconcilable in place).
func (b *driftBuilder) addMutable(field string, detail map[string]any) {
	if b.mutable == nil {
		b.mutable = map[string]any{}
	}
	b.mutable[field] = detail
}

// findings emits the immutable finding (if any immutable drift was recorded)
// followed by the mutable finding (if any mutable drift was recorded), both
// stamped with the given drift kind and resource name. The order — immutable
// before mutable — matches the original per-classifier assembly.
func (b *driftBuilder) findings(kind, name string) []DriftFinding {
	findings := make([]DriftFinding, 0, 2)
	if len(b.immutable) > 0 {
		findings = append(findings, DriftFinding{
			Severity: SeverityDriftImmutable,
			Kind:     kind,
			Name:     name,
			Detail:   b.immutable,
		})
	}
	if len(b.mutable) > 0 {
		findings = append(findings, DriftFinding{
			Severity: SeverityDriftMutable,
			Kind:     kind,
			Name:     name,
			Detail:   b.mutable,
		})
	}

	return findings
}
