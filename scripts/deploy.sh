#!/usr/bin/env bash
# Deploy the currently tagged commit to production.
#
# Run from anywhere inside the repo, on the server, as the deploy user:
#   scripts/deploy.sh
#
# What it fixes: the build tag (ferri:$VERSION) and the tag pinned in
# docker-compose.yml used to be set by hand, in two separate steps, on two
# different files. They drifted apart three times in a row — most recently
# 2026-09-24, where docker-compose.yml stayed on ferri:v1.1.0 while `docker
# build` produced ferri:latest, so `docker compose up -d` silently kept
# running the old image. This script makes VERSION the single source for
# both the build tag and the compose pin, so they cannot diverge again.
#
# Refuses to deploy an untagged commit — tag first:
#   git tag vX.Y.Z && git push --tags

set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY_DIR="$(dirname "$REPO_DIR")"
LIVE_COMPOSE="$DEPLOY_DIR/docker-compose.yml"

if [[ ! -f "$LIVE_COMPOSE" ]]; then
	echo "ERROR: $LIVE_COMPOSE not found — this script expects to run from a" >&2
	echo "checkout at <deploy-dir>/src, next to <deploy-dir>/docker-compose.yml" >&2
	exit 1
fi

cd "$REPO_DIR"
git pull

VERSION=$(git describe --tags --exact-match 2>/dev/null) || {
	echo "ERROR: HEAD is not an exact tag. Tag this release first:" >&2
	echo "  git tag vX.Y.Z && git push --tags" >&2
	exit 1
}

echo "==> Deploying $VERSION"
docker build --build-arg VERSION="$VERSION" -t "ferri:$VERSION" -t ferri:latest .

echo "==> Pinning docker-compose.yml (repo + live) to ferri:$VERSION"
for f in "$REPO_DIR/docker-compose.yml" "$LIVE_COMPOSE"; do
	sed -i.bak -E "s#^(\s*image:\s*ferri:).*#\1$VERSION#" "$f"
	rm -f "$f.bak"
done

if ! git diff --quiet -- docker-compose.yml; then
	git add docker-compose.yml
	git commit -m "Pin deployment to $VERSION"
	git push
fi

echo "==> Restarting"
cd "$DEPLOY_DIR"
docker compose down
docker compose up -d
docker compose logs --tail=20
