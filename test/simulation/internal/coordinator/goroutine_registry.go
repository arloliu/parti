package coordinator

import (
	"context"
	"sync"
)

// GoroutineType represents the type of goroutine being tracked.
type GoroutineType string

const (
	// WorkerGoroutine represents a worker goroutine.
	WorkerGoroutine GoroutineType = "worker"
	// ProducerGoroutine represents a producer goroutine.
	ProducerGoroutine GoroutineType = "producer"
)

// GoroutineInfo contains information about a tracked goroutine.
type GoroutineInfo struct {
	ID     string
	Type   GoroutineType
	Cancel context.CancelFunc
	Active bool
}

// GoroutineRegistry tracks running goroutines for chaos engineering.
//
// This is used in all-in-one mode to enable goroutine-level chaos events
// (crashes, restarts, scaling) without requiring separate OS processes.
type GoroutineRegistry struct {
	mu         sync.RWMutex
	goroutines map[string]*GoroutineInfo
}

// NewGoroutineRegistry creates a new goroutine registry.
//
// Returns:
//   - *GoroutineRegistry: Initialized registry
func NewGoroutineRegistry() *GoroutineRegistry {
	return &GoroutineRegistry{
		goroutines: make(map[string]*GoroutineInfo),
	}
}

// Register adds a goroutine to the registry.
//
// Parameters:
//   - id: Unique identifier for the goroutine
//   - goroutineType: Type of goroutine (worker or producer)
//   - cancel: Cancel function to stop the goroutine
func (r *GoroutineRegistry) Register(id string, goroutineType GoroutineType, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.goroutines[id] = &GoroutineInfo{
		ID:     id,
		Type:   goroutineType,
		Cancel: cancel,
		Active: true,
	}
}

// Unregister removes a goroutine from the registry.
//
// Parameters:
//   - id: Unique identifier of the goroutine to remove
func (r *GoroutineRegistry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.goroutines, id)
}

// MarkInactive marks a goroutine as inactive (stopped but not yet unregistered).
//
// Parameters:
//   - id: Unique identifier of the goroutine
func (r *GoroutineRegistry) MarkInactive(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if info, exists := r.goroutines[id]; exists {
		info.Active = false
	}
}

// GetByType returns all goroutines of a specific type.
//
// Parameters:
//   - goroutineType: Type to filter by
//
// Returns:
//   - []*GoroutineInfo: Slice of matching goroutines
func (r *GoroutineRegistry) GetByType(goroutineType GoroutineType) []*GoroutineInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*GoroutineInfo
	for _, info := range r.goroutines {
		if info.Type == goroutineType && info.Active {
			result = append(result, info)
		}
	}

	return result
}

// GetActiveCount returns the count of active goroutines of a specific type.
//
// Parameters:
//   - goroutineType: Type to count
//
// Returns:
//   - int: Number of active goroutines
func (r *GoroutineRegistry) GetActiveCount(goroutineType GoroutineType) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	count := 0
	for _, info := range r.goroutines {
		if info.Type == goroutineType && info.Active {
			count++
		}
	}

	return count
}

// GetAll returns all registered goroutines.
//
// Returns:
//   - []*GoroutineInfo: Slice of all goroutines
func (r *GoroutineRegistry) GetAll() []*GoroutineInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*GoroutineInfo, 0, len(r.goroutines))
	for _, info := range r.goroutines {
		result = append(result, info)
	}

	return result
}
