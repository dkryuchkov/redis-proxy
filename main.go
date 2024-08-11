package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Define a mutex for protecting the clientInfoMap
var clientInfoMapMutex sync.RWMutex

var size int32 = 1024

var clientCreds atomic.Value // Holds map[string]map[string]interface{}
var redisPassword, redisConnString string

var singleOkResponse = []byte("+OK\r\n")

type ClientInfo struct {
	isAuthenticated bool
	sourceIP        string
	allocatedIndex  string
}

var (
	currentWorkers    int32
	baseWorkers       int32 = getIntEnvOrDefault("BASE_WORKERS", 1)
	maxWorkers        int32 = getIntEnvOrDefault("MAX_WORKERS", 1000)
	workQueue               = make(chan connectionTask, getIntEnvOrDefault("WORK_QUEUE_SIZE", 1000))
	workerIdleTimeout       = time.Duration(getIntEnvOrDefault("WORKER_IDLE_TIMEOUT_SEC", 30)) * time.Second
	activeConnections int64
	dialFunc          = func(network, address string) (net.Conn, error) {
		return net.Dial(network, address)
	}
)

// getIntEnvOrDefault tries to get an environment variable as an int. If not found or on error, returns the default value.
func getIntEnvOrDefault(envKey string, defaultValue int) int32 {
	valueStr, exists := os.LookupEnv(envKey)
	if !exists {
		return int32(defaultValue)
	}
	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return int32(defaultValue)
	}
	return int32(value)
}

// connectionTask encapsulates a connection and a channel to signal readiness.
type connectionTask struct {
	conn net.Conn
}

// worker listens for work tasks and exits if idle for too long.
func worker(work <-chan connectionTask, dialFunc dialFuncType) {
	atomic.AddInt32(&currentWorkers, 1)
	defer atomic.AddInt32(&currentWorkers, -1)

	idleTimer := time.NewTimer(workerIdleTimeout)
	defer idleTimer.Stop()

	for {
		select {
		case task, ok := <-work:
			if !ok {
				return // Work channel closed
			}
			if !idleTimer.Stop() {
				<-idleTimer.C
			}
			handleConnection(task.conn, dialFunc)
			idleTimer.Reset(workerIdleTimeout)

		case <-idleTimer.C:
			return // Exit due to inactivity
		}
	}
}

func scaleWorkersIfNeeded() {
	totalWorkers := atomic.LoadInt32(&currentWorkers)

	if totalWorkers < maxWorkers {
		go worker(workQueue, dialFunc)
	}
}

func main() {
	var pendingTasks int32
	config, err := getKubernetesConfig()
	if err != nil {
		log.Fatalf("Failed to get Kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	redisPassword, redisConnString, err = fetchRedisCredentials(clientset)
	if err != nil {
		log.Fatalf("Failed to fetch Redis credentials: %v", err)
	}

	log.Printf("Redis credentials fetched: password length = %d, connection string length = %d", len(redisPassword), len(redisConnString))

	refreshClientCredentials(clientset)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)

	go func() {
		for sig := range sigs {
			if sig == syscall.SIGUSR1 {
				log.Println("Received SIGUSR1, refreshing client credentials...")
				refreshClientCredentials(clientset)
			} else {
				log.Printf("Received signal: %v, shutting down...", sig)
				close(workQueue)
				os.Exit(0)
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			log.Println("Refreshing client credentials...")
			refreshClientCredentials(clientset)
		}
	}()

	for i := int32(0); i < baseWorkers; i++ {
		go worker(workQueue, dialFunc)
	}

	ln, err := net.Listen("tcp", ":6379")
	if err != nil {
		log.Fatalf("Failed to listen on port 6379: %v", err)
	}
	defer ln.Close()

	log.Println("Server listening on port 6379")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("Shutting down server...")
		close(workQueue)
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break // Context cancelled, server is shutting down
			}
			log.Printf("Failed to accept connection: %v", err)
			continue
		}

		atomic.AddInt32(&pendingTasks, 1)
		scaleWorkersIfNeeded()

		go func(conn net.Conn) {
			workQueue <- connectionTask{conn: conn}
			atomic.AddInt32(&pendingTasks, -1)
		}(conn)
	}
}

// This map should be protected by a synchronization primitive like sync.RWMutex
var clientInfoMap = make(map[net.Conn]*ClientInfo)

