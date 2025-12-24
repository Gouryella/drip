#!/bin/bash
# Drip One-Click Performance Test Script
# Automatically starts all services, runs tests, and generates reports

set -e

# Color definitions
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
RESULTS_DIR="benchmark-results"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
LOG_DIR="/tmp/drip-test-${TIMESTAMP}"
REPORT_FILE="${RESULTS_DIR}/test-report-${TIMESTAMP}.txt"

# Port configuration
HTTP_TEST_PORT=3000
DRIP_SERVER_PORT=8443
PPROF_PORT=6060

# PID file
PIDS_FILE="${LOG_DIR}/pids.txt"

# Create directories
mkdir -p "$RESULTS_DIR"
mkdir -p "$LOG_DIR"

# ============================================
# Helper functions
# ============================================

# All logs go to stderr to avoid being consumed by command substitution $(...)
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1" >&2
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1" >&2
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1" >&2
}

log_step() {
    echo -e "\n${BLUE}==>${NC} $1\n" >&2
}

# Cleanup function
cleanup() {
    log_step "Cleaning up test environment..."

    if [ -f "$PIDS_FILE" ]; then
        log_info "Stopping all test processes..."
        while read -r pid; do
            if ps -p "$pid" > /dev/null 2>&1; then
                log_info "Stopping process $pid"
                kill "$pid" 2>/dev/null || true
            fi
        done < "$PIDS_FILE"
        rm -f "$PIDS_FILE"
    fi

    # Extra cleanup: ensure ports are released
    pkill -f "test-server.*${HTTP_TEST_PORT}" 2>/dev/null || true
    pkill -f "drip server.*${DRIP_SERVER_PORT}" 2>/dev/null || true
    pkill -f "drip http ${HTTP_TEST_PORT}" 2>/dev/null || true

    log_info "Cleanup completed"
}

# Register cleanup function
trap cleanup EXIT INT TERM

# Check dependencies
check_dependencies() {
    log_step "Checking dependencies..."

    local missing=""

    if ! command -v wrk &> /dev/null; then
        missing="${missing}\n  - wrk (brew install wrk)"
    fi

    if ! command -v go &> /dev/null; then
        missing="${missing}\n  - go (https://go.dev/dl/)"
    fi

    if ! command -v openssl &> /dev/null; then
        missing="${missing}\n  - openssl"
    fi

    if ! command -v nc &> /dev/null; then
        missing="${missing}\n  - nc (netcat)"
    fi

    if [ ! -f "./bin/drip" ]; then
        log_error "Cannot find drip executable"
        log_info "Please run: make build"
        exit 1
    fi

    if [ -n "$missing" ]; then
        log_error "Missing dependencies:${missing}"
        exit 1
    fi

    log_info "✓ All dependencies satisfied"
}

# Generate self-signed ECDSA certificate for testing
generate_test_certs() {
    log_step "Generating test TLS certificate (ECDSA)..."

    local cert_dir="${LOG_DIR}/certs"
    mkdir -p "$cert_dir"

    # Generate ECDSA private key (prime256v1 = P-256)
    openssl ecparam -name prime256v1 -genkey -noout \
        -out "${cert_dir}/server.key" >/dev/null 2>&1

    # Generate self-signed certificate with this private key
    openssl req -new -x509 \
        -key "${cert_dir}/server.key" \
        -out "${cert_dir}/server.crt" \
        -days 1 \
        -subj "/C=US/ST=Test/L=Test/O=Test/CN=localhost" \
        >/dev/null 2>&1

    if [ -f "${cert_dir}/server.crt" ] && [ -f "${cert_dir}/server.key" ]; then
        log_info "✓ ECDSA test certificate generated"
        # Note: this echo is the "return value", stdout only outputs this line
        echo "${cert_dir}/server.crt ${cert_dir}/server.key"
    else
        log_error "ECDSA certificate generation failed"
        exit 1
    fi
}

