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

# update.sh creates .env, picks a free port, and builds and starts Cotta.
exec sh "$DIR/update.sh" --skip-pull
