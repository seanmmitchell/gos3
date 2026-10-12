"""Core bucket, object and listing behaviour through boto3."""

import datetime
import hashlib
import io

import pytest
from botocore.exceptions import ClientError

from conftest import empty_and_delete, error_code, status_code, unique_name


# --- buckets ------------------------------------------------------------------


def test_bucket_lifecycle(s3):
    name = unique_name("bkt")
    resp = s3.create_bucket(Bucket=name)
    assert resp["ResponseMetadata"]["HTTPStatusCode"] == 200
    assert resp["Location"] == f"/{name}"
    try:
        s3.head_bucket(Bucket=name)
        names = [b["Name"] for b in s3.list_buckets()["Buckets"]]
        assert name in names
        loc = s3.get_bucket_location(Bucket=name)
        assert loc["LocationConstraint"] in (None, "", "us-east-1")

        with pytest.raises(ClientError) as ei:
            s3.create_bucket(Bucket=name)
        assert error_code(ei) == "BucketAlreadyOwnedByYou"

        s3.put_object(Bucket=name, Key="k", Body=b"x")
        with pytest.raises(ClientError) as ei:
            s3.delete_bucket(Bucket=name)
        assert error_code(ei) == "BucketNotEmpty"
        s3.delete_object(Bucket=name, Key="k")
    finally:
        s3.delete_bucket(Bucket=name)

    with pytest.raises(ClientError) as ei:
        s3.head_bucket(Bucket=name)
    assert status_code(ei) == 404


def test_create_bucket_with_location_constraint(s3, region):
    name = unique_name("loc")
    if region == "us-east-1":
        s3.create_bucket(Bucket=name)
    else:
        s3.create_bucket(Bucket=name, CreateBucketConfiguration={"LocationConstraint": region})
    empty_and_delete(s3, name)


def test_invalid_bucket_name(s3):
    with pytest.raises(ClientError) as ei:
        s3.create_bucket(Bucket="ab")
    assert error_code(ei) == "InvalidBucketName"


def test_list_buckets_paginator(s3, bucket):
    names = []
    for page in s3.get_paginator("list_buckets").paginate():
        names += [b["Name"] for b in page["Buckets"]]
    assert bucket in names


def test_no_such_bucket(s3):
    with pytest.raises(ClientError) as ei:
        s3.list_objects_v2(Bucket=unique_name("missing"))
    assert error_code(ei) == "NoSuchBucket"
    assert ei.value.response["Error"].get("BucketName")


# --- objects ------------------------------------------------------------------


def test_put_get_head_delete(s3, bucket):
    body = b"hello gos3 from boto3\n" * 100
    put = s3.put_object(Bucket=bucket, Key="dir/hello.txt", Body=body, ContentType="text/plain")
    md5 = hashlib.md5(body).hexdigest()
    assert put["ETag"] == f'"{md5}"'

    got = s3.get_object(Bucket=bucket, Key="dir/hello.txt")
    assert got["Body"].read() == body
    assert got["ContentType"] == "text/plain"
    assert got["ContentLength"] == len(body)
    assert got["ETag"] == f'"{md5}"'
    assert isinstance(got["LastModified"], datetime.datetime)

    head = s3.head_object(Bucket=bucket, Key="dir/hello.txt")
    assert head["ContentLength"] == len(body)
    assert head["ETag"] == put["ETag"]

    s3.delete_object(Bucket=bucket, Key="dir/hello.txt")
    with pytest.raises(ClientError) as ei:
        s3.get_object(Bucket=bucket, Key="dir/hello.txt")
    assert error_code(ei) == "NoSuchKey"
    with pytest.raises(ClientError) as ei:
        s3.head_object(Bucket=bucket, Key="dir/hello.txt")
    assert status_code(ei) == 404
    # Deleting a missing key succeeds.
    s3.delete_object(Bucket=bucket, Key="dir/hello.txt")


