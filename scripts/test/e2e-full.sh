#!/usr/bin/env bash
# Full end-to-end functional test for Drip tunnels.
# Covers HTTP/TCP over TLS and WSS, SSE streaming, proxy auth, health, and concurrency.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
LOG_DIR="${DRIP_E2E_LOG_DIR:-/tmp/drip-e2e-${TIMESTAMP}}"
CERT_DIR="${LOG_DIR}/certs"
PIDS_FILE="${LOG_DIR}/pids.txt"
REPORT_FILE="${LOG_DIR}/report.txt"
PASS=0
FAIL=0
SKIP=0

DRIP_BIN="${DRIP_BIN:-./bin/drip}"
SERVER_PORT="${SERVER_PORT:-18443}"
SERVER_DOMAIN="${SERVER_DOMAIN:-localhost}"
HTTP_BACKEND_PORT="${HTTP_BACKEND_PORT:-13000}"
TCP_BACKEND_PORT="${TCP_BACKEND_PORT:-13001}"
TCP_PORT_MIN="${TCP_PORT_MIN:-21000}"
TCP_PORT_MAX="${TCP_PORT_MAX:-21020}"
TOKEN="${TOKEN:-e2e-token}"
METRICS_TOKEN="${METRICS_TOKEN:-e2e-metrics}"

mkdir -p "$LOG_DIR" "$CERT_DIR"
: >"$PIDS_FILE"
: >"$REPORT_FILE"

log_info()  { echo -e "${GREEN}[INFO]${NC} $*" | tee -a "$REPORT_FILE" >&2; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $*" | tee -a "$REPORT_FILE" >&2; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" | tee -a "$REPORT_FILE" >&2; }
log_step()  { echo -e "\n${BLUE}==>${NC} $*\n" | tee -a "$REPORT_FILE" >&2; }

pass() { PASS=$((PASS + 1)); log_info "PASS: $*"; }
fail() { FAIL=$((FAIL + 1)); log_error "FAIL: $*"; }
skip() { SKIP=$((SKIP + 1)); log_warn "SKIP: $*"; }

track_pid() {
  echo "$1" >>"$PIDS_FILE"
}

cleanup() {
  log_step "Cleaning up e2e processes"
  if [[ -f "$PIDS_FILE" ]]; then
    while read -r pid; do
      [[ -n "$pid" ]] || continue
      if kill -0 "$pid" 2>/dev/null; then
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
      fi
    done <"$PIDS_FILE"
  fi
  # Best-effort sweep for leftover listeners from this run
  pkill -f "drip server --port ${SERVER_PORT}" 2>/dev/null || true
  pkill -f "drip http .*${HTTP_BACKEND_PORT}" 2>/dev/null || true
  pkill -f "drip tcp .*${TCP_BACKEND_PORT}" 2>/dev/null || true
  pkill -f "e2e-backend.py" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

wait_port() {
  local host="$1" port="$2" timeout="${3:-20}"
  python3 - "$host" "$port" "$timeout" <<'PY'
import socket, sys, time
host, port, timeout = sys.argv[1], int(sys.argv[2]), float(sys.argv[3])
deadline = time.time() + timeout
while time.time() < deadline:
    try:
        with socket.create_connection((host, port), 0.5):
            sys.exit(0)
    except OSError:
        time.sleep(0.1)
sys.exit(1)
PY
}

extract_url() {
  local logfile="$1"
  python3 - "$logfile" <<'PY'
import re, sys, pathlib
text = pathlib.Path(sys.argv[1]).read_text(errors="ignore")
text = re.sub(r"\x1b\[[0-9;]*[A-Za-z]", "", text)
# Prefer explicit tunnel URLs
for pat in [
    r"https://[a-zA-Z0-9.-]+:[0-9]+",
    r"https://[a-zA-Z0-9.-]+",
    r"tcp://[a-zA-Z0-9.-]+:[0-9]+",
]:
    m = re.search(pat, text)
    if m:
        print(m.group(0))
        sys.exit(0)
sys.exit(1)
PY
}

curl_tunnel() {
  local url="$1"; shift
  local hostport path
  # url like https://sub.localhost:18443/path
  hostport="$(python3 - "$url" <<'PY'
import sys
from urllib.parse import urlparse
u = urlparse(sys.argv[1])
print(f"{u.hostname}:{u.port or 443}")
PY
)"
  path="$(python3 - "$url" <<'PY'
import sys
from urllib.parse import urlparse
u = urlparse(sys.argv[1])
q = ("?" + u.query) if u.query else ""
print((u.path or "/") + q)
PY
)"
  # Rebuild request URL using Host via --resolve
  local scheme host port
  scheme="$(python3 - "$url" <<'PY'
