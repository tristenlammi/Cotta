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

# --- data folder ---
# Cotta's database lives in ./data, next to this script. The container does
# not run as root, so the folder must belong to the user it runs as.
mkdir -p data
if [ "$(id -u)" = 0 ]; then
	OWNER="65532:65532" # the image's built-in unprivileged user
	chown -R "$OWNER" data
else
	OWNER="$(id -u):$(id -g)" # run the container as you, so you own the files
	set_setting COTTA_UID "$(id -u)"
	set_setting COTTA_GID "$(id -g)"
fi
chmod 700 data

# One-time move for installs that kept data in a Docker volume.
if [ ! -f data/cotta.db ]; then
	OLD_VOLUME="$(docker volume ls -q --filter label=com.docker.compose.volume=cotta-data 2>/dev/null | head -n 1)"
	if [ -n "$OLD_VOLUME" ]; then
		say "Moving your data out of the Docker volume into $(pwd)/data"
		docker compose down >/dev/null 2>&1 || true # stop Cotta so the database is at rest
		docker run --rm -v "$OLD_VOLUME":/from:ro -v "$(pwd)/data":/to alpine \
			sh -c "cp -a /from/. /to/ && chown -R $OWNER /to" ||
			die "Couldn't copy the data. Nothing was deleted; the volume '$OLD_VOLUME' is untouched."
		echo "Done. The old volume '$OLD_VOLUME' was left in place as a backup."
		echo "Once you've checked Cotta works, remove it with: docker volume rm $OLD_VOLUME"
	fi
fi

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

FIRST_PORT=8230

# Print the first unused port, counting up from FIRST_PORT.
free_port() {
	port="$FIRST_PORT"
	while [ "$port" -lt $((FIRST_PORT + 100)) ] && port_in_use "$port"; do
		port=$((port + 1))
	done
	echo "$port"
}

# One-time change for early installs, recognised by a comment line their
# .env was created with: those answered on this machine only and counted
# ports up from 8080. Move them to the current defaults.
OLD_NOTE='# Where Cotta listens on the Docker host. 127.0.0.1 = this machine only.'
if grep -qxF "$OLD_NOTE" .env; then
	if [ "$(setting COTTA_BIND)" = "127.0.0.1" ] && [ -z "$(setting TUNNEL_TOKEN)" ]; then
		set_setting COTTA_BIND 0.0.0.0
		echo "Cotta will now answer on this machine's network address, not just localhost."
	fi
	set_setting COTTA_PORT "" # re-picked below, from $FIRST_PORT
	grep -vxF "$OLD_NOTE" .env > .env.tmp || true
	cat .env.tmp > .env
	rm -f .env.tmp
fi

PORT="$(setting COTTA_PORT)"
if [ -z "$PORT" ]; then
	PORT="$(free_port)"
	set_setting COTTA_PORT "$PORT"
	echo "Cotta will use port $PORT (saved in .env)."
elif [ -z "$(docker compose ps --status running -q cotta 2>/dev/null)" ] && port_in_use "$PORT"; then
	# Cotta isn't running, so something else holds its port: move.
	TAKEN="$PORT"
	PORT="$(free_port)"
	set_setting COTTA_PORT "$PORT"
	echo "Port $TAKEN is already in use on this machine, so Cotta will use $PORT (saved in .env)."
fi

# --- start ---
say "Building and starting Cotta"
if [ -n "$(setting TUNNEL_TOKEN)" ]; then
	docker compose --profile tunnel up -d --build
else
	docker compose up -d --build
fi

BIND="$(setting COTTA_BIND)"
BIND="${BIND:-0.0.0.0}"

# This machine's address on the local network, if we can tell.
lan_ip() {
	ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -n 1
}

say "Cotta is running."
if [ "$BIND" = "127.0.0.1" ]; then
	echo "Open http://localhost:$PORT on this machine and enter your master passphrase."
	echo
	echo "It only answers on this machine. To reach it from your network, set"
	echo "COTTA_BIND=0.0.0.0 in .env here and run ./update.sh again."
else
	HOST_IP="$(lan_ip)"
	[ -n "$HOST_IP" ] || HOST_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
	echo "Open http://${HOST_IP:-<this machine's address>}:$PORT and enter your master passphrase."
	echo
	echo "Anyone on your network can reach that sign-in page. Don't forward this"
	echo "port from the internet; use a Cloudflare Tunnel for access from outside."
fi
echo
echo "Your data lives in $(pwd)/data — back that folder up. To stop: docker compose down"
