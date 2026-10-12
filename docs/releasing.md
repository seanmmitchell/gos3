# Branches, releases and versioning

## Branches

| Branch | Holds | Receives | Merge method |
|---|---|---|---|
| `dev` (default) | the next release, always releasable | pull requests from feature branches and Dependabot | squash |
| `main` | released code; every release is a tag on `main` | pull requests from `dev` only | merge commit |

Work happens on short-lived branches cut from `dev`, merged back with a pull
request. `main` advances only when a release is cut, by one merge commit
from `dev` per release. A fix for a released version goes into `dev` like any
other change and ships as a patch release; `main` never takes changes that
`dev` does not have.

## Branch rules

The rulesets in [`.github/rulesets/`](../.github/rulesets) enforce this model:

| Ruleset | Applies to | Enforces |
|---|---|---|
| `dev` | `dev` | no deletion or force push; changes only through pull requests, squash-merged, with resolved review threads; linear history; signed commits; required checks |
| `main` | `main` | the same, but merge commits only (no linear history), plus the `Release PR source` check, which fails unless the pull request comes from `dev` of this repository |
| `release-tags` | tags `v*` | release tags cannot be deleted, moved or force-pushed |

Required checks on both branches: `Go (fmt, vet, test, e2e)`,
`Go tests (macos-latest)`, `Go tests (windows-latest)`,
`Web UI (typecheck, test, build)`, `Playwright (mocked, real server, standalone)`,
`Compatibility (boto3, AWS CLI)`, `Docker images` (all from `ci.yml`),
`Socket Security: Pull Request Alerts` and `GitGuardian Security Checks`.
A required check is matched by name: after renaming a CI job, update both
JSON files and apply them again, or pull requests wait for a check that
never reports.

Notes:

- **No required approvals.** gos3 has a single maintainer, and GitHub does
  not let authors approve their own pull requests. When a second maintainer
  joins, raise `required_approving_review_count` (and consider
  `require_last_push_approval`) in both branch rulesets.
- **Signed commits.** The commits GitHub creates when it merges a pull
  request are signed, so squash merges into `dev` and merge commits into
  `main` pass; commits on pull request branches do not need signatures.
- **No bypass.** No one, admins included, can push to `dev` or `main`
  directly or merge without the checks. An admin can still edit or disable a
  ruleset, which the audit log records.

### Applying the rules

With the GitHub CLI, signed in as a repository admin:

```sh
.github/rulesets/apply.sh            # or: .github/rulesets/apply.sh OWNER/REPO
```

The script makes `dev` the default branch; allows squash merges and merge
commits but not rebase merges; turns on auto-merge, "update branch" and
deleting merged branches; uses the pull request title and description as the
merge commit message; and creates or updates the three rulesets. It is safe
to run again after changing a JSON file. Without the CLI, import each file
under Settings → Rules → Rulesets → New ruleset → Import a ruleset.

Also turn on release immutability (Settings → General → Releases) so a
published release and its assets cannot be changed.

## Versioning

Releases follow [Semantic Versioning](https://semver.org) as tags
`vMAJOR.MINOR.PATCH` on `main`. Before 1.0.0, a minor release may change
configuration, the admin API or the storage format (the release notes say
how to upgrade); patch releases only fix. `gos3 version` prints the version,
commit and build date stamped into the binary.

Builds from `dev` carry the next patch version as a prerelease, for example
`v0.1.1-dev.42+0123456789ab` (build number and commit), so they sort after
the release they follow and before the next one. Local builds (`make build`)
use `git describe`.

## Cutting a release

1. Open a pull request from `dev` into `main` titled `Release vX.Y.Z`, with
   the `skip-changelog` label. Once the checks pass, merge it (merge commit).
2. Run Actions → **Release** → Run workflow on branch `main`, choosing the
   part of the version to increment: `patch`, `minor` or `major`. The first
   release, from no tags, is `minor` → `v0.1.0`.
3. The workflow ([`release.yml`](../.github/workflows/release.yml)):
   - tags `main`'s head with the next version;
   - runs the Go tests, builds the embedded UI and the archives for Linux,
     macOS and Windows (amd64 and arm64) through
     [`build.yml`](../.github/workflows/build.yml);
   - pushes `ghcr.io/seanmmitchell/gos3` tagged `X.Y.Z`, `X.Y`, `X` (from
     1.0.0 on) and `latest`, with provenance and an SBOM;
   - publishes the GitHub release with the archives, `checksums.txt` and
     notes generated from the merged pull requests, grouped by label
     ([`.github/release.yml`](../.github/release.yml)).

If a run fails after the tag was created, run the workflow again: on a
commit that already has a release tag it rebuilds that release instead of
bumping the version.

## Dev builds

Every push to `dev` runs [`dev-build.yml`](../.github/workflows/dev-build.yml):
the same tests and builds as a release, published as
`ghcr.io/seanmmitchell/gos3:dev` and `:dev-<commit>`, with the archives kept
as artifacts of the run for 30 days. Nothing appears on the Releases page.
