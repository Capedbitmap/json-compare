// main.go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os/signal"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	sonic "github.com/bytedance/sonic"
	gojson "github.com/goccy/go-json"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// Trade struct for decoding Binance WebSocket messages
type Trade struct {
	EventType          string `json:"e"`
	EventTime          int64  `json:"E"`
	Symbol             string `json:"s"`
	TradeID            int64  `json:"t"`
	Price              string `json:"p"`
	Quantity           string `json:"q"`
	BuyerOrderID       int64  `json:"b"`
	SellerOrderID      int64  `json:"a"`
	TradeTime          int64  `json:"T"`
	IsBuyerMarketMaker bool   `json:"m"`
	Ignore             bool   `json:"M"`
}

// Alias for unmarshalling
type TargetTrade Trade

// CombinedStreamMsg represents messages from combined streams
type CombinedStreamMsg struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// Stats to track performance of each JSON library
type Stats struct {
	mu               sync.Mutex
	totalTime        time.Duration
	decodeCount      int64
	errorCount       int64
	warmupComplete   bool
	warmupSampleSize int64
}

func (s *Stats) addRecord(duration time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Skip recording stats during warmup period
	if !s.warmupComplete {
		s.warmupSampleSize++
		if s.warmupSampleSize >= WarmupSamples {
			s.warmupComplete = true
			// Reset counters after warmup
			s.totalTime = 0
			s.decodeCount = 0
			s.errorCount = 0
		}
		return
	}

	if err == nil {
		s.totalTime += duration
		s.decodeCount++
	} else {
		s.errorCount++
	}
}

func (s *Stats) getAverage() (avg time.Duration, count int64, errCount int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decodeCount == 0 {
		return 0, 0, s.errorCount
	}
	avg = s.totalTime / time.Duration(s.decodeCount)
	return avg, s.decodeCount, s.errorCount
}

// Configuration
type Config struct {
	numConnections       int
	connectionsPerBatch  int
	batchDelayMs         int
	numWorkersPerLib     int
	processingQueueSize  int
	reportInterval       time.Duration
	wsBaseURL            string
	testDuration         time.Duration
	warmupPeriod         time.Duration
	randomizeOrder       bool
	useCombinedStreams   bool
	streamsPerConnection int
}

// Global constants
const (
	WarmupSamples         = 1000 // Number of samples to process before recording stats
	MaxSymbolStreams      = 1024 // Maximum streams per connection (Binance limit)
	MaxConnectionsPerFive = 300  // Maximum connections per 5 minutes (Binance limit)
)

// Global variables
var (
	// Symbol list from provided image, all with USDT as quote currency
	symbols = []string{
		"btcusdt", "ethusdt", "solusdt", "xrpusdt", "dogeusdt",
		"1000pepeusdt", "suiusdt", "adausdt", "eosusdt", "vineusdt",
	}

	// Create a stats map for each library
	decoderStats = map[string]*Stats{
		"encoding/json":   {warmupSampleSize: 0, warmupComplete: false},
		"goccy/go-json":   {warmupSampleSize: 0, warmupComplete: false},
		"bytedance/sonic": {warmupSampleSize: 0, warmupComplete: false},
		"tidwall/gjson":   {warmupSampleSize: 0, warmupComplete: false},
	}

	// Field names for use with gjson
	gjsonFields = []string{"e", "E", "s", "t", "p", "q", "b", "a", "T", "m", "M"}

	// Libraries to test - order can be randomized
	libraries = []string{
		"encoding/json",
		"goccy/go-json",
		"bytedance/sonic",
		"tidwall/gjson",
	}
)

// Initialize Sonic's optimization features
func init() {
	// Pre-compile serialization code paths for trade structure using reflection
	// This enables JIT compilation for our specific type
	sonic.Pretouch(reflect.TypeOf(TargetTrade{}))
}

