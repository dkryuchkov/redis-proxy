package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// readFullCommand reads a full Redis command from the reader.
func readFullCommand(reader *bufio.Reader) (string, error) {
	var command strings.Builder

	// Read the first line to determine if it's an array
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	command.WriteString(line)

	if strings.HasPrefix(line, "*") {
		// Parse the array length
		var arrayLen int
		fmt.Sscanf(line, "*%d", &arrayLen)

		for i := 0; i < arrayLen; i++ {
			// Read the bulk string length
			lenLine, err := reader.ReadString('\n')
			if err != nil {
				return "", err
			}
			command.WriteString(lenLine)

			// Read the actual data
			length := 0
			fmt.Sscanf(lenLine, "$%d", &length)
			data := make([]byte, length+2) // +2 for \r\n
			_, err = reader.Read(data)
			if err != nil {
				return "", err
			}
			command.Write(data)
		}
	}
	return command.String(), nil
}

func TestHandleConnection_AuthAndPing(t *testing.T) {
	// Initialize client credentials
	initialCreds := map[string]map[string]interface{}{
		"newpassword123": {
			"dbIndex": "0",
		},
	}
	clientCreds.Store(initialCreds)

	// Create a listener for the mock Redis server
	redisLn, err := net.Listen("tcp", "127.0.0.1:0") // Automatically pick an available port
	if err != nil {
		t.Fatalf("Failed to create Redis listener: %v", err)
	}
	defer redisLn.Close()

	// Extract the actual address of the Redis listener
	redisAddress := redisLn.Addr().String()

	// Channel to synchronize the Redis server handling
	redisDone := make(chan struct{})

	// Mock Redis server
	go func() {
		conn, err := redisLn.Accept()
		if err != nil {
			t.Fatalf("Failed to accept Redis connection: %v", err)
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)

		// Handle AUTH command
		authCmd, _ := readFullCommand(reader)
		if strings.Contains(authCmd, "AUTH") {
			_, _ = writer.WriteString("+OK\r\n")
			writer.Flush()
		}

		// Handle SELECT command
		selectCmd, _ := readFullCommand(reader)
		if strings.Contains(selectCmd, "SELECT") {
			_, _ = writer.WriteString("+OK\r\n")
			writer.Flush()
		}

		// Handle PING command
		pingCmd, _ := readFullCommand(reader)
		if strings.Contains(pingCmd, "PING") {
			_, _ = writer.WriteString("+PONG\r\n")
			writer.Flush()
		}

		redisDone <- struct{}{}
	}()

	// Create a listener for the client connection
	clientLn, err := net.Listen("tcp", "127.0.0.1:0") // Automatically pick an available port
	if err != nil {
		t.Fatalf("Failed to create client listener: %v", err)
	}
	defer clientLn.Close()

	// Extract the actual address of the client listener
	clientAddress := clientLn.Addr().String()

	// Channel to synchronize the client handling
	clientDone := make(chan struct{})

	// Mock client connection
	go func() {
		clientConn, err := clientLn.Accept()
		if err != nil {
			t.Fatalf("Failed to accept client connection: %v", err)
		}
		defer clientConn.Close()

		// Define a dial function to connect to the mock Redis server
		mockDialFunc := func(network, address string) (net.Conn, error) {
			return net.Dial("tcp", redisAddress)
		}

		// Run handleConnection in the client connection
		handleConnection(clientConn, mockDialFunc)
		clientDone <- struct{}{}
	}()

	// Simulate a client connection
	clientConn, err := net.Dial("tcp", clientAddress)
	if err != nil {
		t.Fatalf("Failed to connect to client listener: %v", err)
	}

	// Step 1: Send AUTH command and read response
	clientConn.Write([]byte("*2\r\n$4\r\nAUTH\r\n$14\r\nnewpassword123\r\n"))

	authResponse := make([]byte, 1024)
	n, err := clientConn.Read(authResponse)
	if err != nil {
		t.Fatalf("Error reading AUTH response: %v", err)
	}

	expectedAuthResponse := "+OK\r\n"
	if string(authResponse[:n]) != expectedAuthResponse {
		t.Errorf("Expected AUTH response %q, but got %q", expectedAuthResponse, string(authResponse[:n]))
	}

	// Step 2: Send PING command and read response
	clientConn.Write([]byte("*1\r\n$4\r\nPING\r\n"))

	pingResponse := make([]byte, 1024)
	n, err = clientConn.Read(pingResponse)
	if err != nil {
		t.Fatalf("Error reading PING response: %v", err)
	}

	expectedPingResponse := "+PONG\r\n"
	if string(pingResponse[:n]) != expectedPingResponse {
		t.Errorf("Expected PING response %q, but got %q", expectedPingResponse, string(pingResponse[:n]))
	}

	// Wait for the client and Redis mock servers to finish handling
	clientConn.Close()
	<-clientDone
	<-redisDone
}