// FetchSecretData fetches a Kubernetes secret and returns its data as a map.
func FetchSecretData(clientset *kubernetes.Clientset, namespace string, secretName string) (map[string][]byte, error) {
	secret, err := clientset.CoreV1().Secrets(namespace).Get(context.TODO(), secretName, v1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return secret.Data, nil
}

func getKubernetesConfig() (*rest.Config, error) {
	if os.Getenv("KUBERNETES_PORT") != "" {
		return rest.InClusterConfig()
	}
	kubeconfig := os.Getenv("HOME") + "/.kube/config"
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

// fetchRedisCredentials retrieves the Redis password and connection string from a Kubernetes secret.
func fetchRedisCredentials(clientset *kubernetes.Clientset) (string, string, error) {
	secretName := "redis-secret-master"
	namespace := "kube-system"
	passwordKey := "redis-password"
	connStringKey := "redis-connection-string"

	secret, err := clientset.CoreV1().Secrets(namespace).Get(context.TODO(), secretName, v1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("failed to fetch secret '%s/%s': %v", namespace, secretName, err)
	}

	passwordBytes, ok := secret.Data[passwordKey]
	if !ok {
		return "", "", fmt.Errorf("'%s' key not found in secret '%s/%s'", passwordKey, namespace, secretName)
	}
	password := string(passwordBytes)

	connStringBytes, ok := secret.Data[connStringKey]
	if !ok {
		return "", "", fmt.Errorf("'%s' key not found in secret '%s/%s'", connStringKey, namespace, secretName)
	}
	connString := string(connStringBytes)

	return password, connString, nil
}

// Define a type for the dial function to make it easier to test
type dialFuncType func(network, address string) (net.Conn, error)

func handleConnection(conn net.Conn, dialFunc dialFuncType) {
	defer conn.Close()

	var clientAuthorized bool
	u, err := url.Parse(redisConnString)
	if err != nil {
		log.Fatalf("Failed to parse URL: %v", err)
	}

	host := "redis-headless.kube-system.svc.cluster.local:6379"
	if os.Getenv("KUBERNETES_PORT") != "" {
		host = u.Host
	}

	redisConn, err := dialFunc("tcp", host)
	if err != nil {
		log.Printf("Failed to connect to Redis: %v", err)
		return
	}
	defer redisConn.Close()

	newCount := atomic.AddInt64(&activeConnections, 1)
	log.Printf("Client %v connected. Total active connections: %d", conn.RemoteAddr(), newCount)

	defer func() {
		newCount := atomic.AddInt64(&activeConnections, -1)
		log.Printf("Client %v closed connection. Total active connections: %d", conn.RemoteAddr(), newCount)
	}()

	ch := make(chan []byte, 10)
	defer close(ch)

	go func() {
		for buf := range ch {
			_, err = conn.Write(buf)
			if err != nil {
				log.Printf("Failed to write to client: %v", err)
				return
			}
		}
	}()

	for {
		var totalBuf []byte

		for {
			buf := make([]byte, size)
			n, err := conn.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Printf("Error reading from client: %v", err)
				}
				return
			}

			totalBuf = append(totalBuf, buf[:n]...)

			if isCommandEnd(totalBuf) {
				break
			}

			nextBulkLength, isBulk := getLastBulkLength(totalBuf)
			if isBulk && nextBulkLength > 0 {
				size = max(size, nextBulkLength+2)
			}
		}

		commandStart := 0
		for commandStart < len(totalBuf) {
			commandEnd, err := findCommandEnd(totalBuf[commandStart:], totalBuf)
			if err != nil {
				fmt.Println("Error:", err)
				break
			}

			remainingBytes := commandEnd - len(totalBuf)
			if remainingBytes > 0 {
				additionalBuf, err := readUntil(conn, remainingBytes)
				if err != nil {
					log.Printf("Error reading until: %v", err)
					return
				}
				totalBuf = append(totalBuf, additionalBuf...)
			}

			commandBuf := totalBuf[commandStart : commandStart+commandEnd]
			commandStart += commandEnd

			var commandParts []string
			if commandBuf[0] == '*' {
				reader := bytes.NewReader(commandBuf)
				bufReader := bufio.NewReader(reader)

				_, err = bufReader.ReadByte()
				if err != nil {
					log.Printf("Failed to read 1: %v", err)
					return
				}

				line, err := bufReader.ReadString('\n')
				if err != nil {
					log.Printf("Failed to read 2: %v", err)
					return
				}

				arrayLen, err := strconv.Atoi(strings.TrimSpace(line))
				if err != nil {
					log.Printf("Failed to convert array length: %v", err)
					return
				}

				for i := 0; i < arrayLen; i++ {
					_, err := bufReader.ReadString('$')
					if err != nil {
						log.Printf("Failed to read '$': %v", err)
						return
					}

					lenStr, err := bufReader.ReadString('\n')
					if err != nil {
						log.Printf("Failed to read length of argument: %v", err)
						return
					}

					argLen, err := strconv.Atoi(strings.TrimSpace(lenStr))
					if err != nil {
						log.Printf("Failed to convert length to integer: %v", err)
						return
					}

					argBytes := make([]byte, argLen)
					_, err = io.ReadFull(bufReader, argBytes)
					if err != nil {
						log.Printf("Failed to read argument: %v", err)
						return
					}

					commandParts = append(commandParts, string(argBytes))
				}
			} else {
				commandLine := strings.TrimRight(string(commandBuf), "\r\n")
				commandParts = strings.Split(commandLine, " ")
			}

			if len(commandParts) > 1 && strings.ToUpper(commandParts[0]) == "SELECT" {
				selectedIndex := commandParts[1]

				clientInfoMapMutex.RLock()
				clientInfo, exists := clientInfoMap[conn]
				clientInfoMapMutex.RUnlock()

				if !exists {
					log.Printf("Warning: SELECT command discovered but client info not found for connection %v", conn)
					return
				}

				if !clientInfo.isAuthenticated || selectedIndex != clientInfo.allocatedIndex {
					log.Printf("Warning: Unauthorized SELECT command. "+
						"Client IP: %s, Authenticated: %t, Selected Index: %s, Allocated Index: %s",
						clientInfo.sourceIP, clientInfo.isAuthenticated, selectedIndex, clientInfo.allocatedIndex)
					ch <- singleOkResponse
					break
				}
			}

			if len(commandParts) > 1 && strings.ToUpper(commandParts[0]) == "AUTH" {
				if !clientAuthorized {
					clientAuthorized = handleAuth(redisConn, conn, commandParts[1])
					if clientAuthorized {
						ch <- singleOkResponse
					}
				} else {
					ch <- singleOkResponse
				}
			} else {
				if clientAuthorized {
					_, err := redisConn.Write(commandBuf)
					if err != nil {
						log.Printf("Error writing to Redis: %v", err)
						return
					}

					responseBuf, err := readResponse(bufio.NewReader(redisConn))
					if err != nil {
						log.Printf("Error reading from Redis: %v", err)
						return
					}

					ch <- responseBuf
				} else {
					log.Printf("Unauthorized")
					return
				}
			}
		}
	}
}