import sys
from urllib.parse import urlparse
print(urlparse(sys.argv[1]).scheme)
PY
)"
  host="$(python3 - "$url" <<'PY'
import sys
from urllib.parse import urlparse
print(urlparse(sys.argv[1]).hostname)
PY
)"
  port="$(python3 - "$url" <<'PY'
import sys
from urllib.parse import urlparse
print(urlparse(sys.argv[1]).port or (443 if urlparse(sys.argv[1]).scheme=='https' else 80))
PY
)"
  local req_url="${scheme}://${host}:${port}${path}"
  curl -sk --resolve "${host}:${port}:127.0.0.1" "$@" "$req_url"
}

assert_http() {
  local name="$1" url="$2" expect_code="$3" expect_body="${4:-}"
  local tmp="${LOG_DIR}/assert-${name}.out"
  local code
  code="$(curl_tunnel "$url" -o "$tmp" -w '%{http_code}' --max-time 10 || true)"
  if [[ "$code" != "$expect_code" ]]; then
    fail "$name (status=$code want=$expect_code) body=$(head -c 200 "$tmp" 2>/dev/null || true)"
    return 1
  fi
  if [[ -n "$expect_body" ]]; then
    if ! grep -Fq "$expect_body" "$tmp"; then
      fail "$name (body missing '$expect_body'): $(head -c 200 "$tmp")"
      return 1
    fi
  fi
  pass "$name"
}

generate_certs() {
  log_step "Generating TLS certificates"
  openssl ecparam -name prime256v1 -genkey -noout -out "${CERT_DIR}/server.key" >/dev/null 2>&1
  openssl req -new -x509 \
    -key "${CERT_DIR}/server.key" \
    -out "${CERT_DIR}/server.crt" \
    -days 1 \
    -subj "/CN=localhost" >/dev/null 2>&1
  log_info "certs ready in ${CERT_DIR}"
}

