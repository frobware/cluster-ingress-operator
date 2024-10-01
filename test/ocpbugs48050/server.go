package ocpbugs40850

// https://issues.redhat.com/browse/OCPBUGS-40850.

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"os"
	"time"
)

// handleConnection processes an incoming network connection and sends
// an HTTP response based on the request's path. It either includes or
// excludes duplicate "Transfer-Encoding" headers, depending on the
// path. The function manually writes headers and chunked body data
// using bufio.Writer, rather than using http.ResponseWriter.
//
// We do not use http.ResponseWriter because the Go HTTP stack
// automatically sanitises headers, which prevents the inclusion of
// duplicate "Transfer-Encoding" headers. For testing purposes, where
// we need to simulate multiple "Transfer-Encoding" headers, this
// manual approach provides full control over the headers in the
// response.
func handleConnection(conn net.Conn) {
	defer conn.Close()

	log.Printf("%v: Connection from %s\n", conn.LocalAddr(), conn.RemoteAddr())

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	// Read the request line (e.g., "GET /duplicate-te HTTP/1.1").
	requestLine, err := reader.ReadString('\n')
	if err != nil {
		log.Printf("%v: Error reading request line: %v\n", conn.LocalAddr(), err)
		return
	}

	log.Printf("%v: Received request: %s", conn.LocalAddr(), requestLine)

	// Split the request line to get the method, path, and protocol.
	var method, path, protocol string

	if _, err = fmt.Sscanf(requestLine, "%s %s %s", &method, &path, &protocol); err != nil {
		log.Printf("%v: Error parsing request line: %v\n", conn.LocalAddr(), err)
		return
	}

	switch path {
	case "/healthz":
		healthResponse := "HTTP/1.1 200 OK\r\n" +
			"Content-Type: text/plain\r\n" +
			"Connection: close\r\n\r\n" +
			"OK\r\n"
		if _, err := writer.WriteString(healthResponse); err != nil {
			log.Printf("%v: Error writing /healthz response: %v\n", conn.LocalAddr(), err)
			return
		}

	case "/single-te":
		if err := handleSingleTE(writer); err != nil {
			log.Printf("%v: Error handling /single-te: %v\n", conn.LocalAddr(), err)
			return
		}

	case "/duplicate-te":
		if err := handleDuplicateTE(writer); err != nil {
			log.Printf("%v: Error handling /duplicate-te: %v\n", conn.LocalAddr(), err)
			return
		}

	default:
		notFoundResponse := "HTTP/1.1 404 Not Found\r\n" +
			"Content-Type: text/plain\r\n" +
			"Connection: close\r\n\r\n" +
			"404 page not found\r\n"
		if _, err := writer.WriteString(notFoundResponse); err != nil {
			log.Printf("%v: Error writing 404 response: %v\n", conn.LocalAddr(), err)
			return
		}
	}

	if err := writer.Flush(); err != nil {
		log.Printf("%v: Error flushing data to connection: %v\n", conn.LocalAddr(), err)
	}
}

// writeChunk writes a single chunk to the provided writer and flushes the writer.
// It returns an error with context if writing or flushing fails.
func writeChunk(writer *bufio.Writer, chunk string, chunkNumber int) error {
	if _, err := writer.WriteString(chunk); err != nil {
		return fmt.Errorf("error writing chunk %d (%q): %v", chunkNumber, chunk, err)
	}

	if err := writer.Flush(); err != nil {
		return fmt.Errorf("error flushing writer after chunk %d (%q): %v", chunkNumber, chunk, err)
	}

	return nil
}

// writeChunks writes multiple chunks to the provided writer. It
// returns an error with context if any chunk fails to write.
func writeChunks(writer *bufio.Writer, chunks []string) error {
	for i, chunk := range chunks {
		if err := writeChunk(writer, chunk, i+1); err != nil {
			return fmt.Errorf("error writing chunks: %w", err)
		}
	}
	return nil
}

func handleSingleTE(writer *bufio.Writer) error {
	response := fmt.Sprintf("HTTP/1.1 200 OK\r\n"+
		"Date: %s\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n"+
		"Connection: close\r\n"+
		"Transfer-Encoding: chunked\r\n\r\n", time.Now().UTC().Format(time.RFC1123))
	if _, err := writer.WriteString(response); err != nil {
		return fmt.Errorf("error writing single-te response headers: %v", err)
	}

	// Form the chunked response
	return writeChunks(writer, []string{
		// First chunk:
		// - 'A' is the hexadecimal representation of 10 (the length of "single-te\n").
		// - \r\n separates the chunk size from the chunk data.
		// - "single-te\n" is the chunk data (the \n is included in the chunk data).
		// - \r\n terminates the chunk.
		"A\r\nsingle-te\n\r\n",

		// Final chunk:
		// - '0' indicates a chunk of zero length, signaling the end of the chunked message.
		// - The first \r\n separates the chunk size (0) from what would be the chunk data.
		// - The second \r\n terminates the zero-length chunk and the entire chunked message.
		"0\r\n\r\n",
	})
}

func handleDuplicateTE(writer *bufio.Writer) error {
	response := fmt.Sprintf("HTTP/1.1 200 OK\r\n"+
		"Date: %s\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n"+
		"Connection: close\r\n"+
		"Transfer-Encoding: chunked\r\n"+
		"Transfer-Encoding: chunked\r\n\r\n", time.Now().UTC().Format(time.RFC1123))
	if _, err := writer.WriteString(response); err != nil {
		return fmt.Errorf("error writing duplicate-te response headers: %v", err)
	}

	return writeChunks(writer, []string{
		// First chunk:
		// - 'D' is the hexadecimal representation of 13 (the length of "duplicate-te\n").
		// - \r\n separates the chunk size from the chunk data.
		// - "duplicate-te\n" is the chunk data (note the \n is part of the data).
		// - \r\n terminates the chunk.
		"D\r\nduplicate-te\n\r\n",

		// Final chunk:
		// - '0' indicates a chunk of zero length, signaling the end of the chunked message.
		// - The first \r\n separates the chunk size (0) from what would be the chunk data.
		// - The second \r\n terminates the zero-length chunk and the entire chunked message.
		"0\r\n\r\n",
	})
}

func createListener(port string, useTLS bool) (net.Listener, error) {
	if useTLS {
		cert, err := tls.LoadX509KeyPair("/etc/serving-cert/tls.crt", "/etc/serving-cert/tls.key")
		if err != nil {
			return nil, fmt.Errorf("error loading TLS certificate: %w", err)
		}

		config := &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
		return tls.Listen("tcp", ":"+port, config)
	}

	return net.Listen("tcp", ":"+port)
}

func startServer(port string, useTLS bool) {
	listener, err := createListener(port, useTLS)
	if err != nil {
		log.Fatalf("Error starting server on port %s: %v\n", port, err)
	}

	defer listener.Close()

	fmt.Printf("Server is listening on port %v (useTLS=%v)\n", port, useTLS)

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Error accepting connection:", err)
			continue
		}
		go handleConnection(conn)
	}
}

func Serve() {
	httpPort := os.Getenv("HTTP_PORT")
	httpsPort := os.Getenv("HTTPS_PORT")

	if httpPort == "" || httpsPort == "" {
		log.Fatalf("Environment variables HTTP_PORT and HTTPS_PORT must be set\n")
	}

	// Start non-TLS server handling all paths, including /healthz.
	go startServer(httpPort, false)

	// Start TLS server handling all paths, including /healthz.
	go startServer(httpsPort, true)

	// Block forever (needed to keep the servers running).
	select {}
}
