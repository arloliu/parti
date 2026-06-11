package durable

import "errors"

// Guardrail errors for worker consumer updates.

// ErrMaxSubjectsExceeded indicates the requested subjects exceed MaxConcurrentSubjects.
var ErrMaxSubjectsExceeded = errors.New("worker consumer subjects exceed MaxConcurrentSubjects cap")

// ErrWorkerIDMutation indicates workerID changed while AllowWorkerIDChange=false.
var ErrWorkerIDMutation = errors.New("workerID mutation is not allowed by configuration")