start_backends() {
  log_step "Starting local backends"
  cat >"${LOG_DIR}/e2e-backend.py" <<'PY'
#!/usr/bin/env python3
import argparse, json, socket, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _read_body(self):
        n = int(self.headers.get("Content-Length", "0") or 0)
        return self.rfile.read(n) if n else b""

    def do_GET(self):
        if self.path.startswith("/sse"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "keep-alive")
            self.end_headers()
            self.wfile.write(b"data: first\n\n")
            self.wfile.flush()
            time.sleep(0.3)
            self.wfile.write(b"data: second\n\n")
            self.wfile.flush()
            return
        if self.path.startswith("/json"):
            body = json.dumps({"ok": True, "path": self.path}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path.startswith("/slow"):
            time.sleep(0.5)
        body = b"OK-HTTP"
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        data = self._read_body()
        body = b"ECHO:" + data
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

def serve_http(port):
    httpd = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    httpd.serve_forever()

def serve_tcp(port):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", port))
    s.listen(64)
    while True:
        c, _ = s.accept()
        def handle(conn):
            try:
                data = conn.recv(65536)
                if data:
                    conn.sendall(b"ECHO:" + data)
            finally:
                conn.close()
        threading.Thread(target=handle, args=(c,), daemon=True).start()

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--http-port", type=int, required=True)
    ap.add_argument("--tcp-port", type=int, required=True)
    args = ap.parse_args()
    threading.Thread(target=serve_http, args=(args.http_port,), daemon=True).start()
    serve_tcp(args.tcp_port)

if __name__ == "__main__":
    main()
PY
  python3 "${LOG_DIR}/e2e-backend.py" \
    --http-port "$HTTP_BACKEND_PORT" \
    --tcp-port "$TCP_BACKEND_PORT" \
    >"${LOG_DIR}/backend.log" 2>&1 &
  track_pid $!
  wait_port 127.0.0.1 "$HTTP_BACKEND_PORT" 10
  wait_port 127.0.0.1 "$TCP_BACKEND_PORT" 10
  log_info "backends up on ${HTTP_BACKEND_PORT}/${TCP_BACKEND_PORT}"
}

start_server() {
  log_step "Starting Drip server on :${SERVER_PORT}"
  "$DRIP_BIN" server \
    --port "$SERVER_PORT" \
    --domain "$SERVER_DOMAIN" \
    --tls-cert "${CERT_DIR}/server.crt" \
    --tls-key "${CERT_DIR}/server.key" \
    --tcp-port-min "$TCP_PORT_MIN" \
    --tcp-port-max "$TCP_PORT_MAX" \
    --token "$TOKEN" \
    --metrics-token "$METRICS_TOKEN" \
    --transports tcp,wss \
    --tunnel-types http,https,tcp \
    >"${LOG_DIR}/drip-server.log" 2>&1 &
  track_pid $!
  wait_port 127.0.0.1 "$SERVER_PORT" 15
  log_info "drip server ready"
}

CLIENT_PIDS=()

stop_clients() {
  local pid
  for pid in "${CLIENT_PIDS[@]:-}"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  CLIENT_PIDS=()
  # Give server a moment to unregister tunnels
  sleep 0.5
}

start_client() {
  local kind="$1" name="$2" port="$3"; shift 3
  local logfile="${LOG_DIR}/client-${name}.log"
  log_info "starting client ${name} (${kind} ${port})"
  "$DRIP_BIN" "$kind" "$port" \
    --server "localhost:${SERVER_PORT}" \
    --insecure \
    --token "$TOKEN" \
    "$@" \
    >"$logfile" 2>&1 &
  local pid=$!
  track_pid "$pid"
  CLIENT_PIDS+=("$pid")
  local url=""
  for _ in $(seq 1 40); do
    if url="$(extract_url "$logfile" 2>/dev/null)"; then
      break
    fi
    sleep 0.25
  done
  if [[ -z "${url:-}" ]]; then
    log_error "failed to get URL for ${name}; log:"
    sed 's/\x1b\[[0-9;]*[A-Za-z]//g' "$logfile" | head -80 | tee -a "$REPORT_FILE" >&2 || true
    return 1
  fi
  echo "$url"
}

tcp_echo() {
  local host="$1" port="$2" payload="$3"
  python3 - "$host" "$port" "$payload" <<'PY'
import socket, sys
host, port, payload = sys.argv[1], int(sys.argv[2]), sys.argv[3].encode()
with socket.create_connection((host, port), 5) as s:
    s.sendall(payload)
    data = s.recv(65536)
print(data.decode(errors="replace"))
PY
}

test_server_health() {
  log_step "Server health / discover / metrics"
  local code body
  code="$(curl -sk -o "${LOG_DIR}/health.out" -w '%{http_code}' "https://127.0.0.1:${SERVER_PORT}/health" || true)"
  if [[ "$code" == "200" ]]; then
    pass "health endpoint"
  else
    fail "health endpoint status=$code"
  fi

  code="$(curl -sk -o "${LOG_DIR}/discover.out" -w '%{http_code}' "https://127.0.0.1:${SERVER_PORT}/_drip/discover" || true)"
  if [[ "$code" == "200" ]] && grep -Eq 'tcp|wss' "${LOG_DIR}/discover.out"; then
    pass "discover endpoint"
  else
    fail "discover endpoint status=$code body=$(cat "${LOG_DIR}/discover.out" 2>/dev/null || true)"
  fi

  code="$(curl -sk -H "Host: ${SERVER_DOMAIN}" -o "${LOG_DIR}/metrics-unauth.out" -w '%{http_code}' "https://127.0.0.1:${SERVER_PORT}/metrics" || true)"
  if [[ "$code" == "401" || "$code" == "403" ]]; then
    pass "metrics rejects unauthenticated"
  else
    fail "metrics unauth status=$code"
  fi

  code="$(curl -sk -H "Host: ${SERVER_DOMAIN}" -o "${LOG_DIR}/metrics.out" -w '%{http_code}' \
    -H "Authorization: Bearer ${METRICS_TOKEN}" \
    "https://127.0.0.1:${SERVER_PORT}/metrics" || true)"
  if [[ "$code" == "200" ]]; then
    pass "metrics with token"
  else
    fail "metrics auth status=$code"
  fi
}

test_http_tcp_transport() {
  log_step "HTTP tunnel over TCP transport"
  local url
  url="$(start_client http http-tcp "$HTTP_BACKEND_PORT" -n e2ehttptcp --transport tcp)"
  assert_http "http-tcp GET" "$url/" 200 "OK-HTTP"
  assert_http "http-tcp JSON" "$url/json" 200 '"ok": true'

  local tmp="${LOG_DIR}/http-post.out"
  local code
  code="$(curl_tunnel "$url/echo" -o "$tmp" -w '%{http_code}' -X POST --data 'hello-e2e' --max-time 10 || true)"
  if [[ "$code" == "200" ]] && grep -Fq "ECHO:hello-e2e" "$tmp"; then
    pass "http-tcp POST echo"
  else
    fail "http-tcp POST status=$code body=$(head -c 200 "$tmp")"
  fi
  stop_clients
}

test_http_wss_transport() {
  log_step "HTTP tunnel over WSS transport"
  local url
  url="$(start_client http http-wss "$HTTP_BACKEND_PORT" -n e2ehttpwss --transport wss)"
  assert_http "http-wss GET" "$url/" 200 "OK-HTTP"
  stop_clients
}

test_sse() {
  log_step "SSE streaming through HTTP tunnel"
  local url
  url="$(start_client http http-sse "$HTTP_BACKEND_PORT" -n e2esse --transport tcp)"
  local tmp="${LOG_DIR}/sse.out"
  curl_tunnel "$url/sse" --max-time 5 -N -o "$tmp" >/dev/null 2>&1 || true
  if grep -Fq "data: first" "$tmp" && grep -Fq "data: second" "$tmp"; then
    pass "sse events streamed"
  else
    fail "sse body=$(head -c 300 "$tmp" 2>/dev/null || true)"
  fi
  stop_clients
}

test_proxy_auth_password() {
  log_step "HTTP proxy password auth"
  local url
  url="$(start_client http http-auth "$HTTP_BACKEND_PORT" -n e2eauth --transport tcp --auth secretpass)"
  local tmp="${LOG_DIR}/auth-blocked.out"
  local code
  code="$(curl_tunnel "$url/" -o "$tmp" -w '%{http_code}' --max-time 10 || true)"
  if [[ "$code" == "401" ]] && grep -Eiq 'password|login|drip' "$tmp" && ! grep -Fq "OK-HTTP" "$tmp"; then
    pass "password auth shows login page"
  else
    fail "password auth gate status=$code body=$(head -c 200 "$tmp")"
  fi

  local cookie_jar="${LOG_DIR}/auth.cookies"
  code="$(curl_tunnel "$url/_drip/login" -o "${LOG_DIR}/login.out" -w '%{http_code}' \
    -c "$cookie_jar" -b "$cookie_jar" \
    -X POST --data "password=secretpass&redirect=/" --max-time 10 || true)"
  if [[ "$code" != "303" && "$code" != "302" ]]; then
    log_warn "login status=$code (expected redirect; continuing)"
  fi

  code="$(curl_tunnel "$url/" -o "${LOG_DIR}/authed.out" -w '%{http_code}' \
    -b "$cookie_jar" -c "$cookie_jar" --max-time 10 || true)"
  if [[ "$code" == "200" ]] && grep -Fq "OK-HTTP" "${LOG_DIR}/authed.out"; then
    pass "password auth session works"
  else
    fail "password auth after login status=$code body=$(head -c 200 "${LOG_DIR}/authed.out" 2>/dev/null || true) cookies=$(cat "$cookie_jar" 2>/dev/null || true)"
  fi
  stop_clients
}

test_proxy_auth_bearer() {
  log_step "HTTP proxy bearer auth"
  local url
  url="$(start_client http http-bearer "$HTTP_BACKEND_PORT" -n e2ebearer --transport tcp --auth-bearer sk-e2e-secret)"
  assert_http "bearer blocks missing token" "$url/" 401 ""
  local tmp="${LOG_DIR}/bearer.out"
  local code
  code="$(curl_tunnel "$url/" -o "$tmp" -w '%{http_code}' \
    -H "Authorization: Bearer sk-e2e-secret" --max-time 10 || true)"
  if [[ "$code" == "200" ]] && grep -Fq "OK-HTTP" "$tmp"; then
    pass "bearer auth works"
  else
    fail "bearer auth status=$code body=$(head -c 200 "$tmp")"
  fi
  stop_clients
}

test_tcp_tunnel() {
  log_step "TCP tunnel over TCP transport"
  local url host port
  url="$(start_client tcp tcp-plain "$TCP_BACKEND_PORT" -n e2etcp --transport tcp)"
  host="$(python3 - "$url" <<'PY'
import sys
from urllib.parse import urlparse
u=urlparse(sys.argv[1])
print(u.hostname)
PY
)"
  port="$(python3 - "$url" <<'PY'
import sys
from urllib.parse import urlparse
u=urlparse(sys.argv[1])
print(u.port)
PY
)"
  local resp
  resp="$(tcp_echo 127.0.0.1 "$port" "hello-tcp" || true)"
  if [[ "$resp" == "ECHO:hello-tcp" ]]; then
    pass "tcp echo via ${host}:${port}"
  else
    fail "tcp echo got='$resp'"
  fi
  stop_clients
}

