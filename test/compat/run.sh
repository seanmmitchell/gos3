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
#   GOS3_COMPAT_CLUSTER=1 start two mutually authenticated cluster voters
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
second_server_pid=""
cleanup() {
	local rc=$?
	for pid in "$server_pid" "$second_server_pid"; do
		if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
			kill -TERM "$pid" 2>/dev/null || true
			for _ in $(seq 1 100); do
				kill -0 "$pid" 2>/dev/null || break
				sleep 0.1
			done
			kill -KILL "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
		fi
	done
	if [[ $rc -ne 0 ]]; then
		for file in "$work/server.log" "$work/second-server.log"; do
			if [[ -f "$file" ]]; then
				log "$file (last 60 lines):"
				tail -n 60 "$file" >&2 || true
			fi
		done
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

read -r port second_port peer_a_port peer_b_port < <(python - <<'EOF'
import socket
sockets = []
for _ in range(4):
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    sockets.append(s)
print(*(s.getsockname()[1] for s in sockets))
for s in sockets:
    s.close()
EOF
)
export GOS3_COMPAT_ENDPOINT="http://127.0.0.1:$port"
if [[ "${GOS3_COMPAT_CLUSTER:-}" == 1 ]]; then
	export GOS3_COMPAT_SECOND_ENDPOINT="http://127.0.0.1:$second_port"
else
	unset GOS3_COMPAT_SECOND_ENDPOINT
fi
export GOS3_COMPAT_ACCESS_KEY="compatroot"
export GOS3_COMPAT_SECRET_KEY="compatroot-secret-key"
export GOS3_COMPAT_REGION="us-east-1"

if [[ "${GOS3_COMPAT_CLUSTER:-}" == 1 ]]; then
	log "creating mutual TLS identities for boto3 cluster compatibility"
	openssl genpkey -algorithm ED25519 -out "$work/ca.key" >/dev/null 2>&1
	openssl req -x509 -new -key "$work/ca.key" -out "$work/ca.pem" -days 1 \
		-subj '/CN=gos3 compatibility CA' -addext 'basicConstraints=critical,CA:TRUE' \
		-addext 'keyUsage=keyCertSign' >/dev/null 2>&1
	for node in a b; do
		openssl genpkey -algorithm ED25519 -out "$work/$node.key" >/dev/null 2>&1
		openssl req -new -key "$work/$node.key" -out "$work/$node.csr" -subj "/CN=$node" >/dev/null 2>&1
		cat >"$work/$node.ext" <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=serverAuth,clientAuth
subjectAltName=IP:127.0.0.1,URI:spiffe://gos3/compat-cluster/$node
EOF
		openssl x509 -req -in "$work/$node.csr" -CA "$work/ca.pem" -CAkey "$work/ca.key" \
			-CAcreateserial -out "$work/$node.pem" -days 1 -extfile "$work/$node.ext" >/dev/null 2>&1
	done
	for node in a b; do
		if [[ "$node" == a ]]; then public_port="$port"; else public_port="$second_port"; fi
		cat >"$work/$node.yaml" <<EOF
server:
  address: 127.0.0.1:$public_port
storage:
  backend: fs
  path: $work/$node-data
  fsync_data: true
cluster:
  enabled: true
  id: compat-cluster
  node_id: $node
  peers:
    - id: a
      url: https://127.0.0.1:$peer_a_port
    - id: b
      url: https://127.0.0.1:$peer_b_port
  tls_ca: $work/ca.pem
  tls_cert: $work/$node.pem
  tls_key: $work/$node.key
  replica_count: 2
  min_memory_acks: 2
  min_durable_acks: 2
EOF
	done
	log "starting two gos3 voters on $GOS3_COMPAT_ENDPOINT and $GOS3_COMPAT_SECOND_ENDPOINT"
	GOS3_ROOT_ACCESS_KEY="$GOS3_COMPAT_ACCESS_KEY" GOS3_ROOT_SECRET_KEY="$GOS3_COMPAT_SECRET_KEY" \
		GOS3_REGION="$GOS3_COMPAT_REGION" GOS3_ACCESS_LOG=false GOS3_LIFECYCLE_ENABLED=false \
		"$bin" serve --config "$work/a.yaml" --no-discovery --no-ui --log-level warn \
		>"$work/server.log" 2>&1 &
	server_pid=$!
	GOS3_ROOT_ACCESS_KEY="$GOS3_COMPAT_ACCESS_KEY" GOS3_ROOT_SECRET_KEY="$GOS3_COMPAT_SECRET_KEY" \
		GOS3_REGION="$GOS3_COMPAT_REGION" GOS3_ACCESS_LOG=false GOS3_LIFECYCLE_ENABLED=false \
		"$bin" serve --config "$work/b.yaml" --no-discovery --no-ui --log-level warn \
		>"$work/second-server.log" 2>&1 &
	second_server_pid=$!
else
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
fi

for _ in $(seq 1 300); do
	if "$bin" healthcheck --url "$GOS3_COMPAT_ENDPOINT" --ready >/dev/null 2>&1 &&
		{ [[ "${GOS3_COMPAT_CLUSTER:-}" != 1 ]] || "$bin" healthcheck --url "$GOS3_COMPAT_SECOND_ENDPOINT" --ready >/dev/null 2>&1; }; then
		break
	fi
	if ! kill -0 "$server_pid" 2>/dev/null; then
		log "gos3 exited during startup"
		exit 1
	fi
	if [[ -n "$second_server_pid" ]] && ! kill -0 "$second_server_pid" 2>/dev/null; then
		log "second gos3 voter exited during startup"
		exit 1
	fi
	sleep 0.1
done
"$bin" healthcheck --url "$GOS3_COMPAT_ENDPOINT" --ready >/dev/null
if [[ "${GOS3_COMPAT_CLUSTER:-}" == 1 ]]; then
	"$bin" healthcheck --url "$GOS3_COMPAT_SECOND_ENDPOINT" --ready >/dev/null
fi

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