# Wait for port to be available
wait_for_port() {
    local port=$1
    local max_wait=${2:-30}
    local waited=0

    while ! nc -z localhost "$port" 2>/dev/null; do
        if [ "$waited" -ge "$max_wait" ]; then
            return 1
        fi
        sleep 1
        waited=$((waited + 1))
    done
    return 0
}

# Start HTTP test server (high-performance Go server)
start_http_server() {
    log_step "Starting HTTP test server (port $HTTP_TEST_PORT)..."

    # Build Go test server
    local test_server_dir="scripts/test/test-server"
    local test_server_bin="${LOG_DIR}/test-server"

    if [ ! -d "$test_server_dir" ]; then
        log_error "Test server source not found: $test_server_dir"
        exit 1
    fi

    log_info "Building Go test server..."
    if ! go build -o "$test_server_bin" "./$test_server_dir" > "${LOG_DIR}/build.log" 2>&1; then
        log_error "Failed to build test server"
        cat "${LOG_DIR}/build.log"
        exit 1
    fi

    # Start the Go test server
    "$test_server_bin" -port "$HTTP_TEST_PORT" \
        > "${LOG_DIR}/http-server.log" 2>&1 &
    local pid=$!
    echo "$pid" >> "$PIDS_FILE"

    if wait_for_port "$HTTP_TEST_PORT" 10; then
        log_info "✓ HTTP test server started (PID: $pid)"
    else
        log_error "HTTP test server failed to start"
        cat "${LOG_DIR}/http-server.log"
        exit 1
    fi
}

# Start Drip server
start_drip_server() {
    log_step "Starting Drip server (port $DRIP_SERVER_PORT)..."

    local cert_path=$1
    local key_path=$2

    ./bin/drip server \
        --port "$DRIP_SERVER_PORT" \
        --domain localhost \
        --tls-cert "$cert_path" \
        --tls-key "$key_path" \
        > "${LOG_DIR}/drip-server.log" 2>&1 &
    local pid=$!
    echo "$pid" >> "$PIDS_FILE"

    if wait_for_port "$DRIP_SERVER_PORT" 10; then
        log_info "✓ Drip server started (PID: $pid)"
    else
        log_error "Drip server failed to start"
        cat "${LOG_DIR}/drip-server.log"
        exit 1
    fi
}

# Start Drip client and extract URL
start_drip_client() {
    log_step "Starting Drip client..."

    ./bin/drip http "$HTTP_TEST_PORT" \
        --server "localhost:${DRIP_SERVER_PORT}" \
        --insecure \
        > "${LOG_DIR}/drip-client.log" 2>&1 &
    local pid=$!
    echo "$pid" >> "$PIDS_FILE"

    # Wait for client to start and extract URL
    log_info "Waiting for tunnel to establish..."
    sleep 3

    # Extract tunnel URL from logs
    local tunnel_url=""
    local max_attempts=10
    local attempt=0

    while [ "$attempt" -lt "$max_attempts" ]; do
        # Use grep to extract URL starting with https:// and remove ANSI color codes
        tunnel_url=$(grep -oE 'https://[a-zA-Z0-9.-]+:[0-9]+' "${LOG_DIR}/drip-client.log" 2>/dev/null | head -1)
        if [ -n "$tunnel_url" ]; then
            break
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    if [ -z "$tunnel_url" ]; then
        log_error "Cannot get tunnel URL"
        log_info "Client logs:"
        cat "${LOG_DIR}/drip-client.log"
        exit 1
    fi

    log_info "✓ Drip client started (PID: $pid)"
    log_info "✓ Tunnel URL: $tunnel_url"

    # Return URL
    echo "$tunnel_url"
}

