package subscription

import "strings"

// partitionPlaceholder is the template token used to denote the partition id.
const partitionPlaceholder = "{{.PartitionID}}"

// parseSubjectTemplateParts extracts the prefix and suffix surrounding the partition placeholder.
// Returns ok=false if the placeholder is not present.
func parseSubjectTemplateParts(tmpl string) (prefix, suffix string, ok bool) {
	idx := strings.Index(tmpl, partitionPlaceholder)
	if idx < 0 {
		return "", "", false
	}

	return tmpl[:idx], tmpl[idx+len(partitionPlaceholder):], true
}

// extractPartitionIDFromSubject returns the partition id embedded in subject using the given prefix/suffix.
// Returns ok=false when the subject doesn't match the expected prefix/suffix framing.
func extractPartitionIDFromSubject(subject, prefix, suffix string) (pid string, ok bool) {
	// If both empty, we cannot reliably extract based on template shape.
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