def test_empty_object(s3, bucket):
    s3.put_object(Bucket=bucket, Key="empty", Body=b"")
    got = s3.get_object(Bucket=bucket, Key="empty")
    assert got["Body"].read() == b""
    assert got["ContentLength"] == 0
    assert got["ETag"] == '"d41d8cd98f00b204e9800998ecf8427e"'


@pytest.mark.parametrize(
    "key",
    [
        "unicode/żółć ünïcödé 日本語.txt",
        "spaces and+plus%percent",
        "special/!-_.*'()&$@=;:,?#[]{}~^|`",
        "a/b/c/d/e/f/deep.bin",
        "trailing-slash/",
        "double//slash",
        "tab\tand newline\nkey",
    ],
)
def test_odd_keys(s3, bucket, key):
    s3.put_object(Bucket=bucket, Key=key, Body=key.encode())
    assert s3.get_object(Bucket=bucket, Key=key)["Body"].read() == key.encode()
    keys = [o["Key"] for o in s3.list_objects_v2(Bucket=bucket)["Contents"]]
    assert keys == [key]
    keys = [o["Key"] for o in s3.list_objects(Bucket=bucket)["Contents"]]
    assert keys == [key]
    s3.delete_object(Bucket=bucket, Key=key)


def test_upload_fileobj_and_download_fileobj(s3, bucket):
    data = bytes(range(256)) * 4096
    s3.upload_fileobj(io.BytesIO(data), bucket, "obj.bin")
    out = io.BytesIO()
    s3.download_fileobj(bucket, "obj.bin", out)
    assert out.getvalue() == data


def test_resource_api(s3_resource, bucket):
    b = s3_resource.Bucket(bucket)
    b.put_object(Key="r/1", Body=b"one")
    b.put_object(Key="r/2", Body=b"two")
    assert sorted(o.key for o in b.objects.filter(Prefix="r/")) == ["r/1", "r/2"]
    obj = s3_resource.Object(bucket, "r/1")
    assert obj.get()["Body"].read() == b"one"
    assert obj.content_length == 3
    b.objects.filter(Prefix="r/").delete()
    assert list(b.objects.all()) == []


# --- metadata -----------------------------------------------------------------


def test_metadata_and_standard_headers(s3, bucket):
    expires = datetime.datetime(2030, 1, 2, 3, 4, 5, tzinfo=datetime.timezone.utc)
    s3.put_object(
        Bucket=bucket,
        Key="meta",
        Body=b"data",
        Metadata={"color": "Blue", "x-multi-word": "a b  c"},
        ContentType="application/x-custom",
        ContentEncoding="identity",
        ContentDisposition='attachment; filename="m.txt"',
        ContentLanguage="en-GB",
        CacheControl="max-age=60",
        Expires=expires,
    )
    head = s3.head_object(Bucket=bucket, Key="meta")
    assert head["Metadata"]["color"] == "Blue"
    assert head["Metadata"]["x-multi-word"] == "a b  c"
    assert head["ContentType"] == "application/x-custom"
    assert head["ContentEncoding"] == "identity"
    assert head["ContentDisposition"] == 'attachment; filename="m.txt"'
    assert head["ContentLanguage"] == "en-GB"
    assert head["CacheControl"] == "max-age=60"
    assert head["ExpiresString"] == "Wed, 02 Jan 2030 03:04:05 GMT"

    got = s3.get_object(Bucket=bucket, Key="meta")
    assert got["Metadata"]["color"] == "Blue"

    # Response header overrides on GET.
    got = s3.get_object(
        Bucket=bucket,
        Key="meta",
        ResponseContentType="text/csv",
        ResponseContentDisposition="inline",
        ResponseCacheControl="no-store",
        ResponseContentLanguage="de",
        ResponseContentEncoding="gzip",
        ResponseExpires=expires,
    )
    assert got["ContentType"] == "text/csv"
    assert got["ContentDisposition"] == "inline"
    assert got["CacheControl"] == "no-store"
    assert got["ContentLanguage"] == "de"
    assert got["ContentEncoding"] == "gzip"