# Verify connectivity
verify_connectivity() {
    local url=$1
    log_step "Verifying tunnel connectivity..."

    local max_attempts=5
    local attempt=0

    while [ "$attempt" -lt "$max_attempts" ]; do
        if curl -sk --max-time 5 "$url" > /dev/null 2>&1; then
            log_info "✓ Tunnel connectivity normal"
            return 0
        fi
        attempt=$((attempt + 1))
        log_warn "Attempt $attempt/$max_attempts..."
        sleep 2
    done

    log_error "Tunnel connectivity test failed"
    return 1
}

# Run performance tests
run_performance_tests() {
    local url=$1

    log_step "Starting performance tests..."

    # Test 1: Quick benchmark
    log_info "[1/3] Quick benchmark (10s)..."
    wrk -t 4 -c 50 -d 10s --latency "$url" \
        > "${RESULTS_DIR}/quick-benchmark-${TIMESTAMP}.txt" 2>&1

    # Test 2: Standard load test
    log_info "[2/3] Standard load test (30s)..."
    wrk -t 8 -c 100 -d 30s --latency "$url" \
        > "${RESULTS_DIR}/standard-benchmark-${TIMESTAMP}.txt" 2>&1

    # Test 3: High concurrency test
    log_info "[3/3] High concurrency test (30s)..."
    wrk -t 12 -c 400 -d 30s --latency "$url" \
        > "${RESULTS_DIR}/high-concurrency-${TIMESTAMP}.txt" 2>&1

    log_info "✓ Performance tests completed"
}

# Generate test report
generate_report() {
    log_step "Generating test report..."

    cat > "$REPORT_FILE" << EOF
========================================
Drip Performance Test Report
========================================

Test Time: $(date)
Test Version: $(./bin/drip version 2>/dev/null | head -1 || echo "unknown")

========================================
Test Environment
========================================

OS: $(uname -s)
CPU Cores: $(sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo "unknown")
Memory: $(sysctl -n hw.memsize 2>/dev/null | awk '{print int($1/1024/1024/1024)"GB"}' || echo "unknown")

========================================
Test Results
========================================

EOF

    # Parse and add results from each test
    if [ -f "${RESULTS_DIR}/quick-benchmark-${TIMESTAMP}.txt" ]; then
        {
            echo "--- Quick Benchmark (10s, 50 connections) ---"
            grep "Requests/sec:" "${RESULTS_DIR}/quick-benchmark-${TIMESTAMP}.txt"
            grep "Transfer/sec:" "${RESULTS_DIR}/quick-benchmark-${TIMESTAMP}.txt"
            echo ""
            grep "Latency" "${RESULTS_DIR}/quick-benchmark-${TIMESTAMP}.txt" | head -1
            grep -A 3 "Latency Distribution" "${RESULTS_DIR}/quick-benchmark-${TIMESTAMP}.txt"
            echo ""
        } >> "$REPORT_FILE"
    fi

    if [ -f "${RESULTS_DIR}/standard-benchmark-${TIMESTAMP}.txt" ]; then
        {
            echo "--- Standard Load Test (30s, 100 connections) ---"
            grep "Requests/sec:" "${RESULTS_DIR}/standard-benchmark-${TIMESTAMP}.txt"
            grep "Transfer/sec:" "${RESULTS_DIR}/standard-benchmark-${TIMESTAMP}.txt"
            echo ""
            grep "Latency" "${RESULTS_DIR}/standard-benchmark-${TIMESTAMP}.txt" | head -1
            grep -A 3 "Latency Distribution" "${RESULTS_DIR}/standard-benchmark-${TIMESTAMP}.txt"
            echo ""
        } >> "$REPORT_FILE"
    fi

    if [ -f "${RESULTS_DIR}/high-concurrency-${TIMESTAMP}.txt" ]; then
        {
            echo "--- High Concurrency Test (30s, 400 connections) ---"
            grep "Requests/sec:" "${RESULTS_DIR}/high-concurrency-${TIMESTAMP}.txt"
            grep "Transfer/sec:" "${RESULTS_DIR}/high-concurrency-${TIMESTAMP}.txt"
            echo ""
            grep "Latency" "${RESULTS_DIR}/high-concurrency-${TIMESTAMP}.txt" | head -1
            grep -A 3 "Latency Distribution" "${RESULTS_DIR}/high-concurrency-${TIMESTAMP}.txt"
            echo ""
        } >> "$REPORT_FILE"
    fi

    # Add performance evaluation
    cat >> "$REPORT_FILE" << 'EOF'
========================================
Performance Evaluation Standards
========================================

Excellent (Phase 1 optimization target):
  ✓ QPS > 5000
  ✓ P99 latency < 50ms
  ✓ Error rate = 0%

Good:
  ✓ QPS > 2000
  ✓ P99 latency < 100ms
  ✓ Error rate < 0.1%

Needs Optimization:
  ✗ QPS < 1000
  ✗ P99 latency > 200ms

========================================
Detailed Result Files
========================================

EOF

    {
        echo "Quick test: ${RESULTS_DIR}/quick-benchmark-${TIMESTAMP}.txt"
        echo "Standard test: ${RESULTS_DIR}/standard-benchmark-${TIMESTAMP}.txt"
        echo "High concurrency test: ${RESULTS_DIR}/high-concurrency-${TIMESTAMP}.txt"
        echo ""
        echo "Log directory: ${LOG_DIR}/"
    } >> "$REPORT_FILE"

    log_info "✓ Test report generated: $REPORT_FILE"
}