func main() {
	// Parse command line flags for flexible configuration
	config := Config{
		numConnections:       10,               // Default to 10 connections
		connectionsPerBatch:  5,                // Open 5 connections at a time (respect rate limits)
		batchDelayMs:         1000,             // 1 second between batches
		numWorkersPerLib:     runtime.NumCPU(), // Default to CPU count per library
		processingQueueSize:  1024,             // Buffer size for message processing
		reportInterval:       10 * time.Second, // Reporting interval
		wsBaseURL:            "wss://stream.binance.com:9443",
		testDuration:         15 * time.Minute, // Total test duration
		warmupPeriod:         30 * time.Second, // Warmup period
		randomizeOrder:       true,             // Randomize library processing order
		useCombinedStreams:   true,             // Use combined streams by default
		streamsPerConnection: 3,                // Streams per connection
	}

	// Allow overriding defaults via flags
	flag.IntVar(&config.numConnections, "connections", config.numConnections,
		fmt.Sprintf("Number of WebSocket connections (max %d per 5 minutes)", MaxConnectionsPerFive))
	flag.IntVar(&config.connectionsPerBatch, "batch", config.connectionsPerBatch,
		"Connections to open in each batch")
	flag.IntVar(&config.batchDelayMs, "delay", config.batchDelayMs,
		"Delay in milliseconds between connection batches")
	flag.IntVar(&config.numWorkersPerLib, "workers", config.numWorkersPerLib,
		"Number of worker goroutines per library")
	flag.BoolVar(&config.randomizeOrder, "random", config.randomizeOrder,
		"Randomize the order of library processing")
	flag.BoolVar(&config.useCombinedStreams, "combined", config.useCombinedStreams,
		"Use combined streams instead of individual streams")
	flag.IntVar(&config.streamsPerConnection, "streams", config.streamsPerConnection,
		"Number of streams per connection when using combined streams")
	flag.DurationVar(&config.testDuration, "duration", config.testDuration,
		"Total test duration")
	flag.Parse()

	// Validate configuration
	if config.numConnections > MaxConnectionsPerFive {
		log.Printf("Warning: Requested %d connections exceeds Binance rate limit of %d per 5 minutes",
			config.numConnections, MaxConnectionsPerFive)
		log.Printf("Reducing to %d connections", MaxConnectionsPerFive)
		config.numConnections = MaxConnectionsPerFive
	}

	// Print info about available pairs
	log.Printf("Using %d trading pairs for benchmark", len(symbols))

	// Print configuration details
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if config.useCombinedStreams {
		totalStreams := config.numConnections * config.streamsPerConnection
		log.Printf("Starting JSON decode comparison with %d combined stream connections (%d total streams)",
			config.numConnections, totalStreams)
	} else {
		log.Printf("Starting JSON decode comparison with %d individual stream connections",
			config.numConnections)
	}
	log.Printf("Using %d workers per library, %d libraries total",
		config.numWorkersPerLib, len(libraries))
	log.Printf("Warm-up period: %v, Test duration: %v", config.warmupPeriod, config.testDuration)

	if config.randomizeOrder {
		log.Printf("Randomizing library processing order to avoid cache bias")
	}

	// Print optimizations in use
	log.Printf("Sonic optimizations: JIT compilation, ConfigFastest, pre-touched types")

	// Setup signal handling for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Add a test duration context
	testCtx, cancelTest := context.WithTimeout(ctx, config.testDuration)
	defer cancelTest()

	// Create a message buffer channel
	// This channel gets messages from all connections
	messageChan := make(chan []byte, config.processingQueueSize)

	// Create separate worker channels for each library to ensure fair testing
	channels := map[string]chan []byte{
		"encoding/json":   make(chan []byte, config.processingQueueSize),
		"goccy/go-json":   make(chan []byte, config.processingQueueSize),
		"bytedance/sonic": make(chan []byte, config.processingQueueSize),
		"tidwall/gjson":   make(chan []byte, config.processingQueueSize),
	}

	// Start message distributor - takes messages from messageChan
	// and distributes them to each library's channel
	var wg sync.WaitGroup
	wg.Add(1)
	go distributeMessages(testCtx, &wg, messageChan, channels, config.randomizeOrder)

	// Start worker pools for each library

	// 1. Standard library
	startWorkerPool(testCtx, &wg, "encoding/json", channels["encoding/json"], config.numWorkersPerLib, func(msg []byte) error {
		var trade TargetTrade
		err := json.Unmarshal(msg, &trade)
		if err != nil {
			// Log the specific error and the raw message causing it
			log.Printf("=== encoding/json DECODE ERROR ===\nError: %v\nRaw Message: %s\n=============================", err, string(msg))
		}
		return err
	})

	// 2. go-json library
	startWorkerPool(testCtx, &wg, "goccy/go-json", channels["goccy/go-json"], config.numWorkersPerLib, func(msg []byte) error {
		var trade TargetTrade
		return gojson.Unmarshal(msg, &trade)
	})

	// 3. Sonic library with optimizations
	startWorkerPool(testCtx, &wg, "bytedance/sonic", channels["bytedance/sonic"], config.numWorkersPerLib, func(msg []byte) error {
		// Use the ConfigFastest setting
		var trade TargetTrade
		err := sonic.ConfigFastest.Unmarshal(msg, &trade)
		return err
	})

	// 4. gjson library
	startWorkerPool(testCtx, &wg, "tidwall/gjson", channels["tidwall/gjson"], config.numWorkersPerLib, func(msg []byte) error {
		results := gjson.GetManyBytes(msg, gjsonFields...)
		if len(results) != len(gjsonFields) {
			return fmt.Errorf("gjson.GetManyBytes returned %d results, expected %d", len(results), len(gjsonFields))
		}
		return nil
	})

	// Start reporting goroutine
	wg.Add(1)
	go reportAverages(testCtx, &wg, config.reportInterval)

	// Start connection manager
	connectionsWg := &sync.WaitGroup{}

	// Determine endpoint URLs based on connection mode
	var endpoints []string
	if config.useCombinedStreams {
		endpoints = createCombinedStreamURLs(config)
		log.Printf("Created %d combined stream endpoints", len(endpoints))
	} else {
		// Create individual stream endpoints
		endpoints = make([]string, config.numConnections)
		for i := 0; i < config.numConnections; i++ {
			symbolIndex := i % len(symbols)
			symbol := symbols[symbolIndex]
			endpoints[i] = fmt.Sprintf("%s/ws/%s@trade", config.wsBaseURL, symbol)
		}
		log.Printf("Created %d individual stream endpoints", len(endpoints))
	}

	// Start progressively opening connections in batches to respect rate limits
	go openConnectionsInBatches(testCtx, connectionsWg, messageChan, endpoints, config)

	// Wait for test completion or termination signal
	<-testCtx.Done()

	if testCtx.Err() == context.DeadlineExceeded {
		log.Println("Test duration completed. Starting shutdown...")
	} else {
		log.Println("Shutdown signal received, closing WebSocket connections...")
	}

	// Give time for graceful WebSocket closures
	shutdownTimeout := 5 * time.Second
	log.Printf("Waiting up to %s for connections to close...", shutdownTimeout)

	// Create a timeout channel
	timeoutChan := make(chan struct{})
	go func() {
		connectionsWg.Wait()
		close(timeoutChan)
	}()

	// Wait for either connections to close or timeout
	select {
	case <-timeoutChan:
		log.Println("All WebSocket connections closed cleanly")
	case <-time.After(shutdownTimeout):
		log.Println("Timeout waiting for WebSockets to close, proceeding with shutdown")
	}

	// Close message channel to signal workers to stop
	close(messageChan)

	// Wait for all background goroutines to finish
	log.Println("Waiting for workers to complete...")
	wg.Wait()

	log.Println("All goroutines finished.")
	log.Println("Application shut down gracefully.")

	// Print final statistics
	printFinalStats()
}

