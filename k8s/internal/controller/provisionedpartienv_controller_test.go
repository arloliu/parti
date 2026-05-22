package controller

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/arloliu/parti/v2/k8s/api/v1alpha1"
	"github.com/arloliu/parti/v2/provision"
)

const testResyncPeriod = 5 * time.Minute

// testScheme builds a runtime.Scheme with the core types plus the operator
// CRD types registered.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	return scheme
}

// newCR builds a ProvisionedPartiEnv with a control-plane spec pointed at the
// given NATS server URL. Generation is set to 1 to mimic the apiserver
// stamping a generation on a created object.
func newCR(name, natsURL string) *v1alpha1.ProvisionedPartiEnv {
	return &v1alpha1.ProvisionedPartiEnv{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.ProvisionedPartiEnvSpec{
			NATS:     v1alpha1.NATSConnection{Server: natsURL},
			Instance: "test",
			Policy:   "warn",
			ControlPlane: &v1alpha1.ControlPlaneSpec{
				WorkerIDTTL:     metav1.Duration{Duration: time.Hour},
				ElectionTimeout: metav1.Duration{Duration: 30 * time.Second},
				HeartbeatTTL:    metav1.Duration{Duration: 10 * time.Second},
			},
		},
	}
}

// reconcilerFor builds a reconciler over a fake client seeded with objs. The
// fake client is configured with the status subresource for the CRD so
// Status().Update behaves like the real apiserver.
func reconcilerFor(t *testing.T, objs ...client.Object) (*ProvisionedPartiEnvReconciler, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.ProvisionedPartiEnv{}).
		WithObjects(objs...).
		Build()

	return &ProvisionedPartiEnvReconciler{
		Client:       c,
		Scheme:       scheme,
		ResyncPeriod: testResyncPeriod,
	}, c
}

// requestFor builds a reconcile.Request addressing cr.
func requestFor(cr *v1alpha1.ProvisionedPartiEnv) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: cr.Namespace,
		Name:      cr.Name,
	}}
}

// fetch re-reads the persisted CR after a reconcile.
func fetch(t *testing.T, c client.Client, name string) *v1alpha1.ProvisionedPartiEnv {
	t.Helper()

	var cr v1alpha1.ProvisionedPartiEnv
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: name}, &cr))

	return &cr
}

// readyCondition returns the persisted Ready condition, failing the test if it
// is absent.
func readyCondition(t *testing.T, cr *v1alpha1.ProvisionedPartiEnv) *metav1.Condition {
	t.Helper()

	cond := meta.FindStatusCondition(cr.Status.Conditions, conditionReady)
	require.NotNil(t, cond, "Ready condition must be persisted")

	return cond
}

func TestReconcile_HappyPath(t *testing.T) {
	cr := newCR("happy", startAnonymousNATS(t))
	r, c := reconcilerFor(t, cr)

	res, err := r.Reconcile(context.Background(), requestFor(cr))
	require.NoError(t, err)
	require.Equal(t, testResyncPeriod, res.RequeueAfter, "a converged CR requeues for periodic drift re-check")

	got := fetch(t, c, "happy")
	cond := readyCondition(t, got)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, reasonReconciled, cond.Reason)
	require.Equal(t, int64(1), got.Status.ObservedGeneration)
	require.NotNil(t, got.Status.LastReconcileTime)

	require.NotNil(t, got.Status.LastPlan, "LastPlan must be populated")
	require.Positive(t, got.Status.LastPlan.ActionCount)
	require.NotNil(t, got.Status.LastApply, "LastApply must be populated")
	require.Positive(t, got.Status.LastApply.ExecutedCount)
	require.Zero(t, got.Status.LastApply.ErrorCount)
}

func TestReconcile_CRNotFound(t *testing.T) {
	r, _ := reconcilerFor(t)

	// No object seeded — the fetch returns NotFound.
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "ghost"},
	})
	require.NoError(t, err, "a deleted CR is not an error")
	require.Equal(t, ctrl.Result{}, res)
}

func TestReconcile_SecretMissing(t *testing.T) {
	cr := newCR("secret-missing", startAnonymousNATS(t))
	cr.Spec.NATS.CredentialsSecret = &v1alpha1.NATSAuthSecret{
		Name:           "absent-secret",
		CredentialsKey: "creds",
	}
	r, c := reconcilerFor(t, cr)

	res, err := r.Reconcile(context.Background(), requestFor(cr))
	require.Error(t, err, "a missing Secret returns an error so controller-runtime backs off")
	require.Equal(t, ctrl.Result{}, res)

	got := fetch(t, c, "secret-missing")
	cond := readyCondition(t, got)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, reasonSecretMissing, cond.Reason)
	require.Equal(t, int64(1), got.Status.ObservedGeneration)
}