func handleAuth(redisConn net.Conn, conn net.Conn, password string) bool {
	// Load the client credentials from the atomic.Value
	credsMap, ok := clientCreds.Load().(map[string]map[string]interface{})
	if !ok || credsMap == nil {
		log.Printf("[handleAuth] credsMap is nil or invalid")
		conn.Write([]byte("-ERR internal server error\r\n"))
		return false
	}

	// Look up the credentials for the provided password
	creds, ok := credsMap[password]
	if !ok {
		conn.Write([]byte("-ERR invalid password\r\n"))
		log.Printf("[handleAuth] Wrong password client: %v", password)
		return false
	}

	// Check if the credentials have an expiration and if they are expired
	if validUntil, exists := creds["valid_until"]; exists {
		if time.Now().After(validUntil.(time.Time)) {
			conn.Write([]byte("-ERR password expired\r\n"))
			log.Printf("[handleAuth] Password expired: %v", password)
			return false
		}
	}

	// Prepare the AUTH and SELECT commands for Redis
	authCommand := fmt.Sprintf("*2\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n", len(redisPassword), redisPassword)
	selectCommand := fmt.Sprintf("*2\r\n$6\r\nSELECT\r\n$%d\r\n%s\r\n", len(creds["dbIndex"].(string)), creds["dbIndex"].(string))
	commands := []string{authCommand, selectCommand}

	// Send the commands to Redis and check for success
	for _, command := range commands {
		if _, err := redisConn.Write([]byte(command)); err != nil {
			log.Printf("[handleAuth] Failed to write command to Redis: %v", err)
			conn.Write([]byte("-ERR internal server error\r\n"))
			return false
		}
		if _, err := readResponse(bufio.NewReader(redisConn)); err != nil {
			log.Printf("[handleAuth] Failed to read response from Redis: %v", err)
			conn.Write([]byte("-ERR internal server error\r\n"))
			return false
		}
	}

	// Check if conn.RemoteAddr() is not nil before using it
	var sourceIP string
	if addr := conn.RemoteAddr(); addr != nil {
		sourceIP = addr.String()
	} else {
		sourceIP = "unknown" // Fallback if RemoteAddr is nil
	}

	// Mark the client as authenticated and store their information
	clientInfo := &ClientInfo{
		isAuthenticated: true,
		sourceIP:        sourceIP,
		allocatedIndex:  creds["dbIndex"].(string),
	}
	clientInfoMapMutex.Lock()
	clientInfoMap[conn] = clientInfo
	clientInfoMapMutex.Unlock()

	return true
}

