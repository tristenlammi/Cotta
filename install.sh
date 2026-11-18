#!/bin/sh
# Cotta installer. To update later: cd ~/cotta && ./update.sh
#
#   curl -fsSL https://raw.githubusercontent.com/tristenlammi/Cotta/main/install.sh | sh
#
# Needs git and Docker (with the compose plugin). Installs into ~/cotta;
# set COTTA_DIR to use another folder.
set -eu

REPO="https://github.com/tristenlammi/Cotta.git"
DIR="${COTTA_DIR:-$HOME/cotta}"

say() { printf '\n==> %s\n' "$1"; }
die() { printf '\nError: %s\n' "$1" >&2; exit 1; }

command -v git >/dev/null 2>&1 || die "git is not installed."
command -v docker >/dev/null 2>&1 || die "Docker is not installed. See https://docs.docker.com/engine/install/"
docker compose version >/dev/null 2>&1 || die "The Docker compose plugin is missing. See https://docs.docker.com/compose/install/"
docker info >/dev/null 2>&1 || die "Can't talk to Docker. Is it running, and is your user allowed to use it?"

if [ -d "$DIR/.git" ]; then
	say "Updating Cotta in $DIR"
	git -C "$DIR" pull --ff-only
else
	say "Downloading Cotta into $DIR"
	git clone --depth 1 "$REPO" "$DIR"
fi
cd "$DIR"

# Read a value from .env without executing the file.
setting() { sed -n "s/^$1=//p" .env | tail -n 1; }

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

# Print the first unused port, counting up from 8080.
free_port() {
	port=8080
	while [ "$port" -lt 8180 ] && port_in_use "$port"; do
		port=$((port + 1))
	done
	echo "$port"
}
# --- end port picking ---

# First install: create .env and choose a port. Later runs keep the port
# already in .env, since by then Cotta itself is the one using it.
if [ ! -f .env ]; then
	PICKED="$(free_port)"
	sed "s/^COTTA_PORT=.*/COTTA_PORT=$PICKED/" .env.example > .env
	chmod 600 .env
	[ "$PICKED" = 8080 ] || echo "Port 8080 is taken on this machine, so Cotta will use $PICKED."
fi

if [ -n "$(setting TUNNEL_TOKEN)" ]; then
	say "Building and starting Cotta with the Cloudflare Tunnel"
	docker compose --profile tunnel up -d --build
else
	say "Building and starting Cotta"
	docker compose up -d --build
fi

BIND="$(setting COTTA_BIND)"
PORT="$(setting COTTA_PORT)"
BIND="${BIND:-127.0.0.1}"
PORT="${PORT:-8080}"

say "Cotta is running."
if [ "$BIND" = "127.0.0.1" ]; then
	echo "Open http://localhost:$PORT on this machine and choose a master passphrase."
	echo
	echo "It only answers on this machine. To reach it from elsewhere, edit $DIR/.env:"
	echo "  - set TUNNEL_TOKEN (and the COTTA_CF_* values) to use a Cloudflare Tunnel, or"
	echo "  - set COTTA_BIND=0.0.0.0 to open it to your local network (trusted networks only)."
	echo "Then run: cd $DIR && ./update.sh"
else
	echo "Open http://<this machine's address>:$PORT and choose a master passphrase."
fi
echo
echo "Your data lives in the Docker volume 'cotta-data'. To stop: cd $DIR && docker compose down"
