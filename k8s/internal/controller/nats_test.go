package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/arloliu/parti/v2/k8s/api/v1alpha1"
)

// startNATS starts an embedded NATS server with JetStream from the supplied
// options and registers shutdown cleanup. Host / Port / JetStream / StoreDir /
// NoLog are filled in by the helper; callers set only the auth-relevant
// fields. It returns the client URL.
func startNATS(t *testing.T, opts *natsserver.Options) string {
	t.Helper()

	opts.Host = "127.0.0.1"
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	opts.NoLog = true

	ns, err := natsserver.NewServer(opts)
	require.NoError(t, err, "create embedded NATS server")

	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded NATS server not ready within timeout")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})

	return ns.ClientURL()
}

// startAnonymousNATS starts an embedded NATS server with no authentication.
func startAnonymousNATS(t *testing.T) string {
	t.Helper()

	return startNATS(t, &natsserver.Options{})
}

// nkeyCreds builds a fresh nkey user: the server NkeyUser entry and the seed
// bytes a Secret would carry.
func nkeyCreds(t *testing.T) (nkeyUser *natsserver.NkeyUser, seed []byte) {
	t.Helper()

	kp, err := nkeys.CreateUser()
	require.NoError(t, err)
	pub, err := kp.PublicKey()
	require.NoError(t, err)
	seed, err = kp.Seed()
	require.NoError(t, err)

	return &natsserver.NkeyUser{Nkey: pub}, seed
}

// credsAuthServer starts an embedded NATS server in decentralized (operator)
// JWT mode and returns the client URL plus a working .creds blob for a user
// in a JetStream-enabled account. This is the only auth mode that needs the
// full operator → account → user chain; token and nkey use plain
// server.Options.
func credsAuthServer(t *testing.T) (url string, creds []byte) {
	t.Helper()

	opKP, err := nkeys.CreateOperator()
	require.NoError(t, err)
	opPub, err := opKP.PublicKey()
	require.NoError(t, err)

	// System account.
	sysKP, err := nkeys.CreateAccount()
	require.NoError(t, err)
	sysPub, err := sysKP.PublicKey()
	require.NoError(t, err)
	sysClaims := jwt.NewAccountClaims(sysPub)
	sysClaims.Name = "SYS"
	sysJWT, err := sysClaims.Encode(opKP)
	require.NoError(t, err)

	// User account with JetStream enabled.
	accKP, err := nkeys.CreateAccount()
	require.NoError(t, err)
	accPub, err := accKP.PublicKey()
	require.NoError(t, err)
	accClaims := jwt.NewAccountClaims(accPub)
	accClaims.Name = "TEST"
	accClaims.Limits.DiskStorage = -1
	accClaims.Limits.MemoryStorage = -1
	accClaims.Limits.Streams = -1
	accClaims.Limits.Consumer = -1
	accJWT, err := accClaims.Encode(opKP)
	require.NoError(t, err)

	userKP, err := nkeys.CreateUser()
	require.NoError(t, err)
	userPub, err := userKP.PublicKey()
	require.NoError(t, err)
	userSeed, err := userKP.Seed()
	require.NoError(t, err)
	uc := jwt.NewUserClaims(userPub)
	uc.Name = "test-user"
	userJWT, err := uc.Encode(accKP)
	require.NoError(t, err)
	creds, err = jwt.FormatUserConfig(userJWT, userSeed)
	require.NoError(t, err)

	res := &natsserver.MemAccResolver{}
	require.NoError(t, res.Store(sysPub, sysJWT))
	require.NoError(t, res.Store(accPub, accJWT))

	url = startNATS(t, &natsserver.Options{
		TrustedKeys:     []string{opPub},
		AccountResolver: res,
		SystemAccount:   sysPub,
	})

	return url, creds
}

func TestConnectNATS_Anonymous(t *testing.T) {
	url := startAnonymousNATS(t)

	conn := v1alpha1.NATSConnection{Server: url}
	nc, err := connectNATS(t.Context(), conn, nil)
	require.NoError(t, err)
	defer nc.close()

	require.NotNil(t, nc.nc)
	require.NotNil(t, nc.js)
	require.Nil(t, nc.kp, "anonymous path holds no key pair")
	require.True(t, nc.nc.IsConnected())
}

func TestConnectNATS_CredentialsBytes(t *testing.T) {
	url, creds := credsAuthServer(t)

	conn := v1alpha1.NATSConnection{
		Server: url,
		CredentialsSecret: &v1alpha1.NATSAuthSecret{
			Name:           "nats-auth",
			CredentialsKey: "creds",
		},
	}
	secret := &corev1.Secret{Data: map[string][]byte{"creds": creds}}

	nc, err := connectNATS(t.Context(), conn, secret)
	require.NoError(t, err)
	defer nc.close()

	require.True(t, nc.nc.IsConnected())
	require.NotNil(t, nc.js)
	require.Nil(t, nc.kp, ".creds path holds no key pair")
}

