package worker

import (
	"context"
	"errors"
	"net"
	"sync"
)

// NetworkControl manages simulated network connectivity for a worker.
type NetworkControl struct {
	mu          sync.Mutex
	connected   bool
	activeConns map[*wrappedConn]struct{}
}

// NewNetworkControl creates a new network control.
func NewNetworkControl() *NetworkControl {
	return &NetworkControl{
		connected:   true,
		activeConns: make(map[*wrappedConn]struct{}),
	}
}

// SetConnection sets the NATS connection to control.
//
// Deprecated: No longer needed as we control net.Conn directly.
func (n *NetworkControl) SetConnection(_ any) {
	// No-op
}

// Dial implements nats.CustomDialer.
func (n *NetworkControl) Dial(network, address string) (net.Conn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.connected {
		return nil, errors.New("simulated network disconnect")
	}
	var d net.Dialer
	c, err := d.DialContext(context.Background(), network, address)
	if err != nil {
		return nil, err
	}

	wc := &wrappedConn{Conn: c, ctrl: n}
	n.activeConns[wc] = struct{}{}

	return wc, nil
}

// Disconnect simulates a network disconnect.
func (n *NetworkControl) Disconnect() {
	n.mu.Lock()
	n.connected = false
	// Copy to avoid concurrent map modification when Close() calls removeConn
	conns := make([]*wrappedConn, 0, len(n.activeConns))
	for c := range n.activeConns {
		conns = append(conns, c)
	}
	n.mu.Unlock()

	for _, c := range conns {
		// Close the underlying connection to force read/write errors
		_ = c.Conn.Close()
	}
}

// Reconnect restores network connectivity.
func (n *NetworkControl) Reconnect() {
	n.mu.Lock()
	n.connected = true
	n.mu.Unlock()
	// NATS client will eventually reconnect on its own.
}

func (n *NetworkControl) removeConn(c *wrappedConn) {
	n.mu.Lock()
	delete(n.activeConns, c)
	n.mu.Unlock()
}

type wrappedConn struct {
	net.Conn
	ctrl *NetworkControl
}

func (w *wrappedConn) Close() error {
	w.ctrl.removeConn(w)
	return w.Conn.Close()
}
