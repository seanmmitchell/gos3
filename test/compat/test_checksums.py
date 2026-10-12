"""Flexible checksums (x-amz-checksum-*) through boto3.

botocore computes CRC32C and CRC64NVME through awscrt (botocore[crt]).
"""

import base64
import hashlib
import io
import zlib

import pytest
from botocore.exceptions import ClientError

from conftest import error_code, make_client

ALGORITHMS = ["CRC32", "CRC32C", "SHA1", "SHA256", "CRC64NVME"]


def _crc32(data):
    return base64.b64encode(zlib.crc32(data).to_bytes(4, "big")).decode()


def _sha256(data):
    return base64.b64encode(hashlib.sha256(data).digest()).decode()


def _sha1(data):
    return base64.b64encode(hashlib.sha1(data).digest()).decode()


EXPECTED = {"CRC32": _crc32, "SHA1": _sha1, "SHA256": _sha256}


@pytest.mark.parametrize("algo", ALGORITHMS)
def test_put_object_checksum(s3, bucket, algo):
    data = b"checksum payload " * 1000
    resp = s3.put_object(Bucket=bucket, Key=f"ck-{algo}", Body=data, ChecksumAlgorithm=algo)
    field = f"Checksum{algo}"
    assert resp.get(field), resp
    if algo in EXPECTED:
        assert resp[field] == EXPECTED[algo](data)

    head = s3.head_object(Bucket=bucket, Key=f"ck-{algo}", ChecksumMode="ENABLED")
    assert head[field] == resp[field]
    assert head.get("ChecksumType") == "FULL_OBJECT"

    got = s3.get_object(Bucket=bucket, Key=f"ck-{algo}", ChecksumMode="ENABLED")
    # botocore validates the response checksum while streaming.
    assert got["Body"].read() == data
    assert got[field] == resp[field]

    attrs = s3.get_object_attributes(Bucket=bucket, Key=f"ck-{algo}", ObjectAttributes=["Checksum", "ObjectSize", "ETag"])
    assert attrs["Checksum"][field] == resp[field]
    assert attrs["ObjectSize"] == len(data)


@pytest.mark.parametrize("algo", ALGORITHMS)
def test_put_object_checksum_seekable_file(s3, bucket, algo, tmp_path):
    p = tmp_path / "f.bin"
    data = bytes(range(256)) * 300
    p.write_bytes(data)
    with open(p, "rb") as f:
        resp = s3.put_object(Bucket=bucket, Key="file", Body=f, ChecksumAlgorithm=algo)
    assert resp[f"Checksum{algo}"]
    assert s3.get_object(Bucket=bucket, Key="file")["Body"].read() == data


@pytest.mark.parametrize("algo", ["CRC32", "SHA256"])
def test_explicit_checksum_value(s3, bucket, algo):
    data = b"explicit checksum"
    value = EXPECTED[algo](data)
    kwargs = {f"Checksum{algo}": value}
    resp = s3.put_object(Bucket=bucket, Key="explicit", Body=data, **kwargs)
    assert resp[f"Checksum{algo}"] == value

    bad = EXPECTED[algo](b"something else")
    with pytest.raises(ClientError) as ei:
        s3.put_object(Bucket=bucket, Key="explicit-bad", Body=data, **{f"Checksum{algo}": bad})
    assert error_code(ei) in ("BadDigest", "InvalidRequest")
    with pytest.raises(ClientError):
        s3.head_object(Bucket=bucket, Key="explicit-bad")


def test_content_md5(s3, bucket):
    data = b"md5 checked"
    md5 = base64.b64encode(hashlib.md5(data).digest()).decode()
    s3.put_object(Bucket=bucket, Key="md5", Body=data, ContentMD5=md5)
    bad = base64.b64encode(hashlib.md5(b"other").digest()).decode()
    with pytest.raises(ClientError) as ei:
        s3.put_object(Bucket=bucket, Key="md5-bad", Body=data, ContentMD5=bad)
    assert error_code(ei) == "BadDigest"


@pytest.mark.parametrize(
    "calc",
    ["when_required", "when_supported"],
)
def test_checksum_calculation_modes(endpoint, region, credentials, bucket, calc):
    c = make_client(
        endpoint,
        region,
        credentials,
        request_checksum_calculation=calc,
        response_checksum_validation=calc,
    )
    data = b"mode " * 5000
    c.put_object(Bucket=bucket, Key="m", Body=data)
    c.upload_fileobj(io.BytesIO(data), bucket, "m2")
    assert c.get_object(Bucket=bucket, Key="m")["Body"].read() == data
    assert c.get_object(Bucket=bucket, Key="m2")["Body"].read() == data


