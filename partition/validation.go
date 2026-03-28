package partition

import "github.com/arloliu/parti/v2/internal/partutil"

// validateKeyForPublish validates a partition key before publishing.
func validateKeyForPublish(key string) error {
	return partutil.ValidateKeyForPublish(key, ErrEmptyKey, ErrInvalidKey)
}
