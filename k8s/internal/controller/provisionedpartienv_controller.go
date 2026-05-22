package controller

import (
	"context"
	"errors"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/arloliu/parti/v2/k8s/api/v1alpha1"
	"github.com/arloliu/parti/v2/provision"
)

// The Ready condition type and its reason strings. The CRD carries a single
// condition type, "Ready"; its Reason field carries the granular outcome.
const (
	conditionReady = "Ready"

	// reasonReconciled marks Ready=True — the last reconcile applied cleanly.
	reasonReconciled = "Reconciled"
	// reasonInvalidSpec marks Ready=False — the Spec is malformed (mapping
	// failure or static-validation failure). No requeue.
	reasonInvalidSpec = "InvalidSpec"
	// reasonSecretMissing marks Ready=False — the referenced NATS auth Secret
	// (or a named key within it) is missing/unreadable. Backoff.
	reasonSecretMissing = "SecretMissing"
	// reasonNATSUnreachable marks Ready=False — NATS could not be reached or a
	// read-only Plan failed transiently. Backoff.
	reasonNATSUnreachable = "NATSUnreachable"
	// reasonApplyError marks Ready=False — Apply ran but one or more resources
	// were rejected. Periodic recheck.
	reasonApplyError = "ApplyError"
)

// errorCap bounds the number of ResourceError messages copied onto the CR
// status, keeping the status object within etcd size limits.
const errorCap = 10

// ProvisionedPartiEnvReconciler reconciles a ProvisionedPartiEnv object: it
// maps the CR Spec to a provision.Config, connects to NATS, drives the
// provision SDK (Validate → Plan → Apply), and records the outcome on the CR's
// status subresource.
type ProvisionedPartiEnvReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// ResyncPeriod is how often a converged (or apply-resource-errored) CR is
	// re-reconciled to re-check drift. It is operator-scoped, not per-CR.
	ResyncPeriod time.Duration

	// provisionFn drives the provision SDK. It defaults to runProvision; tests
	// override it to force a specific provisionOutcome (e.g. ClassApplyAborted)
	// without an embedded NATS server. Nil means use runProvision.
	provisionFn func(ctx context.Context, js jetstream.JetStream, cfg provision.Config) (provisionOutcome, error)
}