func TestConnectNATS_Token(t *testing.T) {
	const token = "s3cr3t-token"
	url := startNATS(t, &natsserver.Options{Authorization: token})

	conn := v1alpha1.NATSConnection{
		Server: url,
		CredentialsSecret: &v1alpha1.NATSAuthSecret{
			Name:     "nats-auth",
			TokenKey: "token",
		},
	}
	secret := &corev1.Secret{Data: map[string][]byte{"token": []byte(token)}}

	nc, err := connectNATS(t.Context(), conn, secret)
	require.NoError(t, err)
	defer nc.close()

	require.True(t, nc.nc.IsConnected())
	require.NotNil(t, nc.js)
}

func TestConnectNATS_NKey(t *testing.T) {
	nkeyUser, seed := nkeyCreds(t)
	url := startNATS(t, &natsserver.Options{Nkeys: []*natsserver.NkeyUser{nkeyUser}})

	conn := v1alpha1.NATSConnection{
		Server: url,
		CredentialsSecret: &v1alpha1.NATSAuthSecret{
			Name:    "nats-auth",
			NKeyKey: "nkey",
		},
	}
	secret := &corev1.Secret{Data: map[string][]byte{"nkey": seed}}

	nc, err := connectNATS(t.Context(), conn, secret)
	require.NoError(t, err)
	defer nc.close()

	require.True(t, nc.nc.IsConnected())
	require.NotNil(t, nc.js)
	require.NotNil(t, nc.kp, "nkey path holds the key pair for the connection lifetime")
}

// TestConnectNATS_NKeyKeyLifetime guards the in-memory-keypair fix: the
// keypair held by natsConn must stay live for the connection's whole
// lifetime so reconnect-time nonce signing keeps working, and must be wiped
// on close().
func TestConnectNATS_NKeyKeyLifetime(t *testing.T) {
	nkeyUser, seed := nkeyCreds(t)
	url := startNATS(t, &natsserver.Options{Nkeys: []*natsserver.NkeyUser{nkeyUser}})

	conn := v1alpha1.NATSConnection{
		Server: url,
		CredentialsSecret: &v1alpha1.NATSAuthSecret{
			Name:    "nats-auth",
			NKeyKey: "nkey",
		},
	}
	secret := &corev1.Secret{Data: map[string][]byte{"nkey": seed}}

	nc, err := connectNATS(t.Context(), conn, secret)
	require.NoError(t, err)
	require.NotNil(t, nc.kp)

	// The reconnect sign path is live post-Connect: a synthetic nonce signs.
	nonce := []byte("synthetic-reconnect-nonce")
	sig, err := nc.kp.Sign(nonce)
	require.NoError(t, err, "held key pair must still sign after nats.Connect returns")
	require.NotEmpty(t, sig)

	// After close(), the same key pair is wiped and refuses to sign.
	nc.close()
	_, err = nc.kp.Sign(nonce)
	require.Error(t, err, "key pair must refuse to sign after close() wipes it")
}

func TestConnectNATS_MultipleAuthKeys(t *testing.T) {
	// Spec error — caught pre-connect; the server is never contacted, so a
	// bogus URL is harmless.
	conn := v1alpha1.NATSConnection{
		Server: "nats://unused.invalid:4222",
		CredentialsSecret: &v1alpha1.NATSAuthSecret{
			Name:           "nats-auth",
			CredentialsKey: "creds",
			TokenKey:       "token",
		},
	}
	secret := &corev1.Secret{Data: map[string][]byte{
		"creds": []byte("x"),
		"token": []byte("y"),
	}}

	nc, err := connectNATS(t.Context(), conn, secret)
	require.Nil(t, nc)
	require.Error(t, err)
	require.ErrorIs(t, err, errAuthSpecInvalid)
	require.NotErrorIs(t, err, errSecretKeyMissing)
}

func TestConnectNATS_SecretKeyMissing(t *testing.T) {
	conn := v1alpha1.NATSConnection{
		Server: "nats://unused.invalid:4222",
		CredentialsSecret: &v1alpha1.NATSAuthSecret{
			Name:           "nats-auth",
			CredentialsKey: "creds",
		},
	}
	// Secret present but the named key is absent.
	secret := &corev1.Secret{Data: map[string][]byte{"other": []byte("x")}}

	nc, err := connectNATS(t.Context(), conn, secret)
	require.Nil(t, nc)
	require.Error(t, err)
	require.ErrorIs(t, err, errSecretKeyMissing)
	require.NotErrorIs(t, err, errAuthSpecInvalid)
}