test_tcp_wss_tunnel() {
  log_step "TCP tunnel over WSS transport"
  local url port resp
  url="$(start_client tcp tcp-wss "$TCP_BACKEND_PORT" -n e2etcpwss --transport wss)"
  port="$(python3 - "$url" <<'PY'
import sys
from urllib.parse import urlparse
print(urlparse(sys.argv[1]).port)
PY
)"
  resp="$(tcp_echo 127.0.0.1 "$port" "hello-wss" || true)"
  if [[ "$resp" == "ECHO:hello-wss" ]]; then
    pass "tcp-wss echo via :${port}"
  else
    fail "tcp-wss echo got='$resp'"
  fi
  stop_clients
}

test_concurrency() {
  log_step "Concurrent HTTP requests through tunnel"
  local url
  url="$(start_client http http-conc "$HTTP_BACKEND_PORT" -n e2econc --transport tcp)"

  local ok
  ok="$(python3 - "$url" <<'PY'
import concurrent.futures, subprocess, sys
from urllib.parse import urlparse

url = sys.argv[1]
u = urlparse(url if url.endswith("/") else url + "/")
host = u.hostname
port = u.port or 443
target = f"{u.scheme}://{host}:{port}/"
resolve = f"{host}:{port}:127.0.0.1"

def one(_):
    try:
        p = subprocess.run(
            ["curl", "-sk", "--resolve", resolve, "--max-time", "8", "-o", "/dev/null", "-w", "%{http_code}", target],
            capture_output=True, text=True, timeout=12,
        )
        return p.stdout.strip() == "200"
    except Exception:
        return False

with concurrent.futures.ThreadPoolExecutor(max_workers=20) as ex:
    results = list(ex.map(one, range(40)))
print(sum(1 for r in results if r))
PY
)"
  if [[ "$ok" -eq 40 ]]; then
    pass "concurrency 40/40 parallel requests"
  else
    fail "concurrency only ${ok}/40 succeeded"
  fi
  stop_clients
}