# Show report summary
show_summary() {
    log_step "Test Results Summary"

    if [ -f "${RESULTS_DIR}/standard-benchmark-${TIMESTAMP}.txt" ]; then
        echo ""
        echo "========================================="
        echo "  Standard Load Test Results"
        echo "========================================="
        grep "Requests/sec:" "${RESULTS_DIR}/standard-benchmark-${TIMESTAMP}.txt"
        grep "Transfer/sec:" "${RESULTS_DIR}/standard-benchmark-${TIMESTAMP}.txt"
        echo ""
        grep "50%" "${RESULTS_DIR}/standard-benchmark-${TIMESTAMP}.txt"
        grep "99%" "${RESULTS_DIR}/standard-benchmark-${TIMESTAMP}.txt"
        echo "========================================="
        echo ""
    fi

    log_info "Full report: $REPORT_FILE"
    log_info "Detailed results: $RESULTS_DIR/"
}

# ============================================
# Main flow
# ============================================

main() {
    clear
    echo "========================================="
    echo "  Drip One-Click Performance Test"
    echo "========================================="
    echo ""

    # Check dependencies
    check_dependencies

    # Generate test certificate (ECDSA), only capture the last line of stdout with paths
    CERT_PATHS=$(generate_test_certs)
    CERT_FILE=$(echo "$CERT_PATHS" | awk '{print $1}')
    KEY_FILE=$(echo "$CERT_PATHS" | awk '{print $2}')

    log_info "Using certificate: $CERT_FILE"
    log_info "Using private key: $KEY_FILE"

    # Start all services
    start_http_server
    start_drip_server "$CERT_FILE" "$KEY_FILE"
    TUNNEL_URL=$(start_drip_client)

    # Verify connectivity
    if ! verify_connectivity "$TUNNEL_URL"; then
        log_error "Test aborted: tunnel not accessible"
        exit 1
    fi

    # Warm up (sequential to ensure connection pool is ready)
    log_info "Warming up tunnel (5s)..."
    for _ in {1..20}; do
        curl -sk --max-time 2 "$TUNNEL_URL" > /dev/null 2>&1 || true
    done
    sleep 2

    # Run tests
    run_performance_tests "$TUNNEL_URL"

    # Generate report
    generate_report

    # Show summary
    show_summary

    log_step "Testing completed!"
    echo ""
    echo "Press any key to view full report, or Ctrl+C to exit..."
    read -n 1 -s

    cat "$REPORT_FILE"
}

# Run main flow
main