// TestReconcile_AnonymousWithSecretRefNoKey covers a CR that references a
// CredentialsSecret but names no auth key — a valid anonymous connection. The
// Secret object is deliberately absent: the reconciler must NOT fetch it (no
// auth key would be read from it), so the CR reconciles cleanly instead of
// backing off on a spurious SecretMissing.
func TestReconcile_AnonymousWithSecretRefNoKey(t *testing.T) {
	cr := newCR("anon-secret-ref", startAnonymousNATS(t))
	cr.Spec.NATS.CredentialsSecret = &v1alpha1.NATSAuthSecret{Name: "absent-secret"}
	r, c := reconcilerFor(t, cr)

	res, err := r.Reconcile(context.Background(), requestFor(cr))
	require.NoError(t, err, "a no-key Secret ref is anonymous — a missing Secret must not back off")
	require.Equal(t, testResyncPeriod, res.RequeueAfter)

	got := fetch(t, c, "anon-secret-ref")
	cond := readyCondition(t, got)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, reasonReconciled, cond.Reason)
}

// TestReconcile_MultiKeyAuthSpecInvalid covers a CR whose CredentialsSecret
// names more than one auth key — an invalid auth spec. The classification must
// be InvalidSpec (no requeue) and must NOT be masked as SecretMissing even
// though the referenced Secret is absent: a multi-key spec is invalid
// regardless of whether the Secret exists.
func TestReconcile_MultiKeyAuthSpecInvalid(t *testing.T) {
	cr := newCR("multi-key", startAnonymousNATS(t))
	cr.Spec.NATS.CredentialsSecret = &v1alpha1.NATSAuthSecret{
		Name:           "absent-secret",
		CredentialsKey: "creds",
		TokenKey:       "token",
	}
	r, c := reconcilerFor(t, cr)

	res, err := r.Reconcile(context.Background(), requestFor(cr))
	require.NoError(t, err, "an invalid auth spec is not retried")
	require.Equal(t, ctrl.Result{}, res, "an invalid auth spec is not requeued")

	got := fetch(t, c, "multi-key")
	cond := readyCondition(t, got)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, reasonInvalidSpec, cond.Reason,
		"a multi-key auth spec is InvalidSpec, not SecretMissing, even when the Secret is absent")
	require.Equal(t, int64(1), got.Status.ObservedGeneration)
}

func TestReconcile_NATSUnreachable(t *testing.T) {
	// A server that is not listening: connectNATS fails with a plain error.
	cr := newCR("nats-down", "nats://127.0.0.1:1")
	r, c := reconcilerFor(t, cr)

	res, err := r.Reconcile(context.Background(), requestFor(cr))
	require.Error(t, err, "an unreachable NATS server returns an error for backoff")
	require.Equal(t, ctrl.Result{}, res)

	got := fetch(t, c, "nats-down")
	cond := readyCondition(t, got)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, reasonNATSUnreachable, cond.Reason)
	require.Equal(t, int64(1), got.Status.ObservedGeneration)
}

func TestReconcile_InvalidSpec_HistoryOutOfRange(t *testing.T) {
	// History out of the 0-255 range trips the mapping guard (toProvisionConfig
	// error) — reachable here because the fake client does not enforce the
	// OpenAPI Maximum=255 bound.
	cr := newCR("bad-history", startAnonymousNATS(t))
	cr.Spec.PartitionSource = &v1alpha1.PartitionSourceSpec{
		Bucket:  "parti-partitions",
		Key:     "partitions",
		History: 256,
	}
	r, c := reconcilerFor(t, cr)

	res, err := r.Reconcile(context.Background(), requestFor(cr))
	require.NoError(t, err, "an invalid spec is not retried — the watch delivers the next event")
	require.Equal(t, ctrl.Result{}, res, "an invalid spec is not requeued")

	got := fetch(t, c, "bad-history")
	cond := readyCondition(t, got)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, reasonInvalidSpec, cond.Reason)
	require.Equal(t, int64(1), got.Status.ObservedGeneration)
}

