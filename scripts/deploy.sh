#!/usr/bin/env bash
# Deploy the currently tagged commit to production.
#
# Run from anywhere inside the repo, on the server, as the deploy user:
#   scripts/deploy.sh
#
# GitHub is the source of truth: the server only pulls, it never commits,
# tags or pushes. Tag releases on a dev machine and push them from there.
#
# VERSION is the single source for both the build tag (ferri:$VERSION) and
# the tag pinned in docker-compose.yml, so the two cannot drift apart.
#
# Refuses to deploy an untagged commit — tag first, on your dev machine:
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
	echo "ERROR: HEAD is not an exact tag. Tag this release on your dev machine:" >&2
	echo "  git tag vX.Y.Z && git push --tags" >&2
	exit 1
}

echo "==> Deploying $VERSION"
# --pull: fetch the base images fresh, so security fixes in the runtime image
# arrive without a manual docker pull.
docker build --pull --build-arg VERSION="$VERSION" -t "ferri:$VERSION" -t ferri:latest .

# A restart breaks off running downloads and uploads. Waiting inside the
# shutdown is no answer: a stopping app takes no new requests, so the site
# would be down for as long as the slowest download. So wait here, while the
# running version keeps serving, until nothing is in flight: no open
# connection from the reverse proxy to the app at two checks in a row. The
# restart then takes seconds; browsers retry uploads for about 8.5 minutes and
# resume loose files. A proxy that keeps idle connections open
# (upstream keepalive) holds the count up until it closes them.
# WAIT_MAX caps the wait in minutes (default 180), FORCE=1 skips it,
# APP_ADDR is where the proxy reaches the app (default 127.0.0.1:8080).
if [[ "${FORCE:-}" != 1 ]]; then
	command -v ss >/dev/null || {
		echo "ERROR: ss (iproute2) not found; install it or run with FORCE=1" >&2
		exit 1
	}
	app_addr="${APP_ADDR:-127.0.0.1:8080}"
	wait_max="${WAIT_MAX:-180}"
	started=$SECONDS
	quiet=0
	last=-1
	next_report=0
	while :; do
		n=$(ss -Htn state established "dst $app_addr" | wc -l)
		n=$((n))
		if [[ $n -eq 0 ]]; then
			quiet=$((quiet + 1))
			[[ $quiet -ge 2 ]] && break
		else
			quiet=0
		fi
		waited=$((SECONDS - started))
		if [[ $waited -ge $((wait_max * 60)) ]]; then
			echo "Still busy after $wait_max minutes. Nothing was changed; the new image ferri:$VERSION is built." >&2
			echo "Run again later, with a larger WAIT_MAX, or with FORCE=1." >&2
			exit 1
		fi
		# Report on every change, and every 5 minutes while nothing changes.
		if [[ $n -ne $last || $waited -ge $next_report ]]; then
			echo "==> $n active connection(s), waiting for a quiet moment ($((waited / 60)) min so far; Ctrl-C aborts, nothing changed yet)"
			[[ $last -lt 0 ]] && echo "    What they are: \"Now\" at the top of /admin (idle = keep-alive, nothing running)"
			last=$n
			next_report=$((waited + 300))
		fi
		sleep 30
	done
fi

# Only the live compose file is pinned. The repo copy is never touched, so
# the checkout stays identical to GitHub and the next `git pull` can't
# conflict with a local change.
echo "==> Pinning $LIVE_COMPOSE to ferri:$VERSION"
sed -i.bak -E "s#^(\s*image:\s*ferri:).*#\1$VERSION#" "$LIVE_COMPOSE"
rm -f "$LIVE_COMPOSE.bak"

echo "==> Restarting"
cd "$DEPLOY_DIR"
docker compose down
docker compose up -d
docker compose logs --tail=20