// +kubebuilder:rbac:groups=parti.io,resources=provisionedpartienvs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=parti.io,resources=provisionedpartienvs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile maps a ProvisionedPartiEnv to NATS infrastructure and records the
// outcome on the CR's status subresource.
//
// Every path after the CR fetch funnels through finish — the single terminal
// status write — so no branch returns without either persisting a terminal
// status or, when the reconcile ctx is already dead, returning the
// cancellation. The one exception is the ClassApplyAborted branch, which
// returns ctx.Err() directly: runProvision has already classified the abort,
// and the result is byte-identical to finish's own dead-ctx bypass.
func (r *ProvisionedPartiEnvReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Step 1: fetch the CR.
	var cr v1alpha1.ProvisionedPartiEnv
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted; no finalizer, nothing to persist.
			return ctrl.Result{}, nil
		}
		// The object could not be read — no status to write; let
		// controller-runtime apply backoff.
		return ctrl.Result{}, err
	}

	// Step 2: map Spec → provision.Config.
	cfg, err := toProvisionConfig(cr.Spec)
	if err != nil {
		// The only mapping error is the History narrowing guard — a spec
		// mistake. No requeue; the CR watch delivers the next event on edit.
		return r.finish(ctx, &cr, reasonInvalidSpec, nil, nil, ctrl.Result{}, nil)
	}

	// Step 3: read the NATS auth Secret only when exactly one auth key is
	// named — the sole case connectNATS reads secret.Data. With no key the
	// connection is anonymous and the Secret object is irrelevant; with more
	// than one key the auth spec is invalid regardless of the Secret, and
	// connectNATS classifies that as errAuthSpecInvalid without touching
	// secret.Data. Fetching the Secret unconditionally would wrongly back off
	// an anonymous CR, or mask a multi-key InvalidSpec as SecretMissing, when
	// the Secret happens to be absent.
	var secret *corev1.Secret
	if ref := cr.Spec.NATS.CredentialsSecret; authKeyCount(ref) == 1 {
		var s corev1.Secret
		key := types.NamespacedName{Namespace: cr.Namespace, Name: ref.Name}
		if secretErr := r.Get(ctx, key, &s); secretErr != nil {
			// Not found / unreadable: persist SecretMissing, then return the
			// error so controller-runtime applies exponential backoff.
			return r.finish(ctx, &cr, reasonSecretMissing, nil, nil, ctrl.Result{}, secretErr)
		}
		secret = &s
	}

	// Step 4: connect to NATS. The error is classified by connectNATS (W3);
	// route it without re-parsing.
	conn, err := connectNATS(ctx, cr.Spec.NATS, secret)
	if err != nil {
		switch {
		case errors.Is(err, errAuthSpecInvalid):
			// A spec mistake — not a transient fault. No requeue, nil retErr.
			return r.finish(ctx, &cr, reasonInvalidSpec, nil, nil, ctrl.Result{}, nil)
		case errors.Is(err, errSecretKeyMissing):
			// A named auth key is absent. Backoff.
			return r.finish(ctx, &cr, reasonSecretMissing, nil, nil, ctrl.Result{}, err)
		default:
			// Malformed creds/seed, or nats.Connect itself failing. Backoff.
			return r.finish(ctx, &cr, reasonNATSUnreachable, nil, nil, ctrl.Result{}, err)
		}
	}
	defer conn.close()

	// Step 5: drive the provision SDK and branch purely on outcome.Class.
	provisionFn := r.provisionFn
	if provisionFn == nil {
		provisionFn = runProvision
	}
	outcome, outcomeErr := provisionFn(ctx, conn.js, cfg)
	plan := planSummary(outcome.Plan)
	apply := applySummary(outcome.Report)

	switch outcome.Class {
	case ClassOK:
		// Converged. Periodic drift re-check.
		return r.finish(ctx, &cr, reasonReconciled, plan, apply,
			ctrl.Result{RequeueAfter: r.ResyncPeriod}, nil)

	case ClassValidation:
		// A statically-invalid Config. No requeue, nil retErr.
		return r.finish(ctx, &cr, reasonInvalidSpec, plan, apply, ctrl.Result{}, nil)

	case ClassTransientNATS:
		// A read-only Plan or pre-mutation IO failure. Backoff.
		return r.finish(ctx, &cr, reasonNATSUnreachable, plan, apply, ctrl.Result{}, outcomeErr)

	case ClassApplyResourceError:
		// Converged as far as possible; some resources were rejected. This is
		// a periodic recheck, not a tight backoff crash-loop.
		return r.finish(ctx, &cr, reasonApplyError, plan, apply,
			ctrl.Result{RequeueAfter: r.ResyncPeriod}, nil)

	case ClassApplyAborted:
		// The reconcile ctx was cancelled (manager shutdown). runProvision has
		// already classified the abort; return ctx.Err() directly without
		// calling finish — no status is persisted for an aborted reconcile,
		// and the next reconcile on a fresh ctx records the real outcome.
		return ctrl.Result{}, ctx.Err()

	default:
		// Unreachable: provisionClass is a closed enum set by runProvision.
		return ctrl.Result{}, errors.New("controller: unknown provision class")
	}
}

