package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

type ConnectionTracker struct {
	mu          sync.Mutex
	activeConns map[string]net.Conn
}

func NewConnectionTracker() *ConnectionTracker {
	return &ConnectionTracker{
		activeConns: make(map[string]net.Conn),
	}
}

func (ct *ConnectionTracker) Add(addr string, conn net.Conn) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.activeConns[addr] = conn
}

func (ct *ConnectionTracker) Remove(addr string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	delete(ct.activeConns, addr)
}

func (ct *ConnectionTracker) List() []string {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	var conns []string
	for addr := range ct.activeConns {
		conns = append(conns, addr)
	}
	return conns
}

func main() {
	// Tracker for connections
	tracker := NewConnectionTracker()

	// Custom DialContext to track connections
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err == nil {
				// Track new connection
				tracker.Add(addr, conn)
			}
			return conn, err
		},
	}

	client := &http.Client{Transport: transport}

	// Make a few requests
	_, _ = client.Get("http://example.com")
	_, _ = client.Get("http://example.org")

	// List active connections
	fmt.Println("Active connections:")
	for _, conn := range tracker.List() {
		fmt.Println(conn)
	}

	// Close idle connections
	transport.CloseIdleConnections()

	// Wait to ensure connections are closed
	time.Sleep(2 * time.Second)

	fmt.Println("Connections after CloseIdleConnections:")
	for _, conn := range tracker.List() {
		fmt.Println(conn)
	}
}