func TestReconcile_InvalidSpec_StaticValidation(t *testing.T) {
	// A partition-source spec with a bucket name but an empty key fails
	// provision.Validate inside runProvision → ClassValidation.
	cr := newCR("bad-validation", startAnonymousNATS(t))
	cr.Spec.PartitionSource = &v1alpha1.PartitionSourceSpec{
		Bucket: "parti-partitions",
		Key:    "", // empty key — rejected by provision.Validate
	}
	r, c := reconcilerFor(t, cr)

	res, err := r.Reconcile(context.Background(), requestFor(cr))
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, res, "a static-validation failure is not requeued")

	got := fetch(t, c, "bad-validation")
	cond := readyCondition(t, got)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, reasonInvalidSpec, cond.Reason)
	require.Equal(t, int64(1), got.Status.ObservedGeneration)
}

func TestReconcile_ApplyResourceError(t *testing.T) {
	// A stream with Replicas:3 against a single-node embedded server is
	// rejected by NATS — a non-cancellation Apply error populating
	// Report.Errors → ClassApplyResourceError.
	cr := newCR("apply-error", startAnonymousNATS(t))
	cr.Spec.ControlPlane = nil
	cr.Spec.Streams = []v1alpha1.StreamSpec{{
		Name:     "resource-error-stream",
		Subjects: []string{"resource.error.>"},
		Replicas: 3,
	}}
	r, c := reconcilerFor(t, cr)

	res, err := r.Reconcile(context.Background(), requestFor(cr))
	require.NoError(t, err, "an apply-resource error is a periodic recheck, not a backoff crash-loop")
	require.Equal(t, testResyncPeriod, res.RequeueAfter)

	got := fetch(t, c, "apply-error")
	cond := readyCondition(t, got)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, reasonApplyError, cond.Reason)
	require.Equal(t, int64(1), got.Status.ObservedGeneration)
	require.NotNil(t, got.Status.LastApply)
	require.Positive(t, got.Status.LastApply.ErrorCount, "the resource error count must be surfaced")
	require.NotEmpty(t, got.Status.LastApply.Errors, "capped error messages must be surfaced")
}

// TestReconcile_AbortedBeforeSecretRead cancels the ctx before the reconcile
// begins. The Secret read (step 3) — or, for a CR with no Secret, connectNATS
// (step 4) — fails on the dead ctx and routes to finish, whose dead-ctx bypass
// returns ctx.Err() with no status write.
func TestReconcile_AbortedBeforeSecretRead(t *testing.T) {
	cr := newCR("abort-secret", startAnonymousNATS(t))
	cr.Spec.NATS.CredentialsSecret = &v1alpha1.NATSAuthSecret{
		Name:           "some-secret",
		CredentialsKey: "creds",
	}
	r, c := reconcilerFor(t, cr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead before Reconcile runs

	res, err := r.Reconcile(ctx, requestFor(cr))
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, ctrl.Result{}, res)

	// No status was persisted — the dead-ctx bypass skipped the write.
	got := fetch(t, c, "abort-secret")
	require.Nil(t, meta.FindStatusCondition(got.Status.Conditions, conditionReady),
		"no status condition must be persisted for an aborted reconcile")
	require.Zero(t, got.Status.ObservedGeneration)
}

// TestReconcile_AbortedDuringConnectNATS cancels the ctx before reconcile on a
// CR with no Secret, so connectNATS (step 4) is the first ctx-sensitive call;
// it returns context.Canceled and finish's dead-ctx bypass returns ctx.Err().
func TestReconcile_AbortedDuringConnectNATS(t *testing.T) {
	cr := newCR("abort-connect", startAnonymousNATS(t))
	r, c := reconcilerFor(t, cr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := r.Reconcile(ctx, requestFor(cr))
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, ctrl.Result{}, res)

	got := fetch(t, c, "abort-connect")
	require.Nil(t, meta.FindStatusCondition(got.Status.Conditions, conditionReady),
		"no status persisted when connectNATS aborts on a dead ctx")
}