func TestHandleConnection_InvalidAuth(t *testing.T) {
	// Initialize client credentials with the correct password
	initialCreds := map[string]map[string]interface{}{
		"newpassword123": {
			"dbIndex": "0",
		},
	}
	clientCreds.Store(initialCreds)

	// Create a listener for the mock Redis server
	redisLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create Redis listener: %v", err)
	}
	defer redisLn.Close()

	redisAddress := redisLn.Addr().String()
	redisDone := make(chan struct{})

	// Mock Redis server to handle commands
	go func() {
		conn, err := redisLn.Accept()
		if err != nil {
			t.Fatalf("Failed to accept Redis connection: %v", err)
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)

		// Handle AUTH command
		authCmd, _ := readFullCommand(reader)
		if strings.Contains(authCmd, "AUTH") {
			_, _ = writer.WriteString("-ERR invalid password\r\n")
			writer.Flush()
		}

		redisDone <- struct{}{}
	}()

	// Create a listener for the client connection
	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create client listener: %v", err)
	}
	defer clientLn.Close()

	clientAddress := clientLn.Addr().String()
	clientDone := make(chan struct{})

	go func() {
		clientConn, err := clientLn.Accept()
		if err != nil {
			t.Fatalf("Failed to accept client connection: %v", err)
		}
		defer clientConn.Close()

		mockDialFunc := func(network, address string) (net.Conn, error) {
			return net.Dial("tcp", redisAddress)
		}

		handleConnection(clientConn, mockDialFunc)
		clientDone <- struct{}{}
	}()

	// Simulate a client connection with an invalid password
	clientConn, err := net.Dial("tcp", clientAddress)
	if err != nil {
		t.Fatalf("Failed to connect to client listener: %v", err)
	}

	clientConn.Write([]byte("*2\r\n$4\r\nAUTH\r\n$15\r\ninvalidpassword\r\n"))

	authResponse := make([]byte, 1024)
	n, err := clientConn.Read(authResponse)
	if err != nil {
		t.Fatalf("Error reading AUTH response: %v", err)
	}

	expectedAuthResponse := "-ERR invalid password\r\n"
	if string(authResponse[:n]) != expectedAuthResponse {
		t.Errorf("Expected AUTH response %q, but got %q", expectedAuthResponse, string(authResponse[:n]))
	}

	clientConn.Close()
	<-clientDone
	<-redisDone
}