func TestConnectNATS_NilSecretWithAuthKey(t *testing.T) {
	conn := v1alpha1.NATSConnection{
		Server: "nats://unused.invalid:4222",
		CredentialsSecret: &v1alpha1.NATSAuthSecret{
			Name:     "nats-auth",
			TokenKey: "token",
		},
	}

	// CredentialsSecret names a key but no Secret object was supplied.
	nc, err := connectNATS(t.Context(), conn, nil)
	require.Nil(t, nc)
	require.Error(t, err)
	require.ErrorIs(t, err, errSecretKeyMissing)
}

func TestConnectNATS_MalformedCreds(t *testing.T) {
	url := startAnonymousNATS(t)

	conn := v1alpha1.NATSConnection{
		Server: url,
		CredentialsSecret: &v1alpha1.NATSAuthSecret{
			Name:           "nats-auth",
			CredentialsKey: "creds",
		},
	}
	secret := &corev1.Secret{Data: map[string][]byte{"creds": []byte("not a valid creds file")}}

	nc, err := connectNATS(t.Context(), conn, secret)
	require.Nil(t, nc)
	require.Error(t, err)
	// Malformed creds → a plain error, NEITHER classified sentinel.
	require.NotErrorIs(t, err, errAuthSpecInvalid)
	require.NotErrorIs(t, err, errSecretKeyMissing)
}

func TestConnectNATS_MalformedNKeySeed(t *testing.T) {
	conn := v1alpha1.NATSConnection{
		Server: "nats://unused.invalid:4222",
		CredentialsSecret: &v1alpha1.NATSAuthSecret{
			Name:    "nats-auth",
			NKeyKey: "nkey",
		},
	}
	secret := &corev1.Secret{Data: map[string][]byte{"nkey": []byte("garbage-not-a-seed")}}

	nc, err := connectNATS(t.Context(), conn, secret)
	require.Nil(t, nc)
	require.Error(t, err)
	require.NotErrorIs(t, err, errAuthSpecInvalid)
	require.NotErrorIs(t, err, errSecretKeyMissing)
}

func TestConnectNATS_NonUserNKeySeed(t *testing.T) {
	// An account seed is a valid nkey seed but not a USER seed: it must be
	// rejected by IsValidPublicUserKey, pre-connect, as a plain error.
	accKP, err := nkeys.CreateAccount()
	require.NoError(t, err)
	accSeed, err := accKP.Seed()
	require.NoError(t, err)

	conn := v1alpha1.NATSConnection{
		Server: "nats://unused.invalid:4222",
		CredentialsSecret: &v1alpha1.NATSAuthSecret{
			Name:    "nats-auth",
			NKeyKey: "nkey",
		},
	}
	secret := &corev1.Secret{Data: map[string][]byte{"nkey": accSeed}}

	nc, err := connectNATS(t.Context(), conn, secret)
	require.Nil(t, nc)
	require.Error(t, err)
	require.NotErrorIs(t, err, errAuthSpecInvalid)
	require.NotErrorIs(t, err, errSecretKeyMissing)
}

func TestConnectNATS_ConnectFailure(t *testing.T) {
	// No server listening — nats.Connect fails. A plain error, no sentinel.
	conn := v1alpha1.NATSConnection{Server: "nats://127.0.0.1:1"}

	nc, err := connectNATS(t.Context(), conn, nil)
	require.Nil(t, nc)
	require.Error(t, err)
	require.NotErrorIs(t, err, errAuthSpecInvalid)
	require.NotErrorIs(t, err, errSecretKeyMissing)
}

func TestConnectNATS_NoAuthKeyAnonymous(t *testing.T) {
	// CredentialsSecret is set but names no auth key → anonymous connection.
	url := startAnonymousNATS(t)

	conn := v1alpha1.NATSConnection{
		Server: url,
		CredentialsSecret: &v1alpha1.NATSAuthSecret{
			Name: "nats-auth",
		},
	}
	// secret may be nil — no key is read.
	nc, err := connectNATS(t.Context(), conn, nil)
	require.NoError(t, err)
	defer nc.close()

	require.True(t, nc.nc.IsConnected())
	require.Nil(t, nc.kp)
}

func TestConnectNATS_ContextCancelledAfterConnect(t *testing.T) {
	url := startAnonymousNATS(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // dead ctx before connectNATS runs

	conn := v1alpha1.NATSConnection{Server: url}
	nc, err := connectNATS(ctx, conn, nil)
	require.Nil(t, nc)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
}