// createCombinedStreamURLs creates URLs for combined streams
func createCombinedStreamURLs(config Config) []string {
	urls := make([]string, config.numConnections)

	for i := 0; i < config.numConnections; i++ {
		streamParams := make([]string, config.streamsPerConnection)

		for j := 0; j < config.streamsPerConnection; j++ {
			// Pick symbols in round-robin fashion to spread load
			symbolIndex := (i*config.streamsPerConnection + j) % len(symbols)
			streamParams[j] = fmt.Sprintf("%s@trade", symbols[symbolIndex])
		}

		// Create the combined stream URL
		streamParam := strings.Join(streamParams, "/")
		urls[i] = fmt.Sprintf("%s/stream?streams=%s", config.wsBaseURL, streamParam)
	}

	return urls
}

// distributeMessages takes messages from the input channel and fairly distributes to all libraries
func distributeMessages(ctx context.Context, wg *sync.WaitGroup, input <-chan []byte, outputs map[string]chan []byte, randomize bool) {
	defer wg.Done()
	defer func() {
		// Close all library channels when done
		for _, ch := range outputs {
			close(ch)
		}
	}()

	// Track warmup period
	warmupDone := false
	warmupTimer := time.NewTimer(30 * time.Second)
	defer warmupTimer.Stop()

	// For optional randomization of processing order
	libOrder := make([]string, len(libraries))
	copy(libOrder, libraries)

	for {
		select {
		case <-ctx.Done():
			log.Println("Message distributor stopping due to context done")
			return

		case <-warmupTimer.C:
			if !warmupDone {
				warmupDone = true
				log.Println("Warmup period completed. Now collecting performance metrics.")
				// Update stats objects
				for _, stats := range decoderStats {
					stats.warmupComplete = true
				}
			}

		case msg, ok := <-input:
			if !ok {
				log.Println("Message distributor stopping due to closed input channel")
				return
			}

			// Optionally randomize library processing order to avoid cache bias
			if randomize {
				rand.Shuffle(len(libOrder), func(i, j int) {
					libOrder[i], libOrder[j] = libOrder[j], libOrder[i]
				})
			}

			// Distribute to each library in the (possibly randomized) order
			for _, lib := range libOrder {
				// Create a copy of the message for each library
				msgCopy := make([]byte, len(msg))
				copy(msgCopy, msg)

				select {
				case outputs[lib] <- msgCopy:
					// Message sent successfully
				default:
					// Channel full - log warning but don't block
					log.Printf("Warning: %s channel full, dropping message", lib)
				}
			}
		}
	}
}

