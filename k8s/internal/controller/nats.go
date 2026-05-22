package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
	corev1 "k8s.io/api/core/v1"

	"github.com/arloliu/parti/v2/k8s/api/v1alpha1"
)

// errAuthSpecInvalid is the classified sentinel returned when the
// NATSAuthSecret reference sets more than one of CredentialsKey / TokenKey /
// NKeyKey. It is a pure pre-connect check: the auth spec is internally
// contradictory, so no connection is attempted. The W4 reconciler routes a
// connectNATS error wrapping this sentinel to the Ready=False reason
// InvalidSpec (no requeue — a spec mistake is not a transient fault).
var errAuthSpecInvalid = errors.New("nats auth spec invalid: at most one of credentialsKey, tokenKey, nkeyKey may be set")

// errSecretKeyMissing is the classified sentinel returned when a named auth
// key is absent from the referenced Secret's Data. It is a pure pre-connect
// check. The W4 reconciler routes a connectNATS error wrapping this sentinel
// to the Ready=False reason SecretMissing (backoff — the Secret may appear).
var errSecretKeyMissing = errors.New("nats auth secret key missing")

// natsConn bundles a connected NATS client with its JetStream context and,
// on the nkey auth path, the nkeys.KeyPair that must stay live for the
// connection's whole lifetime.
//
// The keypair is held — not discarded after nats.Connect returns — because
// nats.Nkey's signature callback closes over it and is invoked again on
// every reconnect-time nonce signature (e.g. after a NATS server restart).
// Dropping the keypair would silently break reconnection. close() wipes it.
type natsConn struct {
	nc *nats.Conn
	js jetstream.JetStream
	// kp is non-nil only on the nkey auth path. close() calls kp.Wipe().
	kp nkeys.KeyPair
}

// close closes the underlying NATS connection and, on the nkey path, wipes
// the held key pair's seed from memory. It is connection-scoped teardown:
// connectNATS opens one connection per reconcile and the caller defers this.
//
// close uses nc.Close (not nc.Drain): the connection is being discarded at
// the end of a reconcile, so a synchronous close is simpler and faster than
// an asynchronous drain whose flush guarantees the operator does not need.
func (n *natsConn) close() {
	if n.nc != nil {
		n.nc.Close()
	}
	if n.kp != nil {
		n.kp.Wipe()
	}
}

// connectNATS opens a NATS connection for the operator from a CR's
// NATSConnection block plus, when CredentialsSecret is set, the auth material
// read from the referenced Secret's Data by the named key.
//
// All three auth modes are fully in-memory — no temp file is ever created:
//   - .creds bytes (CredentialsKey) → nats.UserCredentialBytes.
//   - token (TokenKey)             → nats.Token.
//   - nkey seed (NKeyKey)          → nats.Nkey with an in-memory keypair.
//
// No auth key set → anonymous connection.
//
// connectNATS returns a *classified* error so the W4 reconciler can pick the
// correct Ready=False reason without re-parsing the error string:
//   - more than one auth key set → wraps errAuthSpecInvalid (a pre-connect
//     check; W4 → InvalidSpec).
//   - a named auth key absent from secret.Data → wraps errSecretKeyMissing
//     (a pre-connect check; W4 → SecretMissing).
//   - every other failure (malformed creds/seed, non-user nkey seed,
//     nats.Connect failure) → a plain error wrapping NEITHER sentinel
//     (W4 → NATSUnreachable).
func connectNATS(ctx context.Context, conn v1alpha1.NATSConnection, secret *corev1.Secret) (*natsConn, error) {
	opts := []nats.Option{
		nats.Name("parti-operator"),
	}

	// kp is set only on the nkey path; it is handed to the returned natsConn
	// so its lifetime matches the connection's.
	var kp nkeys.KeyPair

	if conn.CredentialsSecret != nil {
		authOpt, authKP, err := buildAuthOption(conn.CredentialsSecret, secret)
		if err != nil {
			return nil, err
		}
		if authOpt != nil {
			opts = append(opts, authOpt)
		}
		kp = authKP
	}

	wipeKP := func() {
		if kp != nil {
			kp.Wipe()
		}
	}

	nc, err := nats.Connect(conn.Server, opts...)
	if err != nil {
		// A nats.Connect failure (network unreachable, server-side auth
		// rejection) is a plain error — neither sentinel. W4 → NATSUnreachable.
		wipeKP()
		return nil, fmt.Errorf("connect to NATS server %q: %w", conn.Server, err)
	}

	// nats.Connect does not take a ctx; honour cancellation immediately after.
	if err := ctx.Err(); err != nil {
		nc.Close()
		wipeKP()
		return nil, err
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		wipeKP()
		return nil, fmt.Errorf("create JetStream context: %w", err)
	}

	return &natsConn{nc: nc, js: js, kp: kp}, nil
}