// TestReconcile_AbortedAfterCRFetch cancels the ctx the instant the CR fetch
// (step 1) completes, via a Get interceptor. Step 4 connectNATS is then the
// first ctx-sensitive call: it returns ctx.Err() from its post-connect check,
// so Reconcile routes through finish's dead-ctx bypass and returns ctx.Err()
// with no status persisted. This exercises a cancellation window later than
// the bare pre-cancelled-ctx tests above. (The separate ClassApplyAborted
// early-return branch — cancellation observed inside runProvision — is covered
// by TestReconcile_ApplyAbortedEarlyReturn.)
func TestReconcile_AbortedAfterCRFetch(t *testing.T) {
	cr := newCR("abort-provision", startAnonymousNATS(t))

	scheme := testScheme(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.ProvisionedPartiEnv{}).
		WithObjects(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				// Let the CR fetch (step 1) succeed, then cancel so step 4
				// connectNATS observes the dead ctx at its post-connect check
				// and Reconcile routes through finish's dead-ctx bypass.
				err := cl.Get(ctx, key, obj, opts...)
				if _, ok := obj.(*v1alpha1.ProvisionedPartiEnv); ok {
					cancel()
				}

				return err
			},
		}).
		Build()
	r := &ProvisionedPartiEnvReconciler{Client: c, Scheme: scheme, ResyncPeriod: testResyncPeriod}

	res, err := r.Reconcile(ctx, requestFor(cr))
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, ctrl.Result{}, res)

	got := fetch(t, c, "abort-provision")
	require.Nil(t, meta.FindStatusCondition(got.Status.Conditions, conditionReady),
		"no status persisted for an aborted-in-provision reconcile")
}

// TestReconcile_AbortedDuringSecretRead exercises the cancellation window
// where the ctx is alive at the CR fetch (step 1) but dies before the Secret
// read (step 3). The Secret read fails (the Secret is absent), routing to
// finish(SecretMissing, ...); finish's dead-ctx bypass then returns ctx.Err()
// with no status write.
func TestReconcile_AbortedDuringSecretRead(t *testing.T) {
	cr := newCR("abort-secret-read", startAnonymousNATS(t))
	cr.Spec.NATS.CredentialsSecret = &v1alpha1.NATSAuthSecret{
		Name:           "some-secret",
		CredentialsKey: "creds",
	}
	scheme := testScheme(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.ProvisionedPartiEnv{}).
		WithObjects(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption,
			) error {
				// Let the CR fetch (step 1) succeed; cancel so the Secret read
				// (step 3) runs on a dead ctx.
				if _, ok := obj.(*v1alpha1.ProvisionedPartiEnv); ok {
					return cl.Get(ctx, key, obj, opts...)
				}
				cancel()

				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &ProvisionedPartiEnvReconciler{Client: c, Scheme: scheme, ResyncPeriod: testResyncPeriod}

	res, err := r.Reconcile(ctx, requestFor(cr))
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, ctrl.Result{}, res)

	got := fetch(t, c, "abort-secret-read")
	require.Nil(t, meta.FindStatusCondition(got.Status.Conditions, conditionReady),
		"no status persisted when the reconcile aborts during the Secret read")
}

// TestReconcile_ApplyAbortedEarlyReturn exercises the one Reconcile branch the
// other cancellation tests cannot reach: the ClassApplyAborted early return.
// connectNATS succeeds (the ctx is live through step 4), then runProvision
// returns ClassApplyAborted — the case where the step-2 Plan or Apply observed
// the cancellation. Reconcile must return ctx.Err() WITHOUT calling finish, so
// no status is persisted. The provisionFn seam forces the outcome
// deterministically without racing an embedded server.
func TestReconcile_ApplyAbortedEarlyReturn(t *testing.T) {
	cr := newCR("abort-early-return", startAnonymousNATS(t))
	r, c := reconcilerFor(t, cr)

	// connectNATS runs on a live ctx and succeeds; the cancellation is observed
	// inside runProvision, which the seam models by returning ClassApplyAborted
	// after cancelling the reconcile ctx (mirroring provision.Plan/Apply
	// returning ctx.Err()).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.provisionFn = func(_ context.Context, _ jetstream.JetStream, _ provision.Config) (provisionOutcome, error) {
		cancel()

		return provisionOutcome{Class: ClassApplyAborted}, context.Canceled
	}

	res, err := r.Reconcile(ctx, requestFor(cr))
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, ctrl.Result{}, res)

	// The ClassApplyAborted branch returns before finish — no status persisted.
	got := fetch(t, c, "abort-early-return")
	require.Nil(t, meta.FindStatusCondition(got.Status.Conditions, conditionReady),
		"the ClassApplyAborted early return must persist no status")
	require.Zero(t, got.Status.ObservedGeneration)
}