// startWorkerPool creates a pool of workers for a specific JSON library
func startWorkerPool(ctx context.Context, wg *sync.WaitGroup, name string,
	ch <-chan []byte, numWorkers int,
	processFunc func([]byte) error) {

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-ch:
					if !ok {
						return
					}

					// Process and time the message
					startTime := time.Now()
					err := processFunc(msg)
					duration := time.Since(startTime)

					// Record stats
					decoderStats[name].addRecord(duration, err)
				}
			}
		}(i + 1)
	}
	log.Printf("Started %d workers for %s", numWorkers, name)
}

// openConnectionsInBatches opens connections in batches with delays between to avoid rate limiting
func openConnectionsInBatches(ctx context.Context, wg *sync.WaitGroup,
	messageChan chan<- []byte, endpoints []string, config Config) {
	totalBatches := (len(endpoints) + config.connectionsPerBatch - 1) / config.connectionsPerBatch
	log.Printf("Opening %d connections in %d batches of %d connections each",
		len(endpoints), totalBatches, config.connectionsPerBatch)

	connectionCount := 0
	for batchNum := 0; batchNum < totalBatches; batchNum++ {
		select {
		case <-ctx.Done():
			log.Println("Connection opener stopping due to context done")
			return
		default:
		}

		batchSize := config.connectionsPerBatch
		remainingConnections := len(endpoints) - connectionCount
		if batchSize > remainingConnections {
			batchSize = remainingConnections
		}

		log.Printf("Opening batch %d of %d connections", batchNum+1, batchSize)

		// Open connections in this batch
		for i := 0; i < batchSize; i++ {
			if connectionCount >= len(endpoints) {
				break
			}

			endpoint := endpoints[connectionCount]

			wg.Add(1)
			go manageConnection(ctx, wg, endpoint, connectionCount+1, messageChan, config.useCombinedStreams)
			connectionCount++
		}

		// Wait before opening next batch
		if batchNum < totalBatches-1 {
			log.Printf("Waiting %dms before opening next batch", config.batchDelayMs)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(config.batchDelayMs) * time.Millisecond):
				// Continue after delay
			}
		}
	}

	log.Printf("All %d connections have been opened", connectionCount)
}