// finish is the single terminal-status write. Every reconcile path after the
// CR fetch funnels through it.
//
// Contract:
//   - Dead-ctx bypass (first action): if ctx is already dead, a status write
//     would itself fail; skip it and return (ctrl.Result{}, ctx.Err()),
//     overriding the caller's intended (result, retErr).
//   - Capture before retry: snapshot cr.Generation and the computed status
//     fields before entering the retry loop, so a spec edit landing mid-retry
//     cannot stamp an outcome computed for generation N as observed for N+1.
//   - Status write with conflict retry: inside retry.RetryOnConflict, re-fetch
//     the CR and re-apply the captured fields onto the fresh object — including
//     ObservedGeneration = the captured gen, never the re-fetched generation.
//   - Status-write-failure precedence: a RetryOnConflict error overrides the
//     caller's intended (result, retErr) — a failed terminal-status write must
//     surface as a non-nil Reconcile error so controller-runtime requeues.
//   - On success, returns the caller's intended (result, retErr).
func (r *ProvisionedPartiEnvReconciler) finish(
	ctx context.Context,
	cr *v1alpha1.ProvisionedPartiEnv,
	reason string,
	plan *v1alpha1.PlanSummary,
	apply *v1alpha1.ApplySummary,
	result ctrl.Result,
	retErr error,
) (ctrl.Result, error) {
	// Dead-ctx bypass — the first action, before any computation. A
	// client.Status().Update on a dead ctx would itself fail; skip it.
	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, err
	}

	// Capture before the retry loop: the generation this reconcile acted on,
	// and the computed status fields. Computed once so LastReconcileTime does
	// not drift across retries.
	gen := cr.Generation
	now := metav1.Now()
	condition := metav1.Condition{
		Type:   conditionReady,
		Status: metav1.ConditionFalse,
		Reason: reason,
	}
	if reason == reasonReconciled {
		condition.Status = metav1.ConditionTrue
	}

	writeErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh v1alpha1.ProvisionedPartiEnv
		if err := r.Get(ctx, client.ObjectKeyFromObject(cr), &fresh); err != nil {
			if apierrors.IsNotFound(err) {
				// The CR was deleted mid-retry; nothing to persist.
				return nil
			}

			return err
		}

		fresh.Status.ObservedGeneration = gen
		fresh.Status.LastReconcileTime = &now
		// SetStatusCondition stamps LastTransitionTime and preserves it across
		// a no-op status change.
		meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
			Type:               condition.Type,
			Status:             condition.Status,
			Reason:             condition.Reason,
			ObservedGeneration: gen,
		})
		// LastPlan / LastApply are set only from non-nil args; a failure
		// branch with no SDK outcome passes nil and leaves the prior
		// summaries untouched.
		if plan != nil {
			fresh.Status.LastPlan = plan
		}
		if apply != nil {
			fresh.Status.LastApply = apply
		}

		return r.Status().Update(ctx, &fresh)
	})
	if writeErr != nil {
		// Status-write-failure precedence: a failed terminal-status write must
		// surface as a non-nil Reconcile error, overriding the caller's
		// intended (result, retErr) — including a nil-error caller branch.
		return ctrl.Result{}, writeErr
	}

	return result, retErr
}

// planSummary converts a provision.PlanResult to the CR-status PlanSummary,
// counting drift findings by severity. It returns nil for a zero-value plan so
// finish leaves a prior LastPlan untouched on a pre-Plan failure.
func planSummary(p provision.PlanResult) *v1alpha1.PlanSummary {
	if len(p.Actions) == 0 && len(p.Drift) == 0 {
		return nil
	}

	s := &v1alpha1.PlanSummary{ActionCount: len(p.Actions)}
	for _, d := range p.Drift {
		switch d.Severity {
		case provision.SeverityInformational:
			s.DriftInformational++
		case provision.SeverityDriftMutable:
			s.DriftMutable++
		case provision.SeverityDriftImmutable:
			s.DriftImmutable++
		case provision.SeverityAdopted:
			s.DriftAdopted++
		}
	}

	return s
}

// applySummary converts a provision.Report to the CR-status ApplySummary,
// capping the copied error messages. It returns nil for a zero-value report so
// finish leaves a prior LastApply untouched on a pre-Apply failure.
func applySummary(rep provision.Report) *v1alpha1.ApplySummary {
	if len(rep.Executed) == 0 && len(rep.Skipped) == 0 &&
		len(rep.Errors) == 0 && !rep.Aborted {
		return nil
	}

	s := &v1alpha1.ApplySummary{
		ExecutedCount: len(rep.Executed),
		SkippedCount:  len(rep.Skipped),
		ErrorCount:    len(rep.Errors),
		Aborted:       rep.Aborted,
	}
	for i, e := range rep.Errors {
		if i >= errorCap {
			break
		}
		s.Errors = append(s.Errors, e.Error)
	}

	return s
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *ProvisionedPartiEnvReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ProvisionedPartiEnv{}).
		Complete(r)
}
