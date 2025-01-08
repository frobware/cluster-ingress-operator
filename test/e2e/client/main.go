package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	// Address of the server (host:port).
	addr := os.Args[2]

	// Path to request and custom Host header value.
	path := "/"
	host := os.Args[1]

	// Create the custom client.
	client, err := NewCustomClient(addr)
	if err != nil {
		fmt.Printf("Error creating client: %v\n", err)
		return
	}
	defer client.Close()

	// Use the Get method to send the request with a custom Host
	// header and get the response.
	resp, err := client.Get(path, host)
	if err != nil {
		fmt.Printf("Error making GET request: %v\n", err)
		return
	}
	defer resp.Body.Close()

	// Print the status line.
	fmt.Printf("Status: %s\n", resp.Status)

	// Print headers.
	fmt.Println("Headers:")
	for key, values := range resp.Header {
		for _, value := range values {
			fmt.Printf("%s: %s\n", key, value)
		}
	}

	// Read and print the body.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading response body: %v\n", err)
		return
	}
	fmt.Println("\nBody:")
	fmt.Printf("%s\n", string(body))
}