// manageConnection handles a single WebSocket connection
func manageConnection(ctx context.Context, wg *sync.WaitGroup, endpoint string,
	id int, messageChan chan<- []byte, isCombined bool) {
	defer wg.Done()

	log.Printf("Connection #%d: Connecting to %s", id, endpoint)

	// Use a simple backoff for retries, limited by context
	retryDelay := 1 * time.Second
	maxRetryDelay := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			log.Printf("Connection #%d: Stopped due to context done", id)
			return
		default:
		}

		// Establish connection
		conn, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
		if err != nil {
			log.Printf("Connection #%d: WebSocket Dial Error: %v. Retrying in %s...",
				id, err, retryDelay)

			select {
			case <-time.After(retryDelay):
				retryDelay *= 2
				if retryDelay > maxRetryDelay {
					retryDelay = maxRetryDelay
				}
			case <-ctx.Done():
				return
			}
			continue
		}

		log.Printf("Connection #%d: Successfully connected to %s", id, endpoint)

		// Reset retry delay on successful connection
		retryDelay = 1 * time.Second

		// Setup read deadline and pong handler
		readDeadline := 60 * time.Second
		conn.SetReadDeadline(time.Now().Add(readDeadline))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(readDeadline))
			return nil
		})

		// Ping Ticker - Binance requires pong responses within 1 minute after ping
		pingTicker := time.NewTicker(20 * time.Second)

		// Define a cleanup function
		cleanup := func() {
			pingTicker.Stop()
			conn.Close()
		}

		// Connection management variables
		connectionDone := make(chan struct{})
		var readError error

		// Start reader goroutine
		go func() {
			defer close(connectionDone)

			// Pre-allocate a buffer for combined stream messages to reduce allocations
			//var combinedMsg CombinedStreamMsg

			for {
				// Set read deadline before each read
				conn.SetReadDeadline(time.Now().Add(readDeadline))

				// Read message
				messageType, message, err := conn.ReadMessage()
				if err != nil {
					readError = err
					return
				}

				// Process text or binary messages
				if messageType == websocket.TextMessage || messageType == websocket.BinaryMessage {
					// Handle combined stream messages
					if isCombined {
						// Pre-allocate buffer reused across loop iterations
						var combinedMsg CombinedStreamMsg

						// Parse combined stream message (Using standard json as established)
						if err := gojson.Unmarshal(message, &combinedMsg); err != nil {
							log.Printf("Connection #%d: Error parsing combined stream message: %v. Raw: %s", id, err, string(message))
							continue // Skip this message
						}

						// *** THE CRITICAL FIX ***
						// Immediately make a deep copy of the inner data to prevent corruption
						// from underlying buffer reuse by the next ReadMessage() call.
						dataCopy := make([]byte, len(combinedMsg.Data))
						copy(dataCopy, combinedMsg.Data)
						message = dataCopy // Use the safe, independent copy from now on
						// *** END CRITICAL FIX ***
					}
					// else {
					// If not combined, message is already the trade data.
					// Consider if a copy is needed here too if ReadMessage buffer reuse is suspected
					// even without the combined wrapper parsing step. Often safer to copy.
					// dataCopy := make([]byte, len(message))
					// copy(dataCopy, message)
					// message = dataCopy
					// }

					// Send the safe copy (or original if not combined and not copied) to the channel
					select {
					case messageChan <- message:
						// Message enqueued successfully
					case <-ctx.Done():
						return
					default:
						// Channel full - this should be rare with sufficient buffer
						log.Printf("Connection #%d: Warning - message channel full, dropping message", id)
					}
				}

			}
		}()

		// Main connection loop
	connectionLoop:
		for {
			select {
			case <-ctx.Done():
				log.Printf("Connection #%d: Closing due to context done", id)
				// Attempt graceful close
				conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				cleanup()
				return

			case <-connectionDone:
				log.Printf("Connection #%d: Read error: %v", id, readError)
				cleanup()
				break connectionLoop

			case <-pingTicker.C:
				// Send ping to keep connection alive (Binance uses ping/pong)
				err := conn.WriteControl(websocket.PingMessage, []byte{},
					time.Now().Add(10*time.Second))
				if err != nil {
					log.Printf("Connection #%d: Error sending ping: %v", id, err)
					// Don't exit here, ReadMessage error will handle connection loss
				}
			}
		}

		// If we get here, the connection was closed - try to reconnect
		select {
		case <-ctx.Done():
			log.Printf("Connection #%d: Not reconnecting due to context done", id)
			return
		case <-time.After(retryDelay):
			log.Printf("Connection #%d: Reconnecting after %s delay", id, retryDelay)
			retryDelay *= 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		}
	}
}

