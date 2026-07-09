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
	// Start starts a fresh goroutine with a new context derived from the provided parent.
	// It must set the registry entry Active=true when the goroutine starts.
	Start  func(ctx context.Context)
	Obj    any // optional underlying object (e.g., *worker.Worker) for advanced chaos actions
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
func (r *GoroutineRegistry) Register(id string, goroutineType GoroutineType, cancel context.CancelFunc, start func(ctx context.Context), obj any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.goroutines[id] = &GoroutineInfo{
		ID:     id,
		Type:   goroutineType,
		Cancel: cancel,
		Start:  start,
		Obj:    obj,
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

// MarkInactiveObj marks a goroutine inactive ONLY if the currently-registered
// object for id is still obj — an incarnation guard for a worker goroutine
// deactivating itself on exit.
//
// The bug this closes: on a crash+restart (Cancel → MarkInactive → Restart),
// Restart re-registers a FRESH object for the same id and sets it Active=true.
// The OLD goroutine's deferred cleanup then calls to deactivate id — but by
// then the registry entry is the successor object. An unconditional
// MarkInactive(id) here would flip the live successor to Active=false, making
// it vanish from GetByType while it is still running and processing (observed
// as a labeled worker reading empty labels / an unresolvable stable ID in the
// label-takeover-churn scenario under -race). Guarding on obj identity means a
// stale predecessor's cleanup is a no-op and only the current incarnation can
// deactivate itself. The intentional chaos-side MarkInactive(id) in the
// crash handler stays unconditional: it fires before Restart, when the current
// object IS the one being crashed.
func (r *GoroutineRegistry) MarkInactiveObj(id string, obj any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if info, exists := r.goroutines[id]; exists && info.Obj == obj {
		info.Active = false
	}
}

// Restart calls the Start callback for the given goroutine ID if available and marks it active.
// If no Start callback is set, this is a no-op.
func (r *GoroutineRegistry) Restart(ctx context.Context, id string) {
	r.mu.RLock()
	info, exists := r.goroutines[id]
	r.mu.RUnlock()
	if !exists || info.Start == nil {
		return
	}
	info.Start(ctx)
	r.mu.Lock()
	if info2, ok := r.goroutines[id]; ok {
		info2.Active = true
	}
	r.mu.Unlock()
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
