package worker

import (
	"errors"
	"net"
	"sync"

	"github.com/nats-io/nats.go"
)

// NetworkControl manages simulated network connectivity for a worker.
type NetworkControl struct {
	mu        sync.Mutex
	connected bool
	nc        *nats.Conn
}

// NewNetworkControl creates a new network control.
func NewNetworkControl() *NetworkControl {
	return &NetworkControl{
		connected: true,
	}
}

// SetConnection sets the NATS connection to control.
func (n *NetworkControl) SetConnection(nc *nats.Conn) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nc = nc
}

// Dial implements nats.CustomDialer.
func (n *NetworkControl) Dial(network, address string) (net.Conn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.connected {
		return nil, errors.New("simulated network disconnect")
	}
	return net.Dial(network, address) //nolint:noctx // Interface does not support context
}

// Disconnect simulates a network disconnect.
func (n *NetworkControl) Disconnect() {
	n.mu.Lock()
	n.connected = false
	conn := n.nc
	n.mu.Unlock()

	if conn != nil {
		// Close the connection to force immediate disconnect.
		// NATS client will try to reconnect, but Dial will fail.
		conn.Close()
	}
}

// Reconnect restores network connectivity.
func (n *NetworkControl) Reconnect() {
	n.mu.Lock()
	n.connected = true
	n.mu.Unlock()
	// NATS client will eventually reconnect on its own.
}