def test_payload_signing_enabled(endpoint, region, credentials, bucket):
    """Signed payload (hex SHA-256 in x-amz-content-sha256)."""
    c = make_client(endpoint, region, credentials, s3={"addressing_style": "path", "payload_signing_enabled": True})
    data = b"signed payload " * 100
    c.put_object(Bucket=bucket, Key="signed", Body=data, ChecksumAlgorithm="SHA256")
    assert c.get_object(Bucket=bucket, Key="signed")["Body"].read() == data


@pytest.mark.parametrize("algo", ALGORITHMS)
def test_multipart_checksums(s3, bucket, algo):
    key = f"mp-{algo}"
    part = 5 * 1024 * 1024
    chunks = [bytes([i]) * part for i in range(2)] + [b"tail"]
    up = s3.create_multipart_upload(Bucket=bucket, Key=key, ChecksumAlgorithm=algo)
    assert up.get("ChecksumAlgorithm") == algo
    uid = up["UploadId"]
    parts = []
    for n, chunk in enumerate(chunks, start=1):
        r = s3.upload_part(Bucket=bucket, Key=key, UploadId=uid, PartNumber=n, Body=chunk, ChecksumAlgorithm=algo)
        assert r[f"Checksum{algo}"]
        parts.append({"PartNumber": n, "ETag": r["ETag"], f"Checksum{algo}": r[f"Checksum{algo}"]})

    listed = s3.list_parts(Bucket=bucket, Key=key, UploadId=uid)
    assert [p[f"Checksum{algo}"] for p in listed["Parts"]] == [p[f"Checksum{algo}"] for p in parts]
    assert listed.get("ChecksumAlgorithm") == algo

    done = s3.complete_multipart_upload(Bucket=bucket, Key=key, UploadId=uid, MultipartUpload={"Parts": parts})
    value = done[f"Checksum{algo}"]
    if algo == "CRC64NVME":
        # CRC64NVME only supports full-object checksums.
        assert "-" not in value
    else:
        assert value.endswith("-3")

    head = s3.head_object(Bucket=bucket, Key=key, ChecksumMode="ENABLED")
    assert head[f"Checksum{algo}"] == value
    head = s3.head_object(Bucket=bucket, Key=key, ChecksumMode="ENABLED", PartNumber=2)
    assert head[f"Checksum{algo}"] == parts[1][f"Checksum{algo}"]
    assert head["PartsCount"] == 3
    assert s3.get_object(Bucket=bucket, Key=key)["Body"].read() == b"".join(chunks)

    attrs = s3.get_object_attributes(
        Bucket=bucket, Key=key, ObjectAttributes=["Checksum", "ObjectParts", "ObjectSize"]
    )
    assert attrs["ObjectSize"] == sum(len(c) for c in chunks)
    assert attrs["ObjectParts"]["TotalPartsCount"] == 3


@pytest.mark.parametrize("algo", ["CRC32", "CRC32C", "CRC64NVME"])
def test_multipart_full_object_checksum(s3, bucket, algo):
    key = f"full-{algo}"
    part = 5 * 1024 * 1024
    chunks = [b"a" * part, b"b" * 100]
    up = s3.create_multipart_upload(Bucket=bucket, Key=key, ChecksumAlgorithm=algo, ChecksumType="FULL_OBJECT")
    uid = up["UploadId"]
    parts = []
    for n, chunk in enumerate(chunks, start=1):
        r = s3.upload_part(Bucket=bucket, Key=key, UploadId=uid, PartNumber=n, Body=chunk, ChecksumAlgorithm=algo)
        parts.append({"PartNumber": n, "ETag": r["ETag"], f"Checksum{algo}": r[f"Checksum{algo}"]})
    done = s3.complete_multipart_upload(Bucket=bucket, Key=key, UploadId=uid, MultipartUpload={"Parts": parts})
    assert done.get("ChecksumType") == "FULL_OBJECT"
    value = done[f"Checksum{algo}"]
    assert "-" not in value

    # The full-object CRC equals that of a single PUT of the same bytes.
    single = s3.put_object(Bucket=bucket, Key=key + "-single", Body=b"".join(chunks), ChecksumAlgorithm=algo)
    assert single[f"Checksum{algo}"] == value
    if algo == "CRC32":
        assert value == _crc32(b"".join(chunks))