test_invalid_token() {
  log_step "Reject invalid client token"
  local logfile="${LOG_DIR}/client-badtoken.log"
  "$DRIP_BIN" http "$HTTP_BACKEND_PORT" \
    --server "localhost:${SERVER_PORT}" \
    --insecure \
    --token "wrong-token" \
    -n e2ebad \
    --transport tcp \
    >"$logfile" 2>&1 &
  local pid=$!
  track_pid "$pid"
  CLIENT_PIDS+=("$pid")
  sleep 4
  if extract_url "$logfile" >/dev/null 2>&1; then
    fail "invalid token unexpectedly connected"
  else
    if grep -Eiq 'auth|token|unauthorized|denied|failed|error' "$logfile"; then
      pass "invalid token rejected"
    else
      if ! kill -0 "$pid" 2>/dev/null || grep -Eiq 'failed|error|Configuration' "$logfile"; then
        pass "invalid token did not establish tunnel"
      else
        fail "invalid token unclear; log=$(head -c 300 "$logfile")"
      fi
    fi
  fi
  stop_clients
}

main() {
  echo "=========================================" | tee -a "$REPORT_FILE"
  echo "  Drip Full E2E Test" | tee -a "$REPORT_FILE"
  echo "  log: $LOG_DIR" | tee -a "$REPORT_FILE"
  echo "=========================================" | tee -a "$REPORT_FILE"

  if [[ ! -x "$DRIP_BIN" ]]; then
    log_step "Building drip binary"
    make build
  fi

  generate_certs
  start_backends
  start_server

  test_server_health
  test_http_tcp_transport
  test_http_wss_transport
  test_sse
  test_proxy_auth_password
  test_proxy_auth_bearer
  test_tcp_tunnel
  test_tcp_wss_tunnel
  test_concurrency
  test_invalid_token

  echo "" | tee -a "$REPORT_FILE"
  echo "=========================================" | tee -a "$REPORT_FILE"
  echo "  Results: PASS=$PASS FAIL=$FAIL SKIP=$SKIP" | tee -a "$REPORT_FILE"
  echo "  Report: $REPORT_FILE" | tee -a "$REPORT_FILE"
  echo "=========================================" | tee -a "$REPORT_FILE"

  if [[ "$FAIL" -gt 0 ]]; then
    log_error "E2E failed"
    exit 1
  fi
  log_info "E2E passed"
}

main "$@"