func TestHandleConnection_SelectAndSetGetCommands(t *testing.T) {
	// Initialize client credentials
	initialCreds := map[string]map[string]interface{}{
		"newpassword123": {
			"dbIndex": "0",
		},
	}
	clientCreds.Store(initialCreds)

	// Create a listener for the mock Redis server
	redisLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create Redis listener: %v", err)
	}
	defer redisLn.Close()

	redisAddress := redisLn.Addr().String()
	redisDone := make(chan struct{})

	// Mock Redis server to handle commands
	go func() {
		conn, err := redisLn.Accept()
		if err != nil {
			t.Fatalf("Failed to accept Redis connection: %v", err)
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)

		// Handle AUTH command
		authCmd, _ := readFullCommand(reader)
		if strings.Contains(authCmd, "AUTH") {
			_, _ = writer.WriteString("+OK\r\n")
			writer.Flush()
		}

		// Handle SELECT command
		selectCmd, _ := readFullCommand(reader)
		if strings.Contains(selectCmd, "SELECT") {
			_, _ = writer.WriteString("+OK\r\n")
			writer.Flush()
		}

		// Handle SET command
		setCmd, _ := readFullCommand(reader)
		if strings.Contains(setCmd, "SET") {
			_, _ = writer.WriteString("+OK\r\n")
			writer.Flush()
		}

		// Handle GET command
		getCmd, _ := readFullCommand(reader)
		if strings.Contains(getCmd, "GET") {
			_, _ = writer.WriteString("$3\r\nbar\r\n") // Return "bar" for simplicity
			writer.Flush()
		}

		redisDone <- struct{}{}
	}()

	// Create a listener for the client connection
	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create client listener: %v", err)
	}
	defer clientLn.Close()

	clientAddress := clientLn.Addr().String()
	clientDone := make(chan struct{})

	go func() {
		clientConn, err := clientLn.Accept()
		if err != nil {
			t.Fatalf("Failed to accept client connection: %v", err)
		}
		defer clientConn.Close()

		mockDialFunc := func(network, address string) (net.Conn, error) {
			return net.Dial("tcp", redisAddress)
		}

		handleConnection(clientConn, mockDialFunc)
		clientDone <- struct{}{}
	}()

	// Simulate a client connection
	clientConn, err := net.Dial("tcp", clientAddress)
	if err != nil {
		t.Fatalf("Failed to connect to client listener: %v", err)
	}

	// Step 1: Send AUTH command
	clientConn.Write([]byte("*2\r\n$4\r\nAUTH\r\n$14\r\nnewpassword123\r\n"))
	authResponse := make([]byte, 1024)
	n, err := clientConn.Read(authResponse)
	if err != nil {
		t.Fatalf("Error reading AUTH response: %v", err)
	}
	expectedAuthResponse := "+OK\r\n"
	if string(authResponse[:n]) != expectedAuthResponse {
		t.Errorf("Expected AUTH response %q, but got %q", expectedAuthResponse, string(authResponse[:n]))
	}

	// Step 2: Send SELECT command
	clientConn.Write([]byte("*2\r\n$6\r\nSELECT\r\n$1\r\n1\r\n"))
	selectResponse := make([]byte, 1024)
	n, err = clientConn.Read(selectResponse)
	if err != nil {
		t.Fatalf("Error reading SELECT response: %v", err)
	}
	expectedSelectResponse := "+OK\r\n"
	if string(selectResponse[:n]) != expectedSelectResponse {
		t.Errorf("Expected SELECT response %q, but got %q", expectedSelectResponse, string(selectResponse[:n]))
	}

	// Step 3: Send SET command
	clientConn.Write([]byte("*3\r\n$3\r\nSET\r\n$3\r\nfoo\r\n$3\r\nbar\r\n"))
	setResponse := make([]byte, 1024)
	n, err = clientConn.Read(setResponse)
	if err != nil {
		t.Fatalf("Error reading SET response: %v", err)
	}
	expectedSetResponse := "+OK\r\n"
	if string(setResponse[:n]) != expectedSetResponse {
		t.Errorf("Expected SET response %q, but got %q", expectedSetResponse, string(setResponse[:n]))
	}

	// Step 4: Send GET command
	clientConn.Write([]byte("*2\r\n$3\r\nGET\r\n$3\r\nfoo\r\n"))
	getResponse := make([]byte, 1024)
	n, err = clientConn.Read(getResponse)
	if err != nil {
		t.Fatalf("Error reading GET response: %v", err)
	}
	expectedGetResponse := "$3\r\nbar\r\n"
	if string(getResponse[:n]) != expectedGetResponse {
		t.Errorf("Expected GET response %q, but got %q", expectedGetResponse, string(getResponse[:n]))
	}

	clientConn.Close()
	<-clientDone
	<-redisDone
}

func TestHandleConnection_InvalidCommands(t *testing.T) {
	// Initialize client credentials
	initialCreds := map[string]map[string]interface{}{
		"newpassword123": {
			"dbIndex": "0",
		},
	}
	clientCreds.Store(initialCreds)

	// Create a listener for the mock Redis server
	redisLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create Redis listener: %v", err)
	}
	defer redisLn.Close()

	redisAddress := redisLn.Addr().String()
	redisDone := make(chan struct{})

	// Mock Redis server to handle commands
	go func() {
		conn, err := redisLn.Accept()
		if err != nil {
			t.Fatalf("Failed to accept Redis connection: %v", err)
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)

		// Handle AUTH command
		authCmd, _ := readFullCommand(reader)
		if strings.Contains(authCmd, "AUTH") {
			_, _ = writer.WriteString("+OK\r\n")
			writer.Flush()
		}

		// Handle SELECT command
		selectCmd, _ := readFullCommand(reader)
		if strings.Contains(selectCmd, "SELECT") {
			_, _ = writer.WriteString("+OK\r\n")
			writer.Flush()
		}

		// Handle invalid command
		invalidCmd, _ := readFullCommand(reader)
		if strings.Contains(invalidCmd, "INVALID") {
			_, _ = writer.WriteString("-ERR unknown command\r\n")
			writer.Flush()
		}

		redisDone <- struct{}{}
	}()

	// Create a listener for the client connection
	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create client listener: %v", err)
	}
	defer clientLn.Close()

	clientAddress := clientLn.Addr().String()
	clientDone := make(chan struct{})

	go func() {
		clientConn, err := clientLn.Accept()
		if err != nil {
			t.Fatalf("Failed to accept client connection: %v", err)
		}
		defer clientConn.Close()

		mockDialFunc := func(network, address string) (net.Conn, error) {
			return net.Dial("tcp", redisAddress)
		}

		handleConnection(clientConn, mockDialFunc)
		clientDone <- struct{}{}
	}()

	// Simulate a client connection
	clientConn, err := net.Dial("tcp", clientAddress)
	if err != nil {
		t.Fatalf("Failed to connect to client listener: %v", err)
	}

	// Step 1: Send AUTH command
	clientConn.Write([]byte("*2\r\n$4\r\nAUTH\r\n$14\r\nnewpassword123\r\n"))
	authResponse := make([]byte, 1024)
	n, err := clientConn.Read(authResponse)
	if err != nil {
		t.Fatalf("Error reading AUTH response: %v", err)
	}
	expectedAuthResponse := "+OK\r\n"
	if string(authResponse[:n]) != expectedAuthResponse {
		t.Errorf("Expected AUTH response %q, but got %q", expectedAuthResponse, string(authResponse[:n]))
	}

	// Step 2: Send an invalid command
	clientConn.Write([]byte("*1\r\n$7\r\nINVALID\r\n"))
	invalidResponse := make([]byte, 1024)
	n, err = clientConn.Read(invalidResponse)
	if err != nil {
		t.Fatalf("Error reading invalid command response: %v", err)
	}
	expectedInvalidResponse := "-ERR unknown command\r\n"
	if string(invalidResponse[:n]) != expectedInvalidResponse {
		t.Errorf("Expected invalid command response %q, but got %q", expectedInvalidResponse, string(invalidResponse[:n]))
	}

	clientConn.Close()
	<-clientDone
	<-redisDone
}

