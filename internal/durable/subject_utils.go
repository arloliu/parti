package durable

import (
	"errors"
	"fmt"
	"strings"
	"text/template"
	"unicode"
)

// partitionPlaceholder is the template token used to denote the partition id.
const partitionPlaceholder = "{{.PartitionID}}"

var errInvalidSubject = errors.New("invalid subject")

// parseSubjectTemplateParts extracts the prefix and suffix surrounding the partition placeholder.
// Returns ok=false if the placeholder is not present.
func parseSubjectTemplateParts(tmpl string) (prefix, suffix string, ok bool) {
	before, after, ok0 := strings.Cut(tmpl, partitionPlaceholder)
	if !ok0 {
		return "", "", false
	}

	return before, after, true
}

func validateSubjectTokens(subject string, allowWildcard bool) error {
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

func validateSubjectTemplate(tmpl string, allowWildcard bool) error {
	if _, _, ok := parseSubjectTemplateParts(tmpl); !ok {
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

	return validateSubjectTokens(builder.String(), allowWildcard)
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
