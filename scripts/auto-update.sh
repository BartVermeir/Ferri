#!/usr/bin/env bash
# Unattended update, run by a timer on the server: deploys the highest
# release tag on GitHub when it is not the version that runs. Pushing a tag
# is the release; commits without a tag are never deployed.
#
# A file NO_AUTO_UPDATE in the deploy directory pauses it. The script writes
# that file itself when a new version was not healthy and got rolled back,
# so the same version is not tried again; delete it once that is fixed.
# Exit codes are those of scripts/deploy.sh; 0 also when nothing was to do.

set -euo pipefail

# Everything runs from main: deploy.sh checks out another version of this
# file while it runs, and bash reads a script as it goes.
main() {
	local repo_dir deploy_dir pause latest running rc
	repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
	deploy_dir="$(dirname "$repo_dir")"
	pause="$deploy_dir/NO_AUTO_UPDATE"

	if [[ -e "$pause" ]]; then
		echo "Paused: $pause exists. Delete it to resume automatic updates."
		cat "$pause"
		exit 0
	fi

	cd "$repo_dir"
	git fetch --quiet --tags --force origin
	latest="$(git tag --list 'v[0-9]*' --sort=-v:refname | sed -n 1p)"
	running="$(sed -nE 's#^[[:space:]]*image:[[:space:]]*ferri:([^[:space:]]+).*#\1#p' "$deploy_dir/docker-compose.yml" | sed -n 1p)"
	if [[ -z "$latest" ]]; then
		echo "ERROR: no release tag on GitHub" >&2
		exit 1
	fi
	if [[ "$latest" == "$running" ]]; then
		echo "Up to date: $running"
		exit 0
	fi

	echo "==> Update $running → $latest"
	rc=0
	"$repo_dir/scripts/deploy.sh" "$latest" || rc=$?
	case $rc in
	4)
		printf '%s: %s was not healthy, rolled back to %s.\nSee: journalctl -u ferri-auto-update\n' \
			"$(date '+%Y-%m-%d %H:%M')" "$latest" "$running" >"$pause"
		echo "Automatic updates paused: $pause" >&2
		;;
	5)
		printf '%s: %s was not healthy and the rollback to %s failed too: Ferri was down.\nSee: journalctl -u ferri-auto-update\n' \
			"$(date '+%Y-%m-%d %H:%M')" "$latest" "$running" >"$pause"
		echo "Automatic updates paused: $pause" >&2
		;;
	3) echo "Still busy, nothing changed. Next scheduled run tries again." >&2 ;;
	esac
	exit $rc
}

main "$@"; exit
