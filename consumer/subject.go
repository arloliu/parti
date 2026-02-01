package consumer

import (
	"errors"
	"strings"
	"unicode"
)

var errInvalidSubject = errors.New("invalid subject")

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