// authKeyCount returns how many of a NATSAuthSecret's three auth-key selectors
// (CredentialsKey / TokenKey / NKeyKey) are set. Zero means an anonymous
// connection; exactly one selects an auth mode; more than one is an invalid
// auth spec. A nil ref counts as zero.
//
// The W4 reconciler uses this to decide whether the referenced Secret object
// needs fetching at all: only the exactly-one case reads secret.Data.
func authKeyCount(ref *v1alpha1.NATSAuthSecret) int {
	if ref == nil {
		return 0
	}

	n := 0
	if ref.CredentialsKey != "" {
		n++
	}
	if ref.TokenKey != "" {
		n++
	}
	if ref.NKeyKey != "" {
		n++
	}

	return n
}

// buildAuthOption resolves the single auth nats.Option implied by ref and the
// referenced Secret. It returns:
//   - (nil, nil, nil)        when ref names no auth key (anonymous).
//   - (opt, nil, nil)        for the .creds and token modes.
//   - (opt, kp, nil)         for the nkey mode — kp must outlive the connection.
//   - (nil, nil, err)        on any failure; err is classified per connectNATS.
//
// More than one auth key set → errAuthSpecInvalid. A named key absent from
// secret.Data → errSecretKeyMissing. A malformed .creds / nkey seed, or a
// non-user nkey seed → a plain (unclassified) error.
func buildAuthOption(ref *v1alpha1.NATSAuthSecret, secret *corev1.Secret) (nats.Option, nkeys.KeyPair, error) {
	// Pre-connect check: at most one auth key may be set.
	switch n := authKeyCount(ref); {
	case n > 1:
		return nil, nil, fmt.Errorf("secret %q: %w", ref.Name, errAuthSpecInvalid)
	case n == 0:
		// CredentialsSecret is set but names no auth key: anonymous.
		return nil, nil, nil
	}

	// lookup reads a named key from secret.Data, classifying a missing key
	// (or a nil Secret) as errSecretKeyMissing.
	lookup := func(key string) ([]byte, error) {
		if secret == nil {
			return nil, fmt.Errorf("secret %q not provided, cannot read key %q: %w",
				ref.Name, key, errSecretKeyMissing)
		}
		val, ok := secret.Data[key]
		if !ok {
			return nil, fmt.Errorf("secret %q: key %q: %w", ref.Name, key, errSecretKeyMissing)
		}

		return val, nil
	}

	switch {
	case ref.CredentialsKey != "":
		credsBytes, err := lookup(ref.CredentialsKey)
		if err != nil {
			return nil, nil, err
		}

		return nats.UserCredentialBytes(credsBytes), nil, nil

	case ref.TokenKey != "":
		tokenBytes, err := lookup(ref.TokenKey)
		if err != nil {
			return nil, nil, err
		}

		return nats.Token(string(tokenBytes)), nil, nil

	default: // ref.NKeyKey != ""
		seedBytes, err := lookup(ref.NKeyKey)
		if err != nil {
			return nil, nil, err
		}

		return buildNkeyOption(seedBytes)
	}
}

// buildNkeyOption parses an nkey user seed entirely in memory and returns a
// nats.Nkey option plus the parsed keypair. The keypair must be held by the
// caller for the connection's whole lifetime: nats.Nkey's signature callback
// closes over it and re-signs every reconnect-time nonce.
//
// A malformed seed fails at nkeys.FromSeed; a non-user seed (operator /
// account / server) fails the IsValidPublicUserKey check — both before any
// connect attempt, both as plain (unclassified) errors so W4 routes them to
// NATSUnreachable. The file-based nats.NkeyOptionFromSeed performs this
// U-prefix check internally; nats.Nkey does not, so this path replicates it.
func buildNkeyOption(seed []byte) (nats.Option, nkeys.KeyPair, error) {
	kp, err := nkeys.FromSeed(seed)
	if err != nil {
		return nil, nil, fmt.Errorf("parse nkey seed: %w", err)
	}

	pubKey, err := kp.PublicKey()
	if err != nil {
		kp.Wipe()
		return nil, nil, fmt.Errorf("derive nkey public key: %w", err)
	}
	if !nkeys.IsValidPublicUserKey(pubKey) {
		kp.Wipe()
		return nil, nil, fmt.Errorf("nkey seed is not a user seed (public key %q is not a valid user key)", pubKey)
	}

	// The signature callback closes over kp; kp stays live until natsConn.close.
	sigCB := func(nonce []byte) ([]byte, error) {
		return kp.Sign(nonce)
	}

	return nats.Nkey(pubKey, sigCB), kp, nil
}
