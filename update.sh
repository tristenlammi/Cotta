#!/bin/sh
# Update Cotta to the latest version and restart it.
#
#   cd ~/cotta && ./update.sh
#
# Restarting ends any running SSH sessions. Your servers, keys and saved
# commands are kept.
set -eu

cd "$(dirname "$0")"

say() { printf '\n==> %s\n' "$1"; }
die() { printf '\nError: %s\n' "$1" >&2; exit 1; }

[ -d .git ] || die "This folder isn't a Cotta checkout. Run install.sh instead."
docker info >/dev/null 2>&1 || die "Can't talk to Docker. Is it running?"

say "Fetching the latest version"
before="$(git rev-parse HEAD)"
git pull --ff-only || die "Couldn't update cleanly. If you edited files here, undo those changes and try again."
if [ "$before" = "$(git rev-parse HEAD)" ]; then
	echo "Already up to date."
fi

[ -f .env ] || { cp .env.example .env; chmod 600 .env; }

say "Rebuilding and restarting Cotta"
if [ -n "$(sed -n 's/^TUNNEL_TOKEN=//p' .env | tail -n 1)" ]; then
	docker compose --profile tunnel up -d --build
else
	docker compose up -d --build
fi

say "Cotta is up to date. Unlock it again with your master passphrase."
