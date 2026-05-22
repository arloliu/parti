package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// commonFlags holds the NATS connection flags shared by all commands.
type commonFlags struct {
	server  string
	creds   string
	nkey    string
	token   string
	timeout string // parsed separately; stored as flag string
}

// bindCommonFlags registers the NATS connection flags (server, creds, nkey,
// token, timeout) on fs, writing into cf. Every partictl command accepts this
// identical flag block.
func bindCommonFlags(fs *flag.FlagSet, cf *commonFlags) {
	fs.StringVar(&cf.server, "server", defaultServer(), "NATS server URL")
	fs.StringVar(&cf.creds, "creds", "", "path to NATS credentials file")
	fs.StringVar(&cf.nkey, "nkey", "", "path to NATS nkey seed file")
	fs.StringVar(&cf.token, "token", "", "NATS token")
	fs.StringVar(&cf.timeout, "timeout", "30s", "operation timeout (e.g. 30s, 1m)")
}

// natsConn bundles a connected NATS client with its JetStream context.
// Callers must call nc.close() when done.
//
// ctx and stop are populated only by connectWithTimeout: ctx is the
// signal-cancellable operation context and stop releases it. close() invokes
// stop when present, so a single deferred close() unwinds the whole bundle.
type natsConn struct {
	nc   *nats.Conn
	js   jetstream.JetStream
	ctx  context.Context
	stop context.CancelFunc
}

// close closes the underlying NATS connection and, when set, stops the
// operation context.
func (n *natsConn) close() {
	n.nc.Close()
	if n.stop != nil {
		n.stop()
	}
}

// defaultServer returns the NATS server URL to use when -server is not set.
// Priority: $NATS_URL env > nats.DefaultURL.
func defaultServer() string {
	if v := os.Getenv("NATS_URL"); v != "" {
		return v
	}

	return nats.DefaultURL
}

// connectNATS opens a NATS connection using the options from commonFlags.
// On failure it returns ExitNATS (4) as the exit code, along with the error.
// On success callers must call conn.close() when done.
func connectNATS(ctx context.Context, cf commonFlags) (*natsConn, int, error) {
	opts := []nats.Option{
		nats.Name("partictl"),
	}

	if cf.creds != "" {
		opts = append(opts, nats.UserCredentials(cf.creds))
	}
	if cf.nkey != "" {
		nkeyOpt, err := nats.NkeyOptionFromSeed(cf.nkey)
		if err != nil {
			return nil, ExitNATS, fmt.Errorf("partictl: load nkey %q: %w", cf.nkey, err)
		}
		opts = append(opts, nkeyOpt)
	}
	if cf.token != "" {
		opts = append(opts, nats.Token(cf.token))
	}

	nc, err := nats.Connect(cf.server, opts...)
	if err != nil {
		return nil, ExitNATS, fmt.Errorf("partictl: connect to %s: %w", cf.server, err)
	}

	// Honour ctx cancellation. nats.Connect does not accept a context; check after.
	if ctx.Err() != nil {
		nc.Close()
		return nil, ExitNATS, ctx.Err()
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, ExitNATS, fmt.Errorf("partictl: create jetstream: %w", err)
	}

	return &natsConn{nc: nc, js: js}, ExitOK, nil
}

// connectWithTimeout builds the signal-cancellable timeout context and opens a
// NATS connection, classifying any failure into an exit code. Callers parse
// -timeout separately (so an invalid value can fail a static path) and pass the
// resulting duration in.
//
// On success it returns the open connection — with conn.ctx and conn.stop
// populated — and ExitOK. The caller must defer conn.close(), which closes the
// connection and stops the context.
//
// On failure it writes the error to stderr, releases the context, and returns a
// nil connection plus a non-OK exit code (ExitNATS for a timeout/signal,
// otherwise the code from connectNATS).
func connectWithTimeout(d time.Duration, cf commonFlags, stderr io.Writer) (*natsConn, int) {
	ctx, stop := makeContext(d)

	conn, code, err := connectNATS(ctx, cf)
	if err != nil {
		fmt.Fprintln(stderr, err)
		stop()
		if ctxDead(ctx) {
			return nil, ExitNATS
		}

		return nil, code
	}

	conn.ctx = ctx
	conn.stop = stop

	return conn, ExitOK
}
