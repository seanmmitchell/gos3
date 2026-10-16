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
| `release-tag-creation` | tags `v*` | only repository admins and deploy keys may create release tags (the Release workflow pushes them with a deploy key, see [below](#release-tags-and-the-release-environment)) |
| `release-branch-names` | branches `v*` | nobody may create a branch named like a release tag (see [Build integrity](#build-integrity)) |

Required checks on both branches: `Go (fmt, vet, test, e2e)`,
`Go tests (macos-latest)`, `Go tests (windows-latest)`,
`Web UI (typecheck, test, build)`, `Playwright (mocked, real server, standalone)`,
`Compatibility (boto3, AWS CLI)`, `Docker images` (all from `ci.yml`),
`No DO NOT MERGE label` (from `do-not-merge.yml`),
`Socket Security: Pull Request Alerts` and `GitGuardian Security Checks`.
`No DO NOT MERGE label` fails while the pull request has the `DO NOT MERGE`
label and passes again when the label is removed, so the label blocks a
merge into either branch. A required check is matched by name: after
renaming a CI job, update both JSON files and apply them again, or pull
requests wait for a check that never reports.

Notes:

- **No required approvals.** gos3 has a single maintainer, and GitHub does
  not let authors approve their own pull requests. When a second maintainer
  joins, raise `required_approving_review_count` (and consider
  `require_last_push_approval`) in both branch rulesets.
- **Signed commits.** The commits GitHub creates when it merges a pull
  request are signed, so squash merges into `dev` and merge commits into
  `main` pass; commits on pull request branches do not need signatures.
- **No bypass.** No one, admins included, can push to `dev` or `main`
  directly or merge without the checks, or delete or move a release tag. An
  admin can still edit or disable a ruleset, which the audit log records.
  The one exception is creating a `v*` tag (`release-tag-creation`), which
  admins and deploy keys may do and nobody else.
- **Single maintainer.** Without required approvals, the checks and the
  `dev` → `main` → Release sequence are what stand between a change and a
  release; a stolen maintainer token could still merge its own pull request.
  The `release` environment deliberately has no required reviewers either
  (the maintainer would approve their own run). Add both when a second
  maintainer joins.

### Applying the rules

With the GitHub CLI, signed in as a repository admin:

```sh
.github/rulesets/apply.sh            # or: .github/rulesets/apply.sh OWNER/REPO
```

The script makes `dev` the default branch; allows squash merges and merge
commits but not rebase merges; turns on auto-merge, "update branch" and
deleting merged branches; uses the pull request title and description as the
merge commit message; creates or updates the five rulesets; and sets up the
`release` environment with its deploy key (next section). It is safe to run
again after changing a JSON file, and **must** be run again by an admin
after a ruleset file changes: GitHub only enforces what was last applied.
Without the CLI, import each file under Settings → Rules → Rulesets → New
ruleset → Import a ruleset, and set up the environment by hand.

Also turn on release immutability (Settings → General → Releases) so a
published release and its assets cannot be changed.

### Release tags and the release environment

A `v*` tag is permanent: tags cannot be deleted or moved, and once anyone
asks the Go module proxy for that version, `proxy.golang.org` and
`sum.golang.org` record it for good (`go install …@latest` picks the highest
one). So creating one is restricted to repository admins and deploy keys
(`release-tag-creation.json`), not every account or workflow token with
write access.

The Release workflow's tag job runs in the **`release` environment**, which
only `main` may deploy to, and pushes the tag over SSH with a write deploy
key stored there as the secret `RELEASE_DEPLOY_KEY`. Its `GITHUB_TOKEN` is
read-only. A workflow on any other branch cannot use the environment, so it
can neither read the key nor create a release tag. `apply.sh` creates the
environment with that branch policy and, when the secret is missing,
generates an ed25519 key pair, adds the public key as a write deploy key
named "Release tags (environment release)" and stores the private key as
the secret (the key never touches the disk outside a temporary directory).
To rotate it, delete the secret and the deploy key, then run `apply.sh`
again. Without the secret, the Release workflow stops with an error before
tagging.

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
   - tags `main`'s head with the next version (with the deploy key, see
     above);
   - runs the Go and web tests, builds embedded-UI and headless archives for
     Linux, macOS and Windows (amd64 and arm64) through
     [`build.yml`](../.github/workflows/build.yml), without any cache (see
     [build integrity](#build-integrity));
   - pushes `ghcr.io/seanmmitchell/gos3` tagged `X.Y.Z`, `X.Y`, `X` (from
     1.0.0 on) and `latest`, with BuildKit provenance, an SBOM and a signed
     build provenance attestation; publishes the headless image under the
     same tags with `-headless` appended, including `latest-headless`;
   - attests the archives' build provenance, then creates the GitHub release
     as a draft with the archives, `checksums.txt` and notes generated from
     the merged pull requests, grouped by label
     ([`.github/release.yml`](../.github/release.yml)), and publishes it.

If a run fails after the tag was created, run the workflow again: on a
commit that already has a release tag whose release is not published yet,
it rebuilds that release (completing the draft) instead of bumping the
version. A published release is never rebuilt or changed; the workflow
refuses.

## Verifying a release

Every release archive and image from the first release after v0.1.0 on has
a [build provenance attestation](https://docs.github.com/actions/security-for-github-actions/using-artifact-attestations):
a Sigstore-signed statement, made by the workflow run that built it, of the
repository, workflow, branch and commit it came from. `checksums.txt` only
proves integrity (anyone who can upload assets can upload checksums too); an
attestation also proves origin. Check one with the GitHub CLI:

```sh
# A release archive (attested by release.yml)
gh attestation verify gos3_X.Y.Z_linux_amd64.tar.gz --repo seanmmitchell/gos3 \
  --source-ref refs/heads/main \
  --signer-workflow seanmmitchell/gos3/.github/workflows/release.yml

# The container image (attested by build.yml, called from release.yml)
gh attestation verify oci://ghcr.io/seanmmitchell/gos3:X.Y.Z --repo seanmmitchell/gos3 \
  --source-ref refs/heads/main \
  --signer-workflow seanmmitchell/gos3/.github/workflows/build.yml
```

`--source-ref refs/heads/main` is what matters: only the Release workflow
runs on `main`, so an archive or image built by a workflow on any other
branch (even with a write token of this repository) fails the check. Dev
builds verify with `--source-ref refs/heads/dev`. The image check prints the
verified digest (`sha256:…`); deploy that digest (for example `digest:` in
`deploy/kubernetes/kustomization.yaml`) rather than a tag, which can be
pushed again.

GitHub stores attestations only for public repositories (private ones need
GitHub Enterprise Cloud), so the workflows skip the attestation steps while
the repository is private; builds from that time have none to verify, and
only `checksums.txt` and the image's BuildKit provenance are available.

## Build integrity

What keeps code that did not go through `dev` → `main` out of a release:

- **Built from the tagged commit.** The tag job outputs the full SHA of
  the commit it tagged (and checks that it is `main`'s head), and the
  release builds that SHA, never the tag's name: for a bare name,
  `actions/checkout` prefers a branch over a tag, so a pushed branch named
  `vX.Y.Z` would otherwise be built and attested as `main`. `build.yml`
  accepts only a full commit SHA as `ref`, and every job fails unless
  `git rev-parse HEAD` equals it. The `release-branch-names` ruleset also
  refuses `v*` branches outright.
- **No caches in release builds.** GitHub Actions caches are shared between
  branches (a run on `main` restores entries from the default branch, `dev`),
  and any code that runs in a job (tests, npm and pip packages, tools) can
  write them. Go does not re-verify a restored module or build cache, nor
  BuildKit a restored layer, so a poisoned entry would end up in the release.
  `build.yml` therefore takes `use-cache`; release.yml sets it to `false`,
  which turns off the `setup-go` caches and the BuildKit `gha` cache
  (`no-cache`), and `setup-node`'s cache is off in every build job. Dev
  builds still use caches (separate `gos3-dev-embedded` and
  `gos3-dev-headless` image scopes), so a dev build is not proof of
  what the release will contain; the release rebuilds from the tag. The
  `binaries` job also runs `go mod verify` against `go.sum`.
- **Tests apart from the shipped UI.** The embedded UI is built in a job
  that installs production dependencies only (`pnpm install --prod`) and
  runs `pnpm generate:embed`; the web tests run in a separate job, so
  test-only packages (Vitest, happy-dom, @nuxt/test-utils, …) never run
  next to the files that are embedded. The UI's own production dependencies
  (Nuxt and the packages it loads) still run in that job, and anything that
  runs there could change the output: the lockfile (integrity hashes), the
  14-day release-age cooldown, `onlyBuiltDependencies` and Socket's pull
  request checks are the controls for those.
- **No token on disk.** Every checkout uses `persist-credentials: false`,
  except the tag job, which needs its deploy key to push. The image job logs
  out of ghcr.io when it is done (the registry credential has to exist while
  the image is pushed).
- **Pinned inputs.** Actions are pinned by commit SHA and base images by
  digest. The image build fails if the golang image's Go is older than
  `go.mod`'s `go` line, so the image never links an older standard library
  than the minimum the archives are built with.

## Dev builds

Every push to `dev` runs [`dev-build.yml`](../.github/workflows/dev-build.yml):
the same tests and builds as a release (but with caches), published as
`ghcr.io/seanmmitchell/gos3:dev` and `:dev-<commit>` with an attestation
for `--source-ref refs/heads/dev`, and with the archives kept
as artifacts of the run for 30 days. Nothing appears on the Releases page.

Both release and dev builds produce headless archives for every OS and
architecture, named `gos3_<version>_<os>_<arch>-headless.tar.gz` (`.zip` on
Windows), in `bin-<os>-<arch>-headless` workflow artifacts. They compile with
`-tags noui` and never download the built UI. The image workflow selects
Docker's `headless` target, which has no Node/pnpm build dependency, and
publishes `:dev-headless` and `:dev-<commit>-headless` alongside the ordinary
dev tags. Headless release archives receive the same checksums and
attestations as embedded archives; both image variants receive provenance
and SBOMs. Their management API remains enabled by default; use `--no-admin`
for S3-only operation.