// TestReconcile_StatusConflictRecovered injects a single 409 on the failure
// path's status write; RetryOnConflict recovers and the intended failure
// status is persisted.
func TestReconcile_StatusConflictRecovered(t *testing.T) {
	cr := newCR("conflict-recover", "nats://127.0.0.1:1") // NATS unreachable → a failure path
	scheme := testScheme(t)

	var calls int
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.ProvisionedPartiEnv{}).
		WithObjects(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string,
				obj client.Object, opts ...client.SubResourceUpdateOption,
			) error {
				calls++
				if calls == 1 {
					return apierrors.NewConflict(
						schema.GroupResource{Group: "parti.io", Resource: "provisionedpartienvs"},
						obj.GetName(), context.DeadlineExceeded)
				}

				return cl.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &ProvisionedPartiEnvReconciler{Client: c, Scheme: scheme, ResyncPeriod: testResyncPeriod}

	res, err := r.Reconcile(context.Background(), requestFor(cr))
	require.Error(t, err, "the NATSUnreachable retErr survives a recovered conflict")
	require.Equal(t, ctrl.Result{}, res)
	require.GreaterOrEqual(t, calls, 2, "the status write must have been retried after the 409")

	got := fetch(t, c, "conflict-recover")
	cond := readyCondition(t, got)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, reasonNATSUnreachable, cond.Reason,
		"the intended failure status is persisted after the conflict retry")
	require.Equal(t, int64(1), got.Status.ObservedGeneration,
		"the captured generation survives the conflict retry")
}

// TestReconcile_StatusWriteFailurePrecedence injects a non-conflict (forbidden)
// error on the status write of an InvalidSpec branch — whose intended retErr is
// nil. Reconcile must return the non-nil write error, proving a failed terminal
// status write is never masked by a nil-error caller branch.
func TestReconcile_StatusWriteFailurePrecedence(t *testing.T) {
	// History out of range → the InvalidSpec branch, intended retErr nil.
	cr := newCR("write-fail", startAnonymousNATS(t))
	cr.Spec.PartitionSource = &v1alpha1.PartitionSourceSpec{
		Bucket:  "parti-partitions",
		Key:     "partitions",
		History: 256,
	}
	scheme := testScheme(t)

	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: "parti.io", Resource: "provisionedpartienvs"},
		cr.Name, context.Canceled)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.ProvisionedPartiEnv{}).
		WithObjects(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string,
				obj client.Object, opts ...client.SubResourceUpdateOption,
			) error {
				return forbidden
			},
		}).
		Build()
	r := &ProvisionedPartiEnvReconciler{Client: c, Scheme: scheme, ResyncPeriod: testResyncPeriod}

	res, err := r.Reconcile(context.Background(), requestFor(cr))
	require.Error(t, err, "a failed terminal-status write must surface as a non-nil Reconcile error")
	require.True(t, apierrors.IsForbidden(err),
		"the non-conflict write error is returned, not the InvalidSpec branch's nil retErr")
	require.Equal(t, ctrl.Result{}, res)
}

// TestReconcile_GenerationCaptureUnderConflict proves ObservedGeneration is the
// generation the reconcile acted on (captured before the retry loop), not a
// newer generation that landed mid-retry. The first status write returns a 409
// AND bumps the stored CR's generation; the retried write must still stamp the
// original generation.
func TestReconcile_GenerationCaptureUnderConflict(t *testing.T) {
	cr := newCR("gen-capture", "nats://127.0.0.1:1") // a failure path, simplest to exercise
	scheme := testScheme(t)

	var (
		calls  int
		fakeCl client.Client
	)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.ProvisionedPartiEnv{}).
		WithObjects(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string,
				obj client.Object, opts ...client.SubResourceUpdateOption,
			) error {
				calls++
				if calls == 1 {
					// Simulate a spec edit landing mid-retry: bump the stored
					// CR's generation, then return a 409 so finish re-fetches.
					var stored v1alpha1.ProvisionedPartiEnv
					if err := fakeCl.Get(ctx, client.ObjectKeyFromObject(obj), &stored); err != nil {
						return err
					}
					stored.Generation = 7
					stored.Spec.Instance = "edited-mid-retry"
					if err := fakeCl.Update(ctx, &stored); err != nil {
						return err
					}

					return apierrors.NewConflict(
						schema.GroupResource{Group: "parti.io", Resource: "provisionedpartienvs"},
						obj.GetName(), context.DeadlineExceeded)
				}

				return cl.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()
	fakeCl = c
	r := &ProvisionedPartiEnvReconciler{Client: c, Scheme: scheme, ResyncPeriod: testResyncPeriod}

	_, err := r.Reconcile(context.Background(), requestFor(cr))
	require.Error(t, err) // NATSUnreachable retErr

	got := fetch(t, c, "gen-capture")
	require.Equal(t, int64(7), got.Generation, "the mid-retry spec edit bumped the object generation")
	require.Equal(t, int64(1), got.Status.ObservedGeneration,
		"ObservedGeneration is the generation the reconcile acted on, captured before the retry loop")
	cond := readyCondition(t, got)
	require.Equal(t, int64(1), cond.ObservedGeneration,
		"the Ready condition's ObservedGeneration is also the captured generation")
}

