#!/usr/bin/env bash
# Python / AWS CLI compatibility suite for gos3.
#
# Builds bin/gos3 (without the web UI), starts it on a free local port with
# the fs backend in a temporary directory and known root credentials, then
# runs the boto3 pytest suite and the AWS CLI script against it. The server
# is always stopped and the temporary directory removed on exit.
#
# Usage: test/compat/run.sh [pytest args...]
#
# Environment:
#   GOS3_COMPAT_VENV   venv location (default ${TMPDIR:-/tmp}/gos3-compat-venv;
#                      reused when present and matching requirements.txt)
#   GOS3_COMPAT_KEEP=1 keep the temporary data directory and server log
#   GOS3_COMPAT_SKIP_BUILD=1 use the existing bin/gos3
#   GOS3_COMPAT_SKIP_CLI=1 / GOS3_COMPAT_SKIP_PYTEST=1 skip a part
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
venv="${GOS3_COMPAT_VENV:-${TMPDIR:-/tmp}/gos3-compat-venv}"
bin="$repo/bin/gos3"

log() { printf '==> %s\n' "$*" >&2; }

# --- Python environment (outside the repository) -----------------------------
stamp="$venv/.gos3-requirements"
# A venv links to its interpreter's absolute path: after a Python upgrade
# the links dangle, and only --clear recreates them.
if ! "$venv/bin/python" -c '' 2>/dev/null || ! cmp -s "$here/requirements.txt" "$stamp"; then
	if [[ -e "$venv" && ! -f "$venv/pyvenv.cfg" ]]; then
		log "$venv exists but is not a venv; refusing to clear it (set GOS3_COMPAT_VENV)"
		exit 1
	fi
	log "creating venv $venv"
	python3 -m venv --clear "$venv"
	# requirements.txt is hash-locked (see requirements.in); the venv's bundled
	# pip installs it as-is, without first upgrading to an unpinned pip.
	"$venv/bin/python" -m pip install --quiet --require-hashes -r "$here/requirements.txt"
	cp "$here/requirements.txt" "$stamp"
else
	log "reusing venv $venv"
fi
export PATH="$venv/bin:$PATH"
# Keep __pycache__ / .pytest_cache out of the repository.
export PYTHONDONTWRITEBYTECODE=1

# --- Build ---------------------------------------------------------------------
if [[ "${GOS3_COMPAT_SKIP_BUILD:-}" != 1 ]]; then
	log "building $bin"
	(cd "$repo" && go build -o "$bin" ./cmd/gos3)
fi

# --- Start the server ------------------------------------------------------------
work="$(mktemp -d "${TMPDIR:-/tmp}/gos3-compat.XXXXXX")"
server_pid=""
cleanup() {
	local rc=$?
	if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
		kill -TERM "$server_pid" 2>/dev/null || true
		for _ in $(seq 1 100); do
			kill -0 "$server_pid" 2>/dev/null || break
			sleep 0.1
		done
		kill -KILL "$server_pid" 2>/dev/null || true
		wait "$server_pid" 2>/dev/null || true
	fi
	if [[ $rc -ne 0 && -f "$work/server.log" ]]; then
		log "server log (last 60 lines):"
		tail -n 60 "$work/server.log" >&2 || true
	fi
	if [[ "${GOS3_COMPAT_KEEP:-}" == 1 ]]; then
		log "kept $work"
	else
		rm -rf "$work"
	fi
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

port="$(python - <<'EOF'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
EOF
)"
export GOS3_COMPAT_ENDPOINT="http://127.0.0.1:$port"
export GOS3_COMPAT_ACCESS_KEY="compatroot"
export GOS3_COMPAT_SECRET_KEY="compatroot-secret-key"
export GOS3_COMPAT_REGION="us-east-1"

log "starting gos3 on $GOS3_COMPAT_ENDPOINT (data $work/data)"
GOS3_ROOT_ACCESS_KEY="$GOS3_COMPAT_ACCESS_KEY" \
	GOS3_ROOT_SECRET_KEY="$GOS3_COMPAT_SECRET_KEY" \
	GOS3_REGION="$GOS3_COMPAT_REGION" \
	GOS3_ACCESS_LOG=false \
	GOS3_LIFECYCLE_ENABLED=false \
	"$bin" serve --address "127.0.0.1:$port" --backend fs --data "$work/data" \
	--no-discovery --no-ui --fsync=false --log-level warn \
	>"$work/server.log" 2>&1 &
server_pid=$!

for _ in $(seq 1 100); do
	if "$bin" healthcheck --url "$GOS3_COMPAT_ENDPOINT" --ready >/dev/null 2>&1; then
		break
	fi
	if ! kill -0 "$server_pid" 2>/dev/null; then
		log "gos3 exited during startup"
		exit 1
	fi
	sleep 0.1
done
"$bin" healthcheck --url "$GOS3_COMPAT_ENDPOINT" --ready >/dev/null

# --- Run the clients ---------------------------------------------------------------
status=0
if [[ "${GOS3_COMPAT_SKIP_PYTEST:-}" != 1 ]]; then
	log "running pytest"
	(cd "$here" && python -m pytest -p no:cacheprovider -q "$@") || status=1
fi
if [[ "${GOS3_COMPAT_SKIP_CLI:-}" != 1 ]]; then
	log "running AWS CLI script"
	bash "$here/awscli.sh" || status=1
fi

if [[ $status -eq 0 ]]; then
	log "compat suite passed"
else
	log "compat suite FAILED"
fi
exit "$status"
