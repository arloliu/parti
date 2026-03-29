package strategy

import (
	"fmt"

	"github.com/arloliu/parti/v2/types"
)

// ErrNoWorkers indicates that no workers were provided for assignment.
// It wraps types.ErrNoWorkersAvailable so callers can match with errors.Is(err, types.ErrNoWorkersAvailable).
var ErrNoWorkers = fmt.Errorf("no workers available for assignment: %w", types.ErrNoWorkersAvailable)