func TestHandleConnection_AuthTimeout(t *testing.T) {
	// Initialize client credentials
	initialCreds := map[string]map[string]interface{}{
		"newpassword123": {
			"dbIndex": "0",
		},
	}
	clientCreds.Store(initialCreds)

	// Create a listener for the mock Redis server
	redisLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create Redis listener: %v", err)
	}
	defer redisLn.Close()

	redisAddress := redisLn.Addr().String()
	redisDone := make(chan struct{})

	// Mock Redis server to handle commands
	go func() {
		conn, err := redisLn.Accept()
		if err != nil {
			t.Fatalf("Failed to accept Redis connection: %v", err)
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)

		// Simulate a delay before handling the AUTH command
		authCmd, _ := readFullCommand(reader)
		if strings.Contains(authCmd, "AUTH") {
			time.Sleep(6 * time.Second) // Delay longer than the client timeout
			_, _ = writer.WriteString("+OK\r\n")
			writer.Flush()
		}

		// Handle SELECT command
		selectCmd, _ := readFullCommand(reader)
		if strings.Contains(selectCmd, "SELECT") {
			_, _ = writer.WriteString("+OK\r\n")
			writer.Flush()
		}

		redisDone <- struct{}{}
	}()

	// Create a listener for the client connection
	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create client listener: %v", err)
	}
	defer clientLn.Close()

	clientAddress := clientLn.Addr().String()
	clientDone := make(chan struct{})

	go func() {
		clientConn, err := clientLn.Accept()
		if err != nil {
			t.Fatalf("Failed to accept client connection: %v", err)
		}
		defer clientConn.Close()

		mockDialFunc := func(network, address string) (net.Conn, error) {
			return net.Dial("tcp", redisAddress)
		}

		handleConnection(clientConn, mockDialFunc)
		clientDone <- struct{}{}
	}()

	// Simulate a client connection
	clientConn, err := net.Dial("tcp", clientAddress)
	if err != nil {
		t.Fatalf("Failed to connect to client listener: %v", err)
	}

	// Set a timeout for the entire connection
	clientConn.SetDeadline(time.Now().Add(5 * time.Second))

	// Set a timeout only for reading
	clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))

	// Set a timeout only for writing
	clientConn.SetWriteDeadline(time.Now().Add(3 * time.Second))

	// Step 1: Send AUTH command and simulate timeout
	clientConn.Write([]byte("*2\r\n$4\r\nAUTH\r\n$14\r\nnewpassword123\r\n"))

	authResponse := make([]byte, 1024)
	n, err := clientConn.Read(authResponse)
	if err != nil {
		if nErr, ok := err.(net.Error); ok && nErr.Timeout() {
			t.Log("Timeout occurred as expected")
		} else {
			t.Fatalf("Expected timeout error, but got: %v", err)
		}
	} else {
		t.Fatalf("Expected timeout error, but read succeeded with response: %s", string(authResponse[:n]))
	}

	clientConn.Close()
	<-clientDone
	<-redisDone
}