def test_metadata_too_large(s3, bucket):
    with pytest.raises(ClientError) as ei:
        s3.put_object(Bucket=bucket, Key="big-meta", Body=b"x", Metadata={"k": "v" * 3000})
    assert error_code(ei) == "MetadataTooLarge"


def test_storage_class_reported(s3, bucket):
    s3.put_object(Bucket=bucket, Key="sc", Body=b"x")
    objs = s3.list_objects_v2(Bucket=bucket)["Contents"]
    assert objs[0]["StorageClass"] == "STANDARD"


# --- copy ---------------------------------------------------------------------


def test_copy_object(s3, bucket):
    s3.put_object(Bucket=bucket, Key="src", Body=b"copy me", Metadata={"a": "1"}, ContentType="text/x-a")
    resp = s3.copy_object(Bucket=bucket, Key="dst", CopySource={"Bucket": bucket, "Key": "src"})
    assert resp["CopyObjectResult"]["ETag"] == f'"{hashlib.md5(b"copy me").hexdigest()}"'
    head = s3.head_object(Bucket=bucket, Key="dst")
    assert head["Metadata"] == {"a": "1"}
    assert head["ContentType"] == "text/x-a"

    s3.copy_object(
        Bucket=bucket,
        Key="dst2",
        CopySource=f"{bucket}/src",
        MetadataDirective="REPLACE",
        Metadata={"b": "2"},
        ContentType="text/x-b",
    )
    head = s3.head_object(Bucket=bucket, Key="dst2")
    assert head["Metadata"] == {"b": "2"}
    assert head["ContentType"] == "text/x-b"

    # Copy onto itself without REPLACE is invalid.
    with pytest.raises(ClientError) as ei:
        s3.copy_object(Bucket=bucket, Key="src", CopySource={"Bucket": bucket, "Key": "src"})
    assert error_code(ei) == "InvalidRequest"
    # ... but allowed with REPLACE (metadata update in place).
    s3.copy_object(
        Bucket=bucket,
        Key="src",
        CopySource={"Bucket": bucket, "Key": "src"},
        MetadataDirective="REPLACE",
        Metadata={"c": "3"},
    )
    assert s3.head_object(Bucket=bucket, Key="src")["Metadata"] == {"c": "3"}


def test_copy_unicode_key_and_cross_bucket(s3, bucket):
    other = unique_name()
    s3.create_bucket(Bucket=other)
    try:
        key = "źródło/ключ with space+plus.txt"
        s3.put_object(Bucket=bucket, Key=key, Body=b"payload")
        s3.copy_object(Bucket=other, Key=key, CopySource={"Bucket": bucket, "Key": key})
        assert s3.get_object(Bucket=other, Key=key)["Body"].read() == b"payload"
        # Managed copy (s3transfer) uses CopyObject / UploadPartCopy.
        s3.copy({"Bucket": bucket, "Key": key}, other, "managed-copy")
        assert s3.get_object(Bucket=other, Key="managed-copy")["Body"].read() == b"payload"
    finally:
        empty_and_delete(s3, other)


def test_copy_conditions(s3, bucket):
    put = s3.put_object(Bucket=bucket, Key="src", Body=b"cond")
    etag = put["ETag"]
    s3.copy_object(Bucket=bucket, Key="ok", CopySource=f"{bucket}/src", CopySourceIfMatch=etag)
    with pytest.raises(ClientError) as ei:
        s3.copy_object(Bucket=bucket, Key="no", CopySource=f"{bucket}/src", CopySourceIfMatch='"deadbeef"')
    assert error_code(ei) == "PreconditionFailed"
    with pytest.raises(ClientError) as ei:
        s3.copy_object(Bucket=bucket, Key="no", CopySource=f"{bucket}/src", CopySourceIfNoneMatch=etag)
    assert error_code(ei) == "PreconditionFailed"


def test_copy_missing_source(s3, bucket):
    with pytest.raises(ClientError) as ei:
        s3.copy_object(Bucket=bucket, Key="dst", CopySource=f"{bucket}/missing")
    assert error_code(ei) == "NoSuchKey"


# --- ranges -------------------------------------------------------------------


def test_ranges(s3, bucket):
    data = bytes(range(256)) * 40  # 10240 bytes
    s3.put_object(Bucket=bucket, Key="r", Body=data)

    cases = {
        "bytes=0-0": (data[0:1], "bytes 0-0/10240"),
        "bytes=10-19": (data[10:20], "bytes 10-19/10240"),
        "bytes=10000-": (data[10000:], "bytes 10000-10239/10240"),
        "bytes=-100": (data[-100:], "bytes 10140-10239/10240"),
        "bytes=10000-99999": (data[10000:], "bytes 10000-10239/10240"),
    }
    for rng, (want, content_range) in cases.items():
        got = s3.get_object(Bucket=bucket, Key="r", Range=rng)
        assert got["ResponseMetadata"]["HTTPStatusCode"] == 206, rng
        assert got["Body"].read() == want, rng
        assert got["ContentRange"] == content_range, rng
        assert got["ContentLength"] == len(want), rng
        assert got["AcceptRanges"] == "bytes"

    head = s3.head_object(Bucket=bucket, Key="r", Range="bytes=0-9")
    assert head["ContentLength"] == 10
    assert head["ContentRange"] == "bytes 0-9/10240"

    with pytest.raises(ClientError) as ei:
        s3.get_object(Bucket=bucket, Key="r", Range="bytes=20000-")
    assert error_code(ei) == "InvalidRange"
    assert status_code(ei) == 416


def test_range_on_empty_object(s3, bucket):
    s3.put_object(Bucket=bucket, Key="e", Body=b"")
    with pytest.raises(ClientError) as ei:
        s3.get_object(Bucket=bucket, Key="e", Range="bytes=0-10")
    assert error_code(ei) == "InvalidRange"


def test_part_number_get(s3, bucket):
    s3.put_object(Bucket=bucket, Key="p", Body=b"0123456789")
    got = s3.get_object(Bucket=bucket, Key="p", PartNumber=1)
    assert got["Body"].read() == b"0123456789"
    head = s3.head_object(Bucket=bucket, Key="p", PartNumber=1)
    assert head["ContentLength"] == 10


# --- conditional requests -----------------------------------------------------


def test_conditional_get(s3, bucket):
    put = s3.put_object(Bucket=bucket, Key="c", Body=b"conditional")
    etag = put["ETag"]
    lm = s3.head_object(Bucket=bucket, Key="c")["LastModified"]
    before = lm - datetime.timedelta(hours=1)
    after = lm + datetime.timedelta(hours=1)

    assert s3.get_object(Bucket=bucket, Key="c", IfMatch=etag)["Body"].read() == b"conditional"
    assert s3.get_object(Bucket=bucket, Key="c", IfMatch="*")["Body"].read() == b"conditional"
    assert s3.get_object(Bucket=bucket, Key="c", IfNoneMatch='"other"')["Body"].read()
    assert s3.get_object(Bucket=bucket, Key="c", IfModifiedSince=before)["Body"].read()
    assert s3.get_object(Bucket=bucket, Key="c", IfUnmodifiedSince=after)["Body"].read()

    with pytest.raises(ClientError) as ei:
        s3.get_object(Bucket=bucket, Key="c", IfMatch='"nope"')
    assert error_code(ei) == "PreconditionFailed"
    assert status_code(ei) == 412

    with pytest.raises(ClientError) as ei:
        s3.get_object(Bucket=bucket, Key="c", IfNoneMatch=etag)
    assert status_code(ei) == 304

    with pytest.raises(ClientError) as ei:
        s3.get_object(Bucket=bucket, Key="c", IfModifiedSince=after)
    assert status_code(ei) == 304

    with pytest.raises(ClientError) as ei:
        s3.get_object(Bucket=bucket, Key="c", IfUnmodifiedSince=before)
    assert status_code(ei) == 412

    # If-Match true + If-Unmodified-Since false => 200 (RFC 7232 precedence).
    assert s3.get_object(Bucket=bucket, Key="c", IfMatch=etag, IfUnmodifiedSince=before)["Body"].read()
    # If-None-Match false + If-Modified-Since true => 304.
    with pytest.raises(ClientError) as ei:
        s3.get_object(Bucket=bucket, Key="c", IfNoneMatch=etag, IfModifiedSince=before)
    assert status_code(ei) == 304

    with pytest.raises(ClientError) as ei:
        s3.head_object(Bucket=bucket, Key="c", IfMatch='"nope"')
    assert status_code(ei) == 412
    with pytest.raises(ClientError) as ei:
        s3.head_object(Bucket=bucket, Key="c", IfNoneMatch=etag)
    assert status_code(ei) == 304


def test_conditional_put(s3, bucket):
    # If-None-Match: * creates only when absent.
    s3.put_object(Bucket=bucket, Key="once", Body=b"1", IfNoneMatch="*")
    with pytest.raises(ClientError) as ei:
        s3.put_object(Bucket=bucket, Key="once", Body=b"2", IfNoneMatch="*")
    assert error_code(ei) == "PreconditionFailed"

    etag = s3.head_object(Bucket=bucket, Key="once")["ETag"]
    s3.put_object(Bucket=bucket, Key="once", Body=b"3", IfMatch=etag)
    with pytest.raises(ClientError) as ei:
        s3.put_object(Bucket=bucket, Key="once", Body=b"4", IfMatch=etag)
    assert error_code(ei) == "PreconditionFailed"
    with pytest.raises(ClientError) as ei:
        s3.put_object(Bucket=bucket, Key="absent", Body=b"4", IfMatch=etag)
    assert error_code(ei) == "NoSuchKey"
    assert s3.get_object(Bucket=bucket, Key="once")["Body"].read() == b"3"


# --- listing ------------------------------------------------------------------


@pytest.fixture
def listing_bucket(s3, bucket):
    keys = (
        [f"a/{i:03d}" for i in range(25)]
        + [f"b/x/{i}" for i in range(5)]
        + [f"b/y/{i}" for i in range(5)]
        + ["c", "d.txt", "e/"]
    )
    for k in keys:
        s3.put_object(Bucket=bucket, Key=k, Body=k.encode())
    return bucket, sorted(keys)


def test_list_v2_paginator(s3, listing_bucket):
    bucket, keys = listing_bucket
    pages = list(s3.get_paginator("list_objects_v2").paginate(Bucket=bucket, PaginationConfig={"PageSize": 7}))
    got = [o["Key"] for p in pages for o in p.get("Contents", [])]
    assert got == keys
    assert len(pages) == (len(keys) + 6) // 7
    for p in pages[:-1]:
        assert p["IsTruncated"] is True
        assert p["KeyCount"] == 7
    assert pages[-1]["IsTruncated"] is False


def test_list_v1_paginator(s3, listing_bucket):
    bucket, keys = listing_bucket
    pages = list(s3.get_paginator("list_objects").paginate(Bucket=bucket, PaginationConfig={"PageSize": 6}))
    got = [o["Key"] for p in pages for o in p.get("Contents", [])]
    assert got == keys
    assert len(pages) > 1


def test_list_delimiter(s3, listing_bucket):
    bucket, keys = listing_bucket
    resp = s3.list_objects_v2(Bucket=bucket, Delimiter="/")
    assert [o["Key"] for o in resp["Contents"]] == ["c", "d.txt"]
    assert [p["Prefix"] for p in resp["CommonPrefixes"]] == ["a/", "b/", "e/"]

    resp = s3.list_objects_v2(Bucket=bucket, Delimiter="/", Prefix="b/")
    assert "Contents" not in resp
    assert [p["Prefix"] for p in resp["CommonPrefixes"]] == ["b/x/", "b/y/"]

    resp = s3.list_objects(Bucket=bucket, Delimiter="/", Prefix="e/")
    assert [o["Key"] for o in resp["Contents"]] == ["e/"]


def test_list_delimiter_paginated(s3, listing_bucket):
    bucket, keys = listing_bucket
    for op in ("list_objects", "list_objects_v2"):
        prefixes, contents = [], []
        for page in s3.get_paginator(op).paginate(Bucket=bucket, Delimiter="/", PaginationConfig={"PageSize": 1}):
            prefixes += [p["Prefix"] for p in page.get("CommonPrefixes", [])]
            contents += [o["Key"] for o in page.get("Contents", [])]
        assert prefixes == ["a/", "b/", "e/"], op
        assert contents == ["c", "d.txt"], op


def test_list_start_after_and_max_keys(s3, listing_bucket):
    bucket, keys = listing_bucket
    resp = s3.list_objects_v2(Bucket=bucket, StartAfter="b/y/4", MaxKeys=2)
    assert [o["Key"] for o in resp["Contents"]] == ["c", "d.txt"]
    assert resp["IsTruncated"] is True
    assert resp["StartAfter"] == "b/y/4"
    assert resp["MaxKeys"] == 2

    resp = s3.list_objects(Bucket=bucket, Marker="a/023", MaxKeys=3)
    assert [o["Key"] for o in resp["Contents"]] == ["a/024", "b/x/0", "b/x/1"]
    assert resp["IsTruncated"] is True

    resp = s3.list_objects_v2(Bucket=bucket, MaxKeys=0)
    assert resp["KeyCount"] == 0
    assert "Contents" not in resp


def test_list_fetch_owner_and_encoding(s3, bucket):
    s3.put_object(Bucket=bucket, Key="ünï code", Body=b"x")
    resp = s3.list_objects_v2(Bucket=bucket, FetchOwner=True)
    assert resp["Contents"][0]["Owner"]["ID"]
    # boto3 sends encoding-type=url and decodes keys transparently.
    assert resp["Contents"][0]["Key"] == "ünï code"
    resp = s3.list_objects_v2(Bucket=bucket)
    assert "Owner" not in resp["Contents"][0]


# --- delete_objects -----------------------------------------------------------


def test_delete_objects(s3, bucket):
    keys = [f"del/{i}" for i in range(20)]
    for k in keys:
        s3.put_object(Bucket=bucket, Key=k, Body=b"x")
    resp = s3.delete_objects(
        Bucket=bucket,
        Delete={"Objects": [{"Key": k} for k in keys[:10]] + [{"Key": "del/missing"}]},
    )
    assert sorted(d["Key"] for d in resp["Deleted"]) == sorted(keys[:10] + ["del/missing"])
    assert "Errors" not in resp

    resp = s3.delete_objects(Bucket=bucket, Delete={"Objects": [{"Key": k} for k in keys[10:]], "Quiet": True})
    assert "Deleted" not in resp
    assert "Errors" not in resp
    assert s3.list_objects_v2(Bucket=bucket)["KeyCount"] == 0


def test_delete_objects_too_many(s3, bucket):
    with pytest.raises(ClientError) as ei:
        s3.delete_objects(Bucket=bucket, Delete={"Objects": [{"Key": f"k{i}"} for i in range(1001)]})
    assert error_code(ei) in ("MalformedXML", "InvalidRequest")


def test_delete_objects_versioned(s3, versioned_bucket):
    bucket = versioned_bucket
    v1 = s3.put_object(Bucket=bucket, Key="k", Body=b"1")["VersionId"]
    s3.put_object(Bucket=bucket, Key="k", Body=b"2")
    resp = s3.delete_objects(Bucket=bucket, Delete={"Objects": [{"Key": "k"}]})
    d = resp["Deleted"][0]
    assert d["DeleteMarker"] is True
    assert d["DeleteMarkerVersionId"]
    resp = s3.delete_objects(Bucket=bucket, Delete={"Objects": [{"Key": "k", "VersionId": v1}]})
    assert resp["Deleted"][0]["VersionId"] == v1
    with pytest.raises(ClientError) as ei:
        s3.get_object(Bucket=bucket, Key="k", VersionId=v1)
    assert error_code(ei) == "NoSuchVersion"


