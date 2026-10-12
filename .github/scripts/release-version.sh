#!/usr/bin/env bash
# Compute gos3 version numbers from the release tags (vMAJOR.MINOR.PATCH) of
# the repository in the current directory; it needs the tags fetched.
#
#   release-version.sh next patch|minor|major
#       The tag the Release workflow should create: the newest release tag
#       bumped as asked (v0.0.0 when there is none). When HEAD already
#       carries a release tag it prints that tag instead, so re-running a
#       release rebuilds it rather than bumping again.
#
#   release-version.sh dev BUILD_NUMBER
#       A dev build's version: the next patch release as a prerelease with
#       the build number and commit, e.g. v0.1.1-dev.42+0123456789ab, which
#       sorts after the newest release and before the next one.
set -euo pipefail

semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

usage() {
	echo "usage: $0 next patch|minor|major | dev BUILD_NUMBER" >&2
	exit 2
}

# Newest release tag in the repository, or v0.0.0.
latest() {
	git tag --list 'v*' | grep -E "$semver" | sort -V | tail -n 1 | grep . || echo v0.0.0
}

bump() {
	local tag=$1 part=$2 major minor patch
	[[ $tag =~ $semver ]] || return 1
	major=${BASH_REMATCH[1]} minor=${BASH_REMATCH[2]} patch=${BASH_REMATCH[3]}
	case $part in
		major) echo "v$((major + 1)).0.0" ;;
		minor) echo "v$major.$((minor + 1)).0" ;;
		patch) echo "v$major.$minor.$((patch + 1))" ;;
		*) usage ;;
	esac
}

case ${1:-} in
	next)
		[[ $# -eq 2 ]] || usage
		head_tag="$(git tag --points-at HEAD --list 'v*' | grep -E "$semver" | sort -V | tail -n 1 || true)"
		if [[ -n $head_tag ]]; then
			echo "$head_tag"
		else
			bump "$(latest)" "$2"
		fi
		;;
	dev)
		[[ $# -eq 2 && $2 =~ ^[0-9]+$ ]] || usage
		echo "$(bump "$(latest)" patch)-dev.$2+$(git rev-parse --short=12 HEAD)"
		;;
	*) usage ;;
esac
