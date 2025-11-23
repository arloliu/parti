package strategy

import "errors"

// ErrNoWorkers indicates that no workers were provided for assignment.
var ErrNoWorkers = errors.New("no workers available for assignment")
