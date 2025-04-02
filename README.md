# JSON Compare Benchmark

This project benchmarks the JSON decoding performance of multiple Go JSON libraries on WebSocket streams. It compares the standard library (`encoding/json`), [goccy/go-json](https://github.com/goccy/go-json), [bytedance/sonic](https://github.com/bytedance/sonic), and [tidwall/gjson](https://github.com/tidwall/gjson) by simulating connections to Binance’s WebSocket endpoints.

## Overview

The tool connects to Binance (or a test endpoint) and opens multiple WebSocket connections—either as individual or combined streams. It receives trade messages and uses a worker pool for each JSON library to:
- Unmarshal trade messages to a defined structure.
- Measure the decode time, message count, and any errors.
- Report and compare performance at regular intervals.

The benchmark includes a warm-up period to mitigate cache or JIT effects. After warm-up, it collects statistics and prints a final summary, including the average decode times and relative speeds across libraries.

## Features

- **Multiple JSON Libraries:** Benchmarks `encoding/json`, `goccy/go-json`, Sonic (bytedance/sonic), and `tidwall/gjson`.
- **WebSocket Connections:** Supports individual or combined stream connections.
- **Configurable Workers:** Uses a configurable number of goroutines per JSON library.
- **Rate Limiting Considerations:** Opens connections in batches to respect Binance’s rate limits.
- **Graceful Shutdown:** Uses context and signal handling for smooth termination.
- **Performance Reporting:** Periodically prints decode statistics and final summary.

## Requirements

- Go 1.24.1 or later
- Internet connection to connect to the Binance WebSocket endpoint (unless using a local test server)

## Installation

1. Clone the repository:

   ```bash
   git clone https://github.com/your-username/json-compare.git
   cd json-compare
   ```

2. Download dependencies:

   ```bash
   go mod download
   ```

## Usage

Run the benchmark using the default settings:

```bash
go run main.go
```

### Command-Line Flags

You can override default settings using various command-line flags:

- `-connections`  
  Number of WebSocket connections to open (default: 10, max: 300 per 5 minutes).

- `-batch`  
  Number of connections to open per batch (default: 5).

- `-delay`  
  Delay in milliseconds between connection batches (default: 1000).

- `-workers`  
  Number of worker goroutines per JSON library (default: number of CPU cores).

- `-random`  
  Boolean flag to randomize the order of library processing (default: true).

- `-combined`  
  Use combined WebSocket streams instead of individual connections (default: true).

- `-streams`  
  Number of streams per connection when using combined streams (default: 3).

- `-duration`  
  Total test duration (default: 15m).

Example with custom settings:

```bash
go run main.go -connections=20 -batch=10 -delay=500 -workers=4 -duration=5m
```

## Project Structure

- `main.go`  
  Contains the main application logic with connection management, message distribution, worker pools for each JSON library, and performance reporting.

- `go.mod` & `go.sum`  
  Define module dependencies and versioning.

## Customization

- **Endpoint Configuration:**  
  By default, the application connects to Binance’s WebSocket endpoint (`wss://stream.binance.com:9443`). You can change this URL in the `Config` struct in `main.go`.

- **Message Processing:**  
  The benchmark unmarshals trade messages into a `TargetTrade` struct. Customize or extend this struct if you need more detailed benchmarking.

- **Statistics:**  
  The performance metrics are collected separately for each JSON library. You can adjust the warm-up samples number or reporting intervals via constants or the `Config` struct.

## Troubleshooting

- **WebSocket Errors:**  
  If you experience errors connecting to the WebSocket or see warnings about dropped messages, consider increasing the processing queue size or batch delay.

- **Rate Limits:**  
  Binance imposes a rate limit for WebSocket connections. If you plan to run a large number of connections, adjust the `-connections` flag (max 300 per 5 minutes) accordingly.

## License

This project is provided for benchmarking purposes. See [LICENSE](LICENSE) for details.

## Contributing

Pull requests and issue reports are welcome. Please follow the repository guidelines when contributing.