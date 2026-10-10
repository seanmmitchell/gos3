#!/usr/bin/env bash
# AWS CLI (v1) compatibility script for gos3. Run by run.sh, which exports
# GOS3_COMPAT_ENDPOINT / _ACCESS_KEY / _SECRET_KEY / _REGION and puts the venv
# (with `aws`) first on PATH. Uses --endpoint-url and path-style addressing.
set -euo pipefail

: "${GOS3_COMPAT_ENDPOINT:?run via test/compat/run.sh}"

tmp="$(mktemp -d "${TMPDIR:-/tmp}/gos3-awscli.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

export AWS_ACCESS_KEY_ID="$GOS3_COMPAT_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$GOS3_COMPAT_SECRET_KEY"
export AWS_DEFAULT_REGION="${GOS3_COMPAT_REGION:-us-east-1}"
export AWS_CONFIG_FILE="$tmp/aws-config"
export AWS_SHARED_CREDENTIALS_FILE="$tmp/aws-credentials"
export AWS_EC2_METADATA_DISABLED=true
unset AWS_PROFILE AWS_SESSION_TOKEN AWS_ENDPOINT_URL AWS_ENDPOINT_URL_S3 || true
cat >"$AWS_CONFIG_FILE" <<'EOF'
[default]
s3 =
  addressing_style = path
  signature_version = s3v4
  multipart_threshold = 8MB
  multipart_chunksize = 8MB
  max_concurrent_requests = 8
EOF
: >"$AWS_SHARED_CREDENTIALS_FILE"

aws() { command aws --endpoint-url "$GOS3_COMPAT_ENDPOINT" "$@"; }

pass=0
step() { printf -- '--- %s\n' "$*"; }
fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}
ok() { pass=$((pass + 1)); }
eq() { # eq <got> <want> <what>
	[[ "$1" == "$2" ]] || fail "$3: got [$1], want [$2]"
	ok
}
contains() { # contains <haystack> <needle> <what>
	[[ "$1" == *"$2"* ]] || fail "$3: [$2] not found in: $1"
	ok
}
not_contains() {
	[[ "$1" != *"$2"* ]] || fail "$3: [$2] unexpectedly found in: $1"
	ok
}
json() { # json <expr> : evaluate a Python expression over JSON on stdin (as d)
	python -c "import json,sys; d=json.load(sys.stdin); print($1)"
}

suffix="$(python -c 'import uuid; print(uuid.uuid4().hex[:12])')"
b1="cli-$suffix"
b2="cli2-$suffix"
bv="cliv-$suffix"

cleanup_buckets() {
	for b in "$b1" "$b2"; do
		command aws --endpoint-url "$GOS3_COMPAT_ENDPOINT" s3 rb "s3://$b" --force >/dev/null 2>&1 || true
	done
	# Versioned bucket: delete every version and marker first.
	if command aws --endpoint-url "$GOS3_COMPAT_ENDPOINT" s3api head-bucket --bucket "$bv" >/dev/null 2>&1; then
		command aws --endpoint-url "$GOS3_COMPAT_ENDPOINT" s3api list-object-versions --bucket "$bv" --output json |
			python -c '
import json, sys
d = json.load(sys.stdin)
objs = [{"Key": v["Key"], "VersionId": v["VersionId"]} for v in d.get("Versions", []) + d.get("DeleteMarkers", [])]
print(json.dumps({"Objects": objs, "Quiet": True}))' >"$tmp/del.json"
		if [[ "$(python -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["Objects"]))' "$tmp/del.json")" != 0 ]]; then
			command aws --endpoint-url "$GOS3_COMPAT_ENDPOINT" s3api delete-objects --bucket "$bv" --delete "file://$tmp/del.json" >/dev/null
		fi
		command aws --endpoint-url "$GOS3_COMPAT_ENDPOINT" s3api delete-bucket --bucket "$bv" >/dev/null 2>&1 || true
	fi
	rm -rf "$tmp"
}
trap cleanup_buckets EXIT

# --- local fixtures ---------------------------------------------------------------
src="$tmp/src"
mkdir -p "$src/docs/nested" "$src/img" "$src/with space"
printf 'hello world\n' >"$src/hello.txt"
printf 'readme\n' >"$src/docs/README.md"
printf 'deep\n' >"$src/docs/nested/deep.txt"
printf 'unicode\n' >"$src/docs/żółć 日本.txt"
printf 'space\n' >"$src/with space/file +plus.txt"
printf 'odd\n' >"$src/docs/pct%20sign #hash?q=1&x.txt"
: >"$src/docs/empty.txt"
mkdir -p "$src/many"
for i in $(seq -w 1 150); do printf 'small %s\n' "$i" >"$src/many/f$i.txt"; done
head -c 4096 /dev/urandom >"$src/img/random.bin"
# 20 MiB: above the 8 MB multipart threshold.
head -c $((20 * 1024 * 1024)) /dev/urandom >"$src/big.bin"
nfiles=$(find "$src" -type f | wc -l | tr -d ' ')

# --- mb / ls ------------------------------------------------------------------------
step "s3 mb / ls"
out="$(aws s3 mb "s3://$b1")"
contains "$out" "make_bucket: $b1" "mb output"
aws s3 mb "s3://$b2" >/dev/null
out="$(aws s3 ls)"
contains "$out" "$b1" "ls buckets"
contains "$out" "$b2" "ls buckets"
if aws s3 mb "s3://$b1" >/dev/null 2>&1; then fail "mb of an existing bucket succeeded"; fi
ok

# --- cp ---------------------------------------------------------------------------------
step "s3 cp up/down (single + multipart)"
aws s3 cp "$src/hello.txt" "s3://$b1/single/hello.txt" >/dev/null
aws s3 cp "s3://$b1/single/hello.txt" "$tmp/hello.down" >/dev/null
cmp "$src/hello.txt" "$tmp/hello.down" || fail "hello.txt round trip"
ok
aws s3 cp "$src/big.bin" "s3://$b1/single/big.bin" >/dev/null
etag="$(aws s3api head-object --bucket "$b1" --key single/big.bin --query ETag --output text)"
contains "$etag" '-3"' "multipart ETag (20 MiB / 8 MB parts)"
aws s3 cp "s3://$b1/single/big.bin" "$tmp/big.down" >/dev/null
cmp "$src/big.bin" "$tmp/big.down" || fail "big.bin round trip"
ok

step "s3 cp with metadata / content type"
aws s3 cp "$src/hello.txt" "s3://$b1/meta/hello.txt" --metadata color=blue,size=small \
	--content-type text/x-hello --cache-control max-age=10 >/dev/null
head="$(aws s3api head-object --bucket "$b1" --key meta/hello.txt --output json)"
eq "$(json 'd["Metadata"]["color"]' <<<"$head")" blue "metadata color"
eq "$(json 'd["Metadata"]["size"]' <<<"$head")" small "metadata size"
eq "$(json 'd["ContentType"]' <<<"$head")" text/x-hello "content type"
eq "$(json 'd["CacheControl"]' <<<"$head")" max-age=10 "cache control"
eq "$(json 'd["ContentLength"]' <<<"$head")" 12 "content length"

step "s3 cp stdin/stdout"
printf 'from stdin' | aws s3 cp - "s3://$b1/stream/stdin.txt" >/dev/null
eq "$(aws s3 cp "s3://$b1/stream/stdin.txt" -)" "from stdin" "stdout download"

step "s3 cp --recursive"
aws s3 cp "$src" "s3://$b1/rec/" --recursive >/dev/null
n="$(aws s3 ls "s3://$b1/rec/" --recursive | wc -l | tr -d ' ')"
eq "$n" "$nfiles" "recursive copy count"
out="$(aws s3 ls "s3://$b1/rec/")"
contains "$out" "PRE docs/" "ls common prefix"
contains "$out" "PRE with space/" "ls prefix with space"
contains "$out" "hello.txt" "ls object"
out="$(aws s3 ls "s3://$b1/rec/docs/")"
contains "$out" "żółć 日本.txt" "ls unicode key"

step "s3 cp s3->s3 (server-side copy, incl. multipart copy)"
aws s3 cp "s3://$b1/single/big.bin" "s3://$b2/copied/big.bin" >/dev/null
aws s3 cp "s3://$b2/copied/big.bin" "$tmp/big.copied" >/dev/null
cmp "$src/big.bin" "$tmp/big.copied" || fail "s3->s3 copy content"
ok

# --- sync -----------------------------------------------------------------------------------
step "s3 sync up / incremental / --delete"
out="$(aws s3 sync --no-progress "$src" "s3://$b2/sync/")"
eq "$(grep -c '^upload:' <<<"$out")" "$nfiles" "initial sync uploads"
out="$(aws s3 sync --no-progress "$src" "s3://$b2/sync/")"
eq "$out" "" "second sync is a no-op"
sleep 1.1
printf 'hello again, longer\n' >"$src/hello.txt"
out="$(aws s3 sync --no-progress "$src" "s3://$b2/sync/")"
eq "$(grep -c '^upload:' <<<"$out")" 1 "changed file re-uploaded"
contains "$out" "hello.txt" "changed file name"
rm "$src/img/random.bin"
rmdir "$src/img" # S3 has no empty directories
out="$(aws s3 sync --no-progress "$src" "s3://$b2/sync/" --delete)"
contains "$out" "delete: s3://$b2/sync/img/random.bin" "sync --delete"
nfiles=$((nfiles - 1))

step "s3 sync down"
aws s3 sync "s3://$b2/sync/" "$tmp/down" >/dev/null
diff -r "$src" "$tmp/down" || fail "sync down differs"
ok
out="$(aws s3 sync --no-progress "s3://$b2/sync/" "$tmp/down")"
eq "$out" "" "second sync down is a no-op"

step "s3 sync s3->s3 with --exclude/--include"
aws s3 sync "s3://$b2/sync/" "s3://$b1/filtered/" --exclude '*' --include 'docs/*' >/dev/null
out="$(aws s3 ls "s3://$b1/filtered/" --recursive)"
contains "$out" "filtered/docs/README.md" "include docs"
not_contains "$out" "big.bin" "exclude others"

step "s3 ls --recursive --summarize"
out="$(aws s3 ls "s3://$b2/sync/" --recursive --summarize --human-readable)"
contains "$out" "Total Objects: $nfiles" "summarize count"

# --- mv / rm ------------------------------------------------------------------------------
step "s3 mv"
aws s3 mv "s3://$b1/single/hello.txt" "s3://$b1/moved/hello.txt" >/dev/null
out="$(aws s3 ls "s3://$b1/" --recursive)"
contains "$out" "moved/hello.txt" "mv destination"
not_contains "$out" "single/hello.txt" "mv source removed"
aws s3 mv "s3://$b1/moved/" "s3://$b2/moved-rec/" --recursive >/dev/null
out="$(aws s3 ls "s3://$b2/moved-rec/" --recursive)"
contains "$out" "moved-rec/hello.txt" "mv --recursive"
aws s3 mv "$tmp/hello.down" "s3://$b1/local-moved.txt" >/dev/null
[[ ! -e "$tmp/hello.down" ]] || fail "local file still present after mv"
ok

step "s3 rm / rm --recursive"
aws s3 rm "s3://$b1/local-moved.txt" >/dev/null
out="$(aws s3 rm "s3://$b1/rec/" --recursive | tr "\r" "\n")"
eq "$(grep -c '^delete:' <<<"$out")" "$((nfiles + 1))" "rm --recursive count"
eq "$(aws s3 ls "s3://$b1/rec/" --recursive | wc -l | tr -d ' ')" 0 "rec/ empty"

step "s3 presign"
url="$(aws s3 presign "s3://$b1/meta/hello.txt" --expires-in 300)"
contains "$url" "X-Amz-Signature=" "presigned URL is SigV4"
body="$(python -c 'import sys, urllib.request; print(urllib.request.urlopen(sys.argv[1]).read().decode(), end="")' "$url")"
eq "$body" "hello world" "presigned GET"

step "public bucket via --no-sign-request"
cat >"$tmp/policy.json" <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::$b1/meta/*"},
 {"Effect":"Allow","Principal":"*","Action":"s3:ListBucket","Resource":"arn:aws:s3:::$b1"}]}
EOF
aws s3api put-bucket-policy --bucket "$b1" --policy "file://$tmp/policy.json"
eq "$(aws s3 cp "s3://$b1/meta/hello.txt" - --no-sign-request)" "hello world" "anonymous get"
out="$(aws s3 ls "s3://$b1/meta/" --no-sign-request)"
contains "$out" "hello.txt" "anonymous ls"
if aws s3 cp "$src/hello.txt" "s3://$b1/meta/anon.txt" --no-sign-request >/dev/null 2>&1; then
	fail "anonymous upload succeeded"
fi
ok
aws s3api delete-bucket-policy --bucket "$b1"
if aws s3 ls "s3://$b1/meta/" --no-sign-request >/dev/null 2>&1; then fail "anonymous ls after policy removal"; fi
ok

step "s3api put-bucket-cors / get-object --range / checksum algorithms"
cat >"$tmp/cors.json" <<'EOF'
{"CORSRules":[{"AllowedOrigins":["https://ui.example.com"],"AllowedMethods":["GET","PUT"],"AllowedHeaders":["*"],"MaxAgeSeconds":60}]}
EOF
aws s3api put-bucket-cors --bucket "$b1" --cors-configuration "file://$tmp/cors.json"
eq "$(aws s3api get-bucket-cors --bucket "$b1" --query 'CORSRules[0].AllowedOrigins[0]' --output text)" https://ui.example.com "cors origin"
aws s3api get-object --bucket "$b1" --key meta/hello.txt --range bytes=0-4 "$tmp/range.out" >/dev/null
eq "$(cat "$tmp/range.out")" hello "get-object --range"
for algo in CRC32 CRC32C SHA1 SHA256 CRC64NVME; do
	aws s3 cp "$src/big.bin" "s3://$b1/ck/$algo.bin" --checksum-algorithm "$algo" >/dev/null
	v="$(aws s3api head-object --bucket "$b1" --key "ck/$algo.bin" --checksum-mode ENABLED --query "Checksum$algo" --output text)"
	[[ -n "$v" && "$v" != None ]] || fail "checksum $algo missing on multipart upload"
	ok
	aws s3 cp "s3://$b1/ck/$algo.bin" "$tmp/ck.bin" >/dev/null
	cmp "$src/big.bin" "$tmp/ck.bin" || fail "checksum $algo round trip"
	ok
done
aws s3 rm "s3://$b1/ck/" --recursive >/dev/null

# --- s3api: versioning / attributes ------------------------------------------------------
step "s3api create-bucket / put-bucket-versioning"
aws s3api create-bucket --bucket "$bv" >/dev/null
aws s3api put-bucket-versioning --bucket "$bv" --versioning-configuration Status=Enabled
eq "$(aws s3api get-bucket-versioning --bucket "$bv" --query Status --output text)" Enabled "versioning status"

step "s3api put-object versions / list-object-versions"
printf 'v1' >"$tmp/v1"
printf 'version two' >"$tmp/v2"
v1="$(aws s3api put-object --bucket "$bv" --key doc.txt --body "$tmp/v1" --query VersionId --output text)"
v2="$(aws s3api put-object --bucket "$bv" --key doc.txt --body "$tmp/v2" --checksum-algorithm SHA256 --query VersionId --output text)"
[[ -n "$v1" && -n "$v2" && "$v1" != "$v2" && "$v1" != None ]] || fail "version ids [$v1] [$v2]"
ok
aws s3 rm "s3://$bv/doc.txt" >/dev/null
lov="$(aws s3api list-object-versions --bucket "$bv" --output json)"
eq "$(json 'len(d["Versions"])' <<<"$lov")" 2 "two versions"
eq "$(json 'len(d["DeleteMarkers"])' <<<"$lov")" 1 "one delete marker"
eq "$(json 'd["DeleteMarkers"][0]["IsLatest"]' <<<"$lov")" True "marker is latest"
order="$(python -c 'import json,sys; d=json.load(sys.stdin); print(" ".join(v["VersionId"] for v in d["Versions"]))' <<<"$lov")"
eq "$order" "$v2 $v1" "versions newest first"
if aws s3api head-object --bucket "$bv" --key doc.txt >/dev/null 2>&1; then fail "head of deleted key succeeded"; fi
ok

step "s3api head-object / get-object with --version-id"
head="$(aws s3api head-object --bucket "$bv" --key doc.txt --version-id "$v1" --output json)"
eq "$(json 'd["ContentLength"]' <<<"$head")" 2 "v1 length"
eq "$(json 'd["VersionId"]' <<<"$head")" "$v1" "v1 version id"
aws s3api get-object --bucket "$bv" --key doc.txt --version-id "$v2" "$tmp/v2.down" >/dev/null
cmp "$tmp/v2" "$tmp/v2.down" || fail "get-object --version-id"
ok
head="$(aws s3api head-object --bucket "$bv" --key doc.txt --version-id "$v2" --checksum-mode ENABLED --output json)"
want="$(python -c 'import base64,hashlib,sys; print(base64.b64encode(hashlib.sha256(open(sys.argv[1],"rb").read()).digest()).decode())' "$tmp/v2")"
eq "$(json 'd["ChecksumSHA256"]' <<<"$head")" "$want" "head-object checksum"

step "s3api get-object-attributes"
attrs="$(aws s3api get-object-attributes --bucket "$bv" --key doc.txt --version-id "$v2" \
	--object-attributes ETag ObjectSize StorageClass Checksum --output json)"
eq "$(json 'd["ObjectSize"]' <<<"$attrs")" 11 "attributes size"
eq "$(json 'd["StorageClass"]' <<<"$attrs")" STANDARD "attributes storage class"
eq "$(json 'd["Checksum"]["ChecksumSHA256"]' <<<"$attrs")" "$want" "attributes checksum"
eq "$(json 'd["ETag"]' <<<"$attrs")" "$(python -c 'import hashlib,sys; print(hashlib.md5(open(sys.argv[1],"rb").read()).hexdigest())' "$tmp/v2")" "attributes etag (unquoted)"
aws s3 cp "$src/big.bin" "s3://$bv/big.bin" >/dev/null
attrs="$(aws s3api get-object-attributes --bucket "$bv" --key big.bin --object-attributes ObjectParts ObjectSize --output json)"
eq "$(json 'd["ObjectParts"]["TotalPartsCount"]' <<<"$attrs")" 3 "attributes parts count"
eq "$(json 'd["ObjectSize"]' <<<"$attrs")" $((20 * 1024 * 1024)) "attributes big size"

step "s3api tagging / list-objects-v2"
aws s3api put-object-tagging --bucket "$bv" --key big.bin --tagging 'TagSet=[{Key=env,Value=test}]' >/dev/null
eq "$(aws s3api get-object-tagging --bucket "$bv" --key big.bin --query 'TagSet[0].Value' --output text)" test "object tag"
eq "$(aws s3api list-objects-v2 --bucket "$bv" --query 'length(Contents)' --output text)" 1 "list-objects-v2 count"

# --- rb --force -------------------------------------------------------------------------------
step "s3 rb --force"
aws s3 rb "s3://$b1" --force >/dev/null
aws s3 rb "s3://$b2" --force >/dev/null
out="$(aws s3 ls)"
not_contains "$out" "$b1" "b1 removed"
not_contains "$out" "$b2" "b2 removed"
# A versioned bucket still holds versions: rb --force must fail like on AWS.
if aws s3 rb "s3://$bv" --force >/dev/null 2>&1; then fail "rb --force of a versioned bucket with versions succeeded"; fi
ok

printf 'awscli: %d checks passed\n' "$pass"