def test_early_error_does_not_wait_for_100_continue(s3, bucket):
    """botocore sends "Expect: 100-continue" on uploads and waits up to 1 s
    for the server's answer before sending the body; an early error must be
    answered at once instead of after that timeout."""
    import time

    body = b"x" * (2 * 1024 * 1024)
    for _ in range(2):
        start = time.monotonic()
        with pytest.raises(ClientError) as ei:
            s3.put_object(Bucket=unique_name("missing"), Key="k", Body=body)
        assert error_code(ei) == "NoSuchBucket"
        assert time.monotonic() - start < 0.8
    # The connection pool still works afterwards.
    s3.put_object(Bucket=bucket, Key="after", Body=body)


def test_listing_beyond_1000_keys(s3, bucket):
    from concurrent.futures import ThreadPoolExecutor

    keys = [f"many/{i:05d}" for i in range(1105)]
    with ThreadPoolExecutor(16) as pool:
        list(pool.map(lambda k: s3.put_object(Bucket=bucket, Key=k, Body=b""), keys))
    first = s3.list_objects_v2(Bucket=bucket)
    assert first["KeyCount"] == 1000
    assert first["IsTruncated"] is True
    v1 = s3.list_objects(Bucket=bucket)
    assert len(v1["Contents"]) == 1000
    assert v1["IsTruncated"] is True
    assert "NextMarker" not in v1  # only sent with a delimiter
    for op in ("list_objects", "list_objects_v2"):
        got = [o["Key"] for p in s3.get_paginator(op).paginate(Bucket=bucket) for o in p.get("Contents", [])]
        assert got == keys, op


def test_conditional_writes_complete_copy_delete(s3, bucket):
    s3.put_object(Bucket=bucket, Key="exists", Body=b"1")

    def mpu(key):
        uid = s3.create_multipart_upload(Bucket=bucket, Key=key)["UploadId"]
        etag = s3.upload_part(Bucket=bucket, Key=key, UploadId=uid, PartNumber=1, Body=b"x")["ETag"]
        return uid, {"Parts": [{"PartNumber": 1, "ETag": etag}]}

    uid, parts = mpu("exists")
    with pytest.raises(ClientError) as ei:
        s3.complete_multipart_upload(Bucket=bucket, Key="exists", UploadId=uid, MultipartUpload=parts, IfNoneMatch="*")
    assert error_code(ei) == "PreconditionFailed"
    s3.abort_multipart_upload(Bucket=bucket, Key="exists", UploadId=uid)

    uid, parts = mpu("new")
    s3.complete_multipart_upload(Bucket=bucket, Key="new", UploadId=uid, MultipartUpload=parts, IfNoneMatch="*")

    with pytest.raises(ClientError) as ei:
        s3.copy_object(Bucket=bucket, Key="exists", CopySource=f"{bucket}/new", IfNoneMatch="*")
    assert error_code(ei) == "PreconditionFailed"

    with pytest.raises(ClientError) as ei:
        s3.delete_object(Bucket=bucket, Key="exists", IfMatch='"0123"')
    assert error_code(ei) == "PreconditionFailed"
    etag = s3.head_object(Bucket=bucket, Key="exists")["ETag"]
    s3.delete_object(Bucket=bucket, Key="exists", IfMatch=etag)


def test_content_encoding_gzip_is_opaque(s3, bucket):
    import gzip

    data = gzip.compress(b"hello " * 1000)
    s3.put_object(Bucket=bucket, Key="gz", Body=data, ContentEncoding="gzip", ContentType="text/plain")
    got = s3.get_object(Bucket=bucket, Key="gz")
    assert got["ContentEncoding"] == "gzip"
    assert got["ContentLength"] == len(data)
    assert got["Body"].read() == data
