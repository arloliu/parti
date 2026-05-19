package main

import (
	"context"
	"fmt"
	"os"

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

// natsConn bundles a connected NATS client with its JetStream context.
// Callers must call nc.Close() when done.
type natsConn struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// close closes the underlying NATS connection.
func (n *natsConn) close() {
	n.nc.Close()
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