func readResponse(reader *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	prefix, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	buf.WriteByte(prefix)

	switch prefix {
	case '+', '-', ':':
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		buf.Write(line)
	case '$':
		lengthStr, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		buf.WriteString(lengthStr)

		length, err := strconv.Atoi(lengthStr[:len(lengthStr)-2])
		if err != nil {
			return nil, err
		}
		if length == -1 {
		} else {
			data := make([]byte, length+2)
			_, err = io.ReadFull(reader, data)
			if err != nil {
				return nil, err
			}
			buf.Write(data)
		}
	case '*':
		lengthStr, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		buf.WriteString(lengthStr)

		length, err := strconv.Atoi(lengthStr[:len(lengthStr)-2])
		if err != nil {
			return nil, err
		}
		if length == -1 {

		} else {
			for i := 0; i < length; i++ {
				item, err := readResponse(reader)
				if err != nil {
					return nil, err
				}
				buf.Write(item)
			}
		}
	default:
		return nil, fmt.Errorf("unknown RESP type: %c", prefix)
	}
	return buf.Bytes(), nil
}

func isCommandEnd(buf []byte) bool {
	if len(buf) == 0 {
		return false
	}

	switch buf[0] {
	case '+', '-': // Simple strings or errors
		return bytes.HasSuffix(buf, []byte("\r\n"))
	case ':': // Integers
		return bytes.HasSuffix(buf, []byte("\r\n"))
	case '$': // Bulk strings
		return isBulkStringComplete(buf)
	case '*': // Arrays
		return isArrayComplete(buf)
	default:
		return false
	}
}

func isBulkStringComplete(buf []byte) bool {
	crlfPos := bytes.Index(buf, []byte("\r\n"))
	if crlfPos == -1 {
		return false
	}

	lengthStr := buf[1:crlfPos]
	length, err := strconv.Atoi(string(lengthStr))
	if err != nil || length < 0 {
		return false
	}

	totalLength := crlfPos + 2 + length + 2
	return len(buf) >= totalLength
}

func isArrayComplete(buf []byte) bool {
	currentPos := 0
	var lastArrayEnd int

	for currentPos < len(buf) && buf[currentPos] == '*' {
		crlfPos := bytes.Index(buf[currentPos:], []byte("\r\n"))
		if crlfPos == -1 {
			return false // Incomplete array length specifier
		}

		crlfPos += currentPos

		countStr := buf[currentPos+1 : crlfPos]
		count, err := strconv.Atoi(string(countStr))
		if err != nil || count <= 0 {
			return false // Invalid array length
		}

		currentPos = crlfPos + 2
		for i := 0; i < count; i++ {
			if currentPos >= len(buf) || buf[currentPos] != '$' {
				return false // Incomplete or invalid bulk string
			}

			nextCrlfPos := bytes.Index(buf[currentPos:], []byte("\r\n"))
			if nextCrlfPos == -1 {
				return false // Incomplete bulk string length specifier
			}

			nextCrlfPos += currentPos

			bulkLenStr := buf[currentPos+1 : nextCrlfPos]
			bulkLen, err := strconv.Atoi(string(bulkLenStr))
			if err != nil || bulkLen < 0 {
				return false // Invalid bulk string length
			}

			currentPos = nextCrlfPos + 2 + bulkLen + 2 // Move past the bulk string and its CRLF
		}

		lastArrayEnd = currentPos
	}

	return lastArrayEnd == len(buf)
}

func getLastBulkLength(buf []byte) (int32, bool) {
	if len(buf) < 3 || buf[0] != '*' {
		return 0, false
	}

	crlfPos := bytes.Index(buf, []byte("\r\n"))
	if crlfPos == -1 {
		return 0, false // No CRLF found, incomplete command
	}

	arrayLenStr := buf[1:crlfPos]
	arrayLen, err := strconv.Atoi(string(arrayLenStr))
	if err != nil || arrayLen <= 0 {
		return 0, false // Invalid array length
	}

	currentPos := crlfPos + 2
	var lastBulkLen int
	var lastBulkLen64 int64

	for i := 0; i < arrayLen; i++ {
		if currentPos >= len(buf) {
			return 0, false // Incomplete command
		}

		if buf[currentPos] != '$' {
			return 0, false // Invalid bulk string length specifier
		}

		crlfPos = bytes.Index(buf[currentPos:], []byte("\r\n"))
		if crlfPos == -1 {
			return 0, false // Incomplete bulk string length
		}

		crlfPos += currentPos

		bulkLenStr := buf[currentPos+1 : crlfPos]
		lastBulkLen, err = strconv.Atoi(string(bulkLenStr))
		if err != nil || lastBulkLen < 0 {
			return 0, false // Invalid bulk string length
		}

		lastBulkLen64, err = strconv.ParseInt(string(bulkLenStr), 10, 32)
		if err != nil {
			return 0, false
		}

		currentPos += crlfPos + 2 + lastBulkLen + 2
	}

	return int32(lastBulkLen64), true // Return the length of the last bulk string
}

