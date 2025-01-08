package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
)

// idleConnHTTPClient represents a minimal HTTP client with explicit
// connection management.
type idleConnHTTPClient struct {
	conn   net.Conn
	addr   string
	reader *bufio.Reader
}

// NewCustomClient creates a new CustomClient for the specified
// address.
func NewCustomClient(addr string) (*idleConnHTTPClient, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}
	return &idleConnHTTPClient{
		conn:   conn,
		addr:   addr,
		reader: bufio.NewReader(conn),
	}, nil
}

// SendRequest sends an HTTP GET request to the specified path with a custom Host header.
func (c *idleConnHTTPClient) SendRequest(path, host string) error {
	if c.conn == nil {
		return fmt.Errorf("connection is not established")
	}

	// Manually construct the GET request with the specified Host
	// header, and keep-alive's enabled.
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", path, host)

	_, err := c.conn.Write([]byte(request))
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	return nil
}

// ReadResponse parses the HTTP response using http.ReadResponse.
func (c *idleConnHTTPClient) ReadResponse() (*http.Response, error) {
	if c.reader == nil {
		return nil, fmt.Errorf("no connection reader available")
	}

	// Parse the response using the net/http utility.
	resp, err := http.ReadResponse(c.reader, nil)
	if err != nil {
		return nil, fmt.Errorf("error parsing response: %w", err)
	}
	return resp, nil
}

// Get is a convenience method that sends a GET request with a custom Host header and returns the response.
func (c *idleConnHTTPClient) Get(path, host string) (*http.Response, error) {
	if err := c.SendRequest(path, host); err != nil {
		return nil, fmt.Errorf("error sending GET request: %w", err)
	}
	return c.ReadResponse()
}

// Close closes the connection to the server.
func (c *idleConnHTTPClient) Close() error {
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

func (c *idleConnHTTPClient) IsClosed() bool {
	return c.conn == nil
}
