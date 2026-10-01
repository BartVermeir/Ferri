#!/usr/bin/env bash
# Deploy a release tag to production.
#
# Run from anywhere inside the repo, on the server, as the deploy user:
#   scripts/deploy.sh            # the highest release tag on GitHub
#   scripts/deploy.sh vX.Y.Z     # that tag
#
# GitHub is the source of truth: the server only fetches, it never commits,
# tags or pushes. A release is a tag, pushed from a dev machine; commits on
# main without a tag are never deployed. The checkout ends up on the tag
# (detached HEAD), not on a branch.
#
# VERSION is the single source for both the build tag (ferri:$VERSION) and
# the tag pinned in docker-compose.yml, so the two cannot drift apart.
#
# If the new version does not become healthy, the previous one is put back.
# Exit codes: 0 deployed, 1 error (nothing changed), 3 still busy after
# WAIT_MAX (nothing changed), 4 new version unhealthy and rolled back,
# 5 rollback unhealthy too: Ferri is down.

set -euo pipefail

# Everything runs from main: the checkout below rewrites this file, and
# bash reads a script as it goes. A function is read in full before it runs.
main() {
	REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
	DEPLOY_DIR="$(dirname "$REPO_DIR")"
	LIVE_COMPOSE="$DEPLOY_DIR/docker-compose.yml"

	if [[ ! -f "$LIVE_COMPOSE" ]]; then
		echo "ERROR: $LIVE_COMPOSE not found — this script expects to run from a" >&2
		echo "checkout at <deploy-dir>/src, next to <deploy-dir>/docker-compose.yml" >&2
		exit 1
	fi
	# Names only: the values must never end up on screen.
	for key in ADMIN_TOKEN SMTP_PASSWORD; do
		if ! grep -q "^$key=." "$DEPLOY_DIR/.env" 2>/dev/null; then
			echo "ERROR: $key is missing or empty in $DEPLOY_DIR/.env. Nothing changed." >&2
			exit 1
		fi
	done

	cd "$REPO_DIR"
	git fetch --quiet --tags --force origin
	VERSION="${1:-$(latest_tag)}"
	if [[ -z "$VERSION" ]] || ! git rev-parse -q --verify "refs/tags/$VERSION^{commit}" >/dev/null; then
		echo "ERROR: no release tag '${VERSION}'. Tag it on your dev machine:" >&2
		echo "  git tag vX.Y.Z && git push origin vX.Y.Z" >&2
		exit 1
	fi
	git -c advice.detachedHead=false checkout --quiet "refs/tags/$VERSION"

	PREV="$(running_version "$LIVE_COMPOSE")"
	if [[ -z "$PREV" ]]; then
		echo "ERROR: no image: ferri:… line in $LIVE_COMPOSE. Nothing changed." >&2
		exit 1
	fi

	echo "==> Deploying $VERSION (running: $PREV)"
	# --pull: fetch the base images fresh, so security fixes in the runtime image
	# arrive without a manual docker pull.
	docker build --pull --build-arg VERSION="$VERSION" -t "ferri:$VERSION" -t ferri:latest .

	wait_for_quiet

	# Only the live compose file is pinned. The repo copy is never touched, so
	# the checkout stays identical to GitHub.
	echo "==> Pinning $LIVE_COMPOSE to ferri:$VERSION"
	pin "$VERSION"

	echo "==> Restarting"
	cd "$DEPLOY_DIR"
	docker compose down
	if docker compose up -d && wait_healthy; then
		docker compose logs --tail=20
		echo "==> ferri:$VERSION is running and healthy"
		exit 0
	fi
	rollback
}

# latest_tag is the highest vX.Y.Z tag, by version number.
latest_tag() {
	git tag --list 'v[0-9]*' --sort=-v:refname | sed -n 1p
}

# running_version is the tag pinned in the live compose file.
running_version() {
	sed -nE 's#^[[:space:]]*image:[[:space:]]*ferri:([^[:space:]]+).*#\1#p' "$1" | sed -n 1p
}

pin() {
	sed -i.bak -E "s#^([[:space:]]*image:[[:space:]]*ferri:).*#\1$1#" "$LIVE_COMPOSE"
	rm -f "$LIVE_COMPOSE.bak"
}

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
wait_for_quiet() {
	[[ "${FORCE:-}" == 1 ]] && return
	command -v ss >/dev/null || {
		echo "ERROR: ss (iproute2) not found; install it or run with FORCE=1" >&2
		exit 1
	}
	local app_addr="${APP_ADDR:-127.0.0.1:8080}"
	local wait_max="${WAIT_MAX:-180}"
	local started=$SECONDS quiet=0 last=-1 next_report=0 n waited
	while :; do
		n=$(ss -Htn state established "dst $app_addr" | wc -l)
		n=$((n))
		if [[ $n -eq 0 ]]; then
			quiet=$((quiet + 1))
			[[ $quiet -ge 2 ]] && return
		else
			quiet=0
		fi
		waited=$((SECONDS - started))
		if [[ $waited -ge $((wait_max * 60)) ]]; then
			echo "Still busy after $wait_max minutes. Nothing was changed; the new image ferri:$VERSION is built." >&2
			echo "Run again later, with a larger WAIT_MAX, or with FORCE=1." >&2
			exit 3
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
}

# wait_healthy waits up to 3 minutes for Docker's healthcheck (compose
# healthcheck: /ferri -health, every 30 s) to report the app healthy.
wait_healthy() {
	local i cid status
	for ((i = 0; i < 36; i++)); do
		sleep 5
		cid=$(docker compose ps -q app 2>/dev/null || true)
		[[ -n "$cid" ]] || continue
		status=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$cid" 2>/dev/null || true)
		[[ "$status" == healthy ]] && return 0
	done
	return 1
}

# rollback puts the previous image back. Migrations only add, so the
# previous version runs on the database the new one may have migrated.
rollback() {
	echo "ERROR: ferri:$VERSION did not become healthy. Its last log lines:" >&2
	docker compose logs --tail=40 app >&2 || true
	echo "==> Rolling back to ferri:$PREV" >&2
	pin "$PREV"
	docker compose down || true
	if docker compose up -d && wait_healthy; then
		echo "Rolled back: ferri:$PREV is running and healthy. ferri:$VERSION was not deployed." >&2
		exit 4
	fi
	echo "ERROR: ferri:$PREV is not healthy either. Ferri is DOWN." >&2
	docker compose logs --tail=40 app >&2 || true
	exit 5
}

main "$@"; exit