func findCommandEnd(buf []byte, totalBuf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, fmt.Errorf("buffer is empty")
	}
	switch buf[0] {
	case '*':
		return findArrayEnd(buf, totalBuf)
	default:
		return findInlineCommandEnd(buf)
	}
}

func findInlineCommandEnd(buf []byte) (int, error) {
	if newlinePos := bytes.IndexByte(buf, '\n'); newlinePos != -1 {
		commandLine := string(buf[:newlinePos])
		if !isBalancedQuotes(commandLine) {
			return 0, fmt.Errorf("unbalanced quotes in command: %s", commandLine)
		}
		return newlinePos + 1, nil
	}
	return 0, fmt.Errorf("incomplete inline command")
}

func isBalancedQuotes(s string) bool {
	var quoteCount int
	var escaped bool
	for _, ch := range s {
		switch ch {
		case '\\':
			escaped = !escaped
		case '"':
			if !escaped {
				quoteCount++
			}
			escaped = false
		default:
			escaped = false
		}
	}
	return quoteCount%2 == 0
}

func findArrayEnd(buf []byte, totalBuf []byte) (int, error) {
	offset := len(totalBuf) - len(buf)

	lineEnd := bytes.Index(buf, []byte("\r\n"))
	if lineEnd == -1 {
		log.Printf("Incomplete array, buffer: %s", string(buf))
		return 0, fmt.Errorf("incomplete array")
	}

	count, err := strconv.Atoi(string(buf[1:lineEnd]))
	if err != nil {
		log.Printf("Error parsing array length, buffer: %s, error: %v", string(buf), err)
		return 0, err
	}

	pos := lineEnd + 2
	for i := 0; i < count; i++ {
		if pos >= len(buf) {
			log.Printf("Unexpected end of buffer at position: %d, buffer: %s", pos, string(buf))
			return 0, fmt.Errorf("unexpected end of buffer at position: %d", pos)
		}

		if buf[pos] != '$' {
			log.Printf("Expected bulk string but found: %c, buffer: %s", buf[pos], string(buf))
			return 0, fmt.Errorf("expected bulk string")
		}

		lineEnd = bytes.Index(buf[pos:], []byte("\r\n"))
		if lineEnd == -1 {
			log.Printf("Incomplete bulk string, buffer: %s", string(buf))
			return 0, fmt.Errorf("incomplete bulk string")
		}

		length, err := strconv.Atoi(string(buf[pos+1 : pos+lineEnd]))
		if err != nil {
			log.Printf("Error parsing bulk string length, buffer: %s, error: %v", string(buf), err)
			return 0, err
		}

		nextPos := pos + lineEnd + 2 + length + 2

		if nextPos+offset > len(totalBuf) {
			log.Printf("Attempting to jump beyond buffer length: nextPos = %d, buffer length = %d, buffer: %s", nextPos+offset, len(totalBuf), string(buf))
			log.Printf("\n\n\ntotalBuf: %s", string(totalBuf))
			return 0, fmt.Errorf("attempting to jump beyond buffer length")
		}

		pos = nextPos
	}

	return pos, nil
}

func readUntil(conn net.Conn, expectedLength int) ([]byte, error) {
	var totalBuf []byte
	var bytesRead int

	for bytesRead < expectedLength {
		remaining := expectedLength - bytesRead
		buf := make([]byte, min(size, int32(remaining)))

		n, err := conn.Read(buf)
		if err != nil {
			log.Printf("Error reading from client: %v", err)
			return nil, fmt.Errorf("error reading from client: %w", err)
		}

		totalBuf = append(totalBuf, buf[:n]...)
		bytesRead += n
		log.Printf("Read additional %d bytes, total read so far: %d, expected length: %d", n, bytesRead, expectedLength)
	}

	log.Printf("Finished reading until, total read: %d bytes", bytesRead)
	return totalBuf, nil
}

func min(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}