// reportAverages periodically reports the performance stats
func reportAverages(ctx context.Context, wg *sync.WaitGroup, reportInterval time.Duration) {
	defer wg.Done()

	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()

	// For formatting numbers with commas
	p := message.NewPrinter(language.English)

	log.Println("Reporting averages every", reportInterval)

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			// Print Periodic Report
			log.Println("--- CURRENT DECODING STATS ---")
			printStats(p)
		}
	}
}

// printStats displays the performance statistics
func printStats(p *message.Printer) {
	fmt.Println("----------------------------------------------------------------------")
	now := time.Now().Format(time.RFC3339)
	fmt.Printf("Time: %s\n", now)
	fmt.Printf("%-20s | %-20s | %15s | %15s\n",
		"Library", "Avg Decode Time", "Success Count", "Error Count")
	fmt.Println("----------------------------------------------------------------------")

	// Ensure consistent output order
	for _, name := range libraries {
		stats := decoderStats[name]
		avg, count, errCount := stats.getAverage()

		if !stats.warmupComplete {
			fmt.Printf("%-20s | %-20s | %15s | %15s\n",
				name, "WARMING UP",
				p.Sprintf("%d", stats.warmupSampleSize),
				p.Sprintf("%d", errCount))
			continue
		}

		avgStr := "N/A"
		if count > 0 {
			avgStr = fmt.Sprintf("%.3f µs", float64(avg.Nanoseconds())/1000.0)
		}

		// Use the printer for the numbers, applying locale formatting
		p.Printf("%-20s | %-20s | %15d | %15d\n",
			name,
			avgStr,
			count,
			errCount,
		)
	}
	fmt.Println("----------------------------------------------------------------------")
}

// printFinalStats shows the final summary statistics
func printFinalStats() {
	p := message.NewPrinter(language.English)

	fmt.Println("\n==================================================================")
	fmt.Println("                     FINAL BENCHMARK RESULTS                      ")
	fmt.Println("==================================================================")

	// First, find the fastest library for reference
	var fastestLib string
	var fastestTime time.Duration

	for _, name := range libraries {
		stats := decoderStats[name]
		avg, count, _ := stats.getAverage()

		if count > 0 && (fastestLib == "" || avg < fastestTime) {
			fastestLib = name
			fastestTime = avg
		}
	}

	// Now print all libraries with relative performance
	fmt.Printf("%-20s | %-15s | %-15s | %-15s | %-15s\n",
		"Library", "Avg Time (µs)", "Messages", "Errors", "Relative Speed")
	fmt.Println("------------------------------------------------------------------")

	for _, name := range libraries {
		stats := decoderStats[name]
		avg, count, errCount := stats.getAverage()

		avgMicros := float64(avg.Nanoseconds()) / 1000.0
		relSpeed := "1.00x (fastest)"

		if name != fastestLib && fastestTime > 0 {
			relSpeed = fmt.Sprintf("%.2fx slower", float64(avg)/float64(fastestTime))
		}

		p.Printf("%-20s | %-15.3f | %-15d | %-15d | %-15s\n",
			name,
			avgMicros,
			count,
			errCount,
			relSpeed,
		)
	}

	fmt.Println("==================================================================")
}
