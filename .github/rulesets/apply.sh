#!/usr/bin/env bash
# Apply gos3's branch model to a GitHub repository with the GitHub CLI: the
# repository's merge settings (dev as the default branch, squash merges into
# dev, merge commits from dev into main, no rebase merges) and the rulesets in
# this directory. Needs admin rights on the repository (`gh auth login`).
#
#   .github/rulesets/apply.sh [OWNER/REPO]
#
# Idempotent: a ruleset whose name matches its file name is updated in place,
# a missing one is created. The JSON files can also be imported by hand under
# Settings → Rules → Rulesets → New ruleset → Import a ruleset.
# See docs/releasing.md.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="${1:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}"

echo "==> $repo: merge settings"
gh api --method PATCH "repos/$repo" --silent \
	-f default_branch=dev \
	-F allow_squash_merge=true \
	-F allow_merge_commit=true \
	-F allow_rebase_merge=false \
	-F allow_auto_merge=true \
	-F allow_update_branch=true \
	-F delete_branch_on_merge=true \
	-f squash_merge_commit_title=PR_TITLE \
	-f squash_merge_commit_message=PR_BODY \
	-f merge_commit_title=PR_TITLE \
	-f merge_commit_message=PR_BODY

existing="$(gh api "repos/$repo/rulesets" --paginate --jq '.[] | "\(.name)\t\(.id)"')"
for file in "$here"/*.json; do
	name="$(basename "$file" .json)"
	if ! grep -q "\"name\": \"$name\"" "$file"; then
		echo "$file: the ruleset name must match the file name ($name)" >&2
		exit 1
	fi
	id="$(awk -F '\t' -v name="$name" '$1 == name { print $2 }' <<<"$existing")"
	if [[ -n $id ]]; then
		echo "==> $repo: update ruleset $name ($id)"
		gh api --method PUT "repos/$repo/rulesets/$id" --input "$file" --silent
	else
		echo "==> $repo: create ruleset $name"
		gh api --method POST "repos/$repo/rulesets" --input "$file" --silent
	fi
done

cat <<EOF
Done. Also turn on release immutability (Settings → General → Releases), so
published releases and their assets cannot be changed.
EOF
