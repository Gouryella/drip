#!/usr/bin/env bash
#
# Bring up a complete Drip deployment on this machine: server, control plane,
# admin panel, two tunnel clients bound to reserved names, and local backends
# for them to forward to.
#
#   bash scripts/dev/local-demo.sh          # start everything
#   bash scripts/dev/local-demo.sh stop     # stop everything
#
# Everything lives under .local-demo/ and is safe to delete. The clients run
# with HOME pointed inside that directory, so your real ~/.drip/config.yaml is
# never read or written.

set -euo pipefail

cd "$(dirname "$0")/../.."
ROOT="$(pwd)"
DEMO="$ROOT/.local-demo"

TUNNEL_PORT=18450
ADMIN_PORT=8444
TCP_MIN=33000
TCP_MAX=33020
BACKEND_A=19001
BACKEND_B=19002
DOMAIN="local.test"

ADMIN_USER="operator"
ADMIN_PASS="patch-panel-demo-2026"

DRIP="$DEMO/drip"
DB="$DEMO/control.db"

stop_all() {
  for pidfile in "$DEMO"/*.pid; do
    [ -e "$pidfile" ] || continue
    kill "$(cat "$pidfile")" 2>/dev/null || true
    rm -f "$pidfile"
  done
  echo "stopped"
}

if [ "${1:-start}" = "stop" ]; then
  stop_all
  exit 0
fi

mkdir -p "$DEMO"
stop_all >/dev/null 2>&1 || true
rm -f "$DB" "$DB-wal" "$DB-shm"

echo "==> building"
go build -o "$DRIP" ./cmd/drip

# The client always dials TLS, so a local server needs a certificate even though
# nothing here is public. The clients pass -k to skip verification.
if [ ! -f "$DEMO/cert.pem" ]; then
  echo "==> generating a self-signed certificate"
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
    -keyout "$DEMO/key.pem" -out "$DEMO/cert.pem" \
    -subj "/CN=$DOMAIN" -addext "subjectAltName=DNS:$DOMAIN,DNS:*.$DOMAIN,IP:127.0.0.1" 2>/dev/null
fi

# One account is the normal case for a self-hosted deployment; the panel hides
# the concept entirely at this size. Add a second to see the multi-tenant view.
echo "==> seeding the control plane"
"$DRIP" admin --db "$DB" account create default >/dev/null

issue() { # issue <account> <name> -> prints the token
  "$DRIP" admin --db "$DB" client create --account "$1" "$2" 2>/dev/null | awk '/token:/{print $2}'
}

TOKEN_A="$(issue default edge-01)"
TOKEN_B="$(issue default kiosk-02)"
ID_A="$(echo "$TOKEN_A" | cut -d_ -f2)"
ID_B="$(echo "$TOKEN_B" | cut -d_ -f2)"

"$DRIP" admin --db "$DB" reservation create --account default --subdomain billing --client "$ID_A" >/dev/null
"$DRIP" admin --db "$DB" reservation create --account default --subdomain lobby-display --client "$ID_B" >/dev/null
# An allocation with no client connected, so the field shows a dark port too.
"$DRIP" admin --db "$DB" reservation create --account default --subdomain warehouse >/dev/null
"$DRIP" admin --db "$DB" reservation create --account default --tcp-port 20050 >/dev/null

echo "==> starting the server"
"$DRIP" server \
  --db "$DB" \
  --admin "127.0.0.1:$ADMIN_PORT" \
  --tls-cert "$DEMO/cert.pem" --tls-key "$DEMO/key.pem" \
  -d "$DOMAIN" -p "$TUNNEL_PORT" \
  --tcp-port-min "$TCP_MIN" --tcp-port-max "$TCP_MAX" \
  > "$DEMO/server.log" 2>&1 &
echo $! > "$DEMO/server.pid"
sleep 2

echo "==> starting local backends"
python3 -m http.server "$BACKEND_A" --bind 127.0.0.1 > /dev/null 2>&1 &
echo $! > "$DEMO/backend-a.pid"
python3 -m http.server "$BACKEND_B" --bind 127.0.0.1 > /dev/null 2>&1 &
echo $! > "$DEMO/backend-b.pid"

# Each client gets its own HOME so it reads its own ~/.drip/config.yaml and
# never touches yours.
client_home() { # client_home <name> <token>
  local home="$DEMO/$1"
  mkdir -p "$home/.drip"
  printf 'server: 127.0.0.1:%s\ntoken: %s\ntls: true\n' "$TUNNEL_PORT" "$2" > "$home/.drip/config.yaml"
  echo "$home"
}

HOME_A="$(client_home edge-01 "$TOKEN_A")"
HOME_B="$(client_home kiosk-02 "$TOKEN_B")"

echo "==> connecting clients"
HOME="$HOME_A" "$DRIP" http "$BACKEND_A" --transport tcp -k > "$DEMO/client-a.log" 2>&1 &
echo $! > "$DEMO/client-a.pid"
HOME="$HOME_B" "$DRIP" http "$BACKEND_B" --transport tcp -k > "$DEMO/client-b.log" 2>&1 &
echo $! > "$DEMO/client-b.pid"
sleep 4

# Put some traffic through so the counters are not all zero.
for _ in 1 2 3 4 5; do
  curl -sk -H "Host: billing.$DOMAIN" "https://127.0.0.1:$TUNNEL_PORT/" -o /dev/null || true
  curl -sk -H "Host: lobby-display.$DOMAIN" "https://127.0.0.1:$TUNNEL_PORT/" -o /dev/null || true
done

cat <<INFO

  Admin panel   http://127.0.0.1:$ADMIN_PORT
  Sign in       $ADMIN_USER / $ADMIN_PASS
                (create this account on the first-run screen)

  Live tunnels  curl -k -H "Host: billing.$DOMAIN" https://127.0.0.1:$TUNNEL_PORT/
                curl -k -H "Host: lobby-display.$DOMAIN" https://127.0.0.1:$TUNNEL_PORT/

  Logs          $DEMO/server.log, client-a.log, client-b.log
  CLI           $DRIP admin --db $DB reservation list

  Stop          bash scripts/dev/local-demo.sh stop

INFO
