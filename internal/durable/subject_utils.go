package durable

import (
	"github.com/arloliu/parti/v2/internal/dynamicbuild"
)

// partitionPlaceholder is the template token used to denote the partition id.
// Kept here for backwards-compatible test references in subject_utils_test.go.
const partitionPlaceholder = "{{.PartitionID}}"

// parseSubjectTemplateParts extracts the prefix and suffix surrounding the
// partition placeholder. Delegates to internal/dynamicbuild so the runtime
// and provision SDK share one implementation.
func parseSubjectTemplateParts(tmpl string) (prefix, suffix string, ok bool) {
	return dynamicbuild.ParseSubjectTemplateParts(tmpl)
}

// validateSubjectTemplate verifies the subject template parses, renders, and
// produces a valid subject. Delegates to internal/dynamicbuild.
func validateSubjectTemplate(tmpl string, allowWildcard bool) error {
	return dynamicbuild.ValidateSubjectTemplate(tmpl, allowWildcard)
}

// validateSubjectTokens validates every dot-separated token of subject.
// Used by broadcast_config.go for its WildcardFilter validation.
func validateSubjectTokens(subject string, allowWildcard bool) error {
	return dynamicbuild.ValidateSubjectTokens(subject, allowWildcard)
}

// extractPartitionIDFromSubject returns the partition id embedded in subject
// using the given prefix/suffix. Delegates to internal/dynamicbuild.
func extractPartitionIDFromSubject(subject, prefix, suffix string) (pid string, ok bool) {
	return dynamicbuild.ExtractPartitionIDFromSubject(subject, prefix, suffix)
}