// TestReconcile_MultipleCRsIndependent reconciles two CRs and asserts each
// records its own outcome with no cross-talk.
func TestReconcile_MultipleCRsIndependent(t *testing.T) {
	crOK := newCR("multi-ok", startAnonymousNATS(t))
	crDown := newCR("multi-down", "nats://127.0.0.1:1")
	r, c := reconcilerFor(t, crOK, crDown)

	resOK, errOK := r.Reconcile(context.Background(), requestFor(crOK))
	require.NoError(t, errOK)
	require.Equal(t, testResyncPeriod, resOK.RequeueAfter)

	resDown, errDown := r.Reconcile(context.Background(), requestFor(crDown))
	require.Error(t, errDown)
	require.Equal(t, ctrl.Result{}, resDown)

	gotOK := fetch(t, c, "multi-ok")
	require.Equal(t, reasonReconciled, readyCondition(t, gotOK).Reason)

	gotDown := fetch(t, c, "multi-down")
	require.Equal(t, reasonNATSUnreachable, readyCondition(t, gotDown).Reason)
}

// TestReconcile_DriftCounts pre-creates a drifting control-plane bucket so the
// Plan reports drift, then asserts LastPlan's drift counts match.
func TestReconcile_DriftCounts(t *testing.T) {
	url := startAnonymousNATS(t)

	// First reconcile provisions the full control-plane bucket set under
	// PolicyWarn.
	cr := newCR("drift", url)
	r, c := reconcilerFor(t, cr)
	_, err := r.Reconcile(context.Background(), requestFor(cr))
	require.NoError(t, err)

	// Mutate a provisioned bucket's TTL out-of-band so the next Plan observes
	// drift-mutable drift against the desired config.
	js := jsFor(t, url)
	driftExternalKV(t, js)

	// Capture the ground-truth PlanResult out-of-band with the same mapped
	// config the reconciler uses, so the persisted LastPlan can be matched
	// severity-by-severity (not just "some drift surfaced").
	cfg, err := toProvisionConfig(cr.Spec)
	require.NoError(t, err)
	wantPlan, err := provision.Plan(context.Background(), js, cfg)
	require.NoError(t, err)
	want := planSummary(wantPlan)
	require.NotNil(t, want, "the externally-mutated bucket must surface as drift")

	// Second reconcile: Plan now reports drift; LastPlan carries the counts.
	res, err := r.Reconcile(context.Background(), requestFor(cr))
	require.NoError(t, err)
	require.Equal(t, testResyncPeriod, res.RequeueAfter)

	got := fetch(t, c, "drift")
	require.NotNil(t, got.Status.LastPlan)
	// The persisted drift counts match the captured PlanResult severity by
	// severity.
	require.Equal(t, want.DriftInformational, got.Status.LastPlan.DriftInformational)
	require.Equal(t, want.DriftMutable, got.Status.LastPlan.DriftMutable)
	require.Equal(t, want.DriftImmutable, got.Status.LastPlan.DriftImmutable)
	require.Equal(t, want.DriftAdopted, got.Status.LastPlan.DriftAdopted)
	total := want.DriftInformational + want.DriftMutable + want.DriftImmutable + want.DriftAdopted
	require.Positive(t, total, "at least one drift finding must be present")
}

// driftExternalKV mutates an existing control-plane KV bucket so a subsequent
// Plan observes drift. It updates the worker-id bucket's TTL away from the
// desired value.
func driftExternalKV(t *testing.T, js jetstream.JetStream) {
	t.Helper()

	ctx := context.Background()
	// The control-plane buckets carry default names; list them and mutate the
	// first one's metadata to introduce drift.
	names := js.KeyValueStoreNames(ctx)
	var bucket string
	for name := range names.Name() {
		bucket = name
		break
	}
	require.NotEmpty(t, bucket, "the first reconcile must have created control-plane buckets")

	// Mutate the live bucket out-of-band: UpdateKeyValue with a sparse config
	// changes MaxValueSize and drops the Parti ownership marker, so the next
	// Plan observes drift against the desired (marked) config.
	_, err := js.UpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       bucket,
		MaxValueSize: 4096,
	})
	require.NoError(t, err)
}
