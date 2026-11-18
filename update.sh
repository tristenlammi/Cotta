#!/bin/sh
# Update Cotta to the latest version and (re)start it.
#
#   cd ~/cotta && ./update.sh
#
# Restarting ends any running SSH sessions. Your servers, keys and saved
# commands are kept. Also run this after editing .env.
set -eu

cd "$(dirname "$0")"

say() { printf '\n==> %s\n' "$1"; }
die() { printf '\nError: %s\n' "$1" >&2; exit 1; }

[ -d .git ] || die "This folder isn't a Cotta checkout. Run install.sh instead."
docker info >/dev/null 2>&1 || die "Can't talk to Docker. Is it running?"

if [ "${1:-}" != "--skip-pull" ]; then
	say "Fetching the latest version"
	before="$(git rev-parse HEAD)"
	git pull --ff-only || die "Couldn't update cleanly. If you edited files here, undo those changes and try again."
	if [ "$before" != "$(git rev-parse HEAD)" ]; then
		# This script may itself have changed; continue with the new copy.
		exec sh ./update.sh --skip-pull
	fi
	echo "Already up to date."
fi

# --- settings in .env ---
[ -f .env ] || { cp .env.example .env; chmod 600 .env; }

# Read a value from .env without executing the file.
setting() { sed -n "s/^$1=//p" .env | tail -n 1; }

# Set KEY to VALUE in .env, replacing an existing line or adding one.
set_setting() {
	if grep -q "^$1=" .env; then
		sed "s/^$1=.*/$1=$2/" .env > .env.tmp
		cat .env.tmp > .env
		rm -f .env.tmp
	else
		printf '%s=%s\n' "$1" "$2" >> .env
	fi
}

# --- port picking ---
# True if something on this machine is already listening on TCP port $1.
port_in_use() {
	if command -v ss >/dev/null 2>&1; then
		ss -Hltn 2>/dev/null | awk '{print $4}' | grep -q "[:.]$1\$"
	elif command -v netstat >/dev/null 2>&1; then
		netstat -an 2>/dev/null | grep -i listen | awk '{print $4}' | grep -q "[:.]$1\$"
	elif command -v nc >/dev/null 2>&1; then
		nc -z 127.0.0.1 "$1" >/dev/null 2>&1
	else
		return 1 # no way to check; assume it is free
	fi
}

# Print the first unused port, counting up from $1.
free_port() {
	port="$1"
	while [ "$port" -lt 8180 ] && port_in_use "$port"; do
		port=$((port + 1))
	done
	echo "$port"
}

PORT="$(setting COTTA_PORT)"
PORT="${PORT:-8080}"
# If Cotta is already running, it is the one holding its port: keep it.
# Otherwise make sure the port is really free, and move up if it isn't.
if [ -z "$(docker compose ps --status running -q cotta 2>/dev/null)" ] && port_in_use "$PORT"; then
	TAKEN="$PORT"
	PORT="$(free_port 8080)"
	set_setting COTTA_PORT "$PORT"
	echo "Port $TAKEN is already in use on this machine, so Cotta will use $PORT (saved in .env)."
elif ! grep -q "^COTTA_PORT=" .env; then
	set_setting COTTA_PORT "$PORT"
fi

# --- start ---
say "Building and starting Cotta"
if [ -n "$(setting TUNNEL_TOKEN)" ]; then
	docker compose --profile tunnel up -d --build
else
	docker compose up -d --build
fi

BIND="$(setting COTTA_BIND)"
BIND="${BIND:-127.0.0.1}"

say "Cotta is running."
if [ "$BIND" = "127.0.0.1" ]; then
	echo "Open http://localhost:$PORT on this machine and enter your master passphrase."
	echo
	echo "It only answers on this machine. To reach it from elsewhere, edit .env here:"
	echo "  - set TUNNEL_TOKEN (and the COTTA_CF_* values) to use a Cloudflare Tunnel, or"
	echo "  - set COTTA_BIND=0.0.0.0 to open it to your local network (trusted networks only)."
	echo "Then run ./update.sh again."
else
	echo "Open http://<this machine's address>:$PORT and enter your master passphrase."
fi
echo
echo "Your data lives in the Docker volume 'cotta-data'. To stop: docker compose down"
