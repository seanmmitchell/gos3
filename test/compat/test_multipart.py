"""Multipart uploads: s3transfer managed transfers and the low-level API."""

import hashlib
import os

import pytest
from boto3.s3.transfer import TransferConfig
from botocore.exceptions import ClientError

from conftest import error_code

MiB = 1024 * 1024


@pytest.fixture(scope="module")
def big_file(tmp_path_factory):
    """A 64 MiB file of pseudo-random data (not compressible, not uniform)."""
    p = tmp_path_factory.mktemp("big") / "64mib.bin"
    seed = hashlib.sha256(b"gos3").digest()
    block = b"".join(hashlib.sha256(seed + i.to_bytes(4, "big")).digest() for i in range(32768))  # 1 MiB
    with open(p, "wb") as f:
        for i in range(64):
            f.write(block[i:] + block[:i])
    return p


def _sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(MiB), b""):
            h.update(chunk)
    return h.hexdigest()


@pytest.mark.parametrize("checksum", [None, "CRC32", "SHA256"])
def test_upload_download_file_multipart(s3, bucket, big_file, tmp_path, checksum):
    cfg = TransferConfig(multipart_threshold=8 * MiB, multipart_chunksize=8 * MiB, max_concurrency=8)
    extra = {"Metadata": {"origin": "s3transfer"}, "ContentType": "application/x-test"}
    if checksum:
        extra["ChecksumAlgorithm"] = checksum
    s3.upload_file(str(big_file), bucket, "big.bin", Config=cfg, ExtraArgs=extra)

    head = s3.head_object(Bucket=bucket, Key="big.bin", ChecksumMode="ENABLED")
    assert head["ContentLength"] == 64 * MiB
    assert head["ETag"].endswith('-8"'), head["ETag"]
    assert head["Metadata"] == {"origin": "s3transfer"}
    assert head["ContentType"] == "application/x-test"
    assert head["PartsCount"] == 8
    if checksum:
        assert head[f"Checksum{checksum}"]

    out = tmp_path / "out.bin"
    s3.download_file(bucket, "big.bin", str(out), Config=cfg)
    assert os.path.getsize(out) == 64 * MiB
    assert _sha256_file(out) == _sha256_file(big_file)


def test_multipart_copy_via_transfer(s3, bucket, big_file):
    cfg = TransferConfig(multipart_threshold=8 * MiB, multipart_chunksize=16 * MiB, max_concurrency=4)
    s3.upload_file(str(big_file), bucket, "src.bin", Config=cfg)
    # s3transfer copies objects above the threshold with UploadPartCopy.
    s3.copy({"Bucket": bucket, "Key": "src.bin"}, bucket, "dst.bin", Config=cfg)
    head = s3.head_object(Bucket=bucket, Key="dst.bin")
    assert head["ContentLength"] == 64 * MiB
    assert head["ETag"].endswith('-4"')
    got = s3.get_object(Bucket=bucket, Key="dst.bin", Range="bytes=33554430-33554440")["Body"].read()
    with open(big_file, "rb") as f:
        f.seek(33554430)
        assert got == f.read(11)


def test_low_level_multipart(s3, bucket):
    key = "manual.bin"
    up = s3.create_multipart_upload(Bucket=bucket, Key=key, Metadata={"m": "1"}, ContentType="text/x-mp")
    uid = up["UploadId"]
    uploads = s3.list_multipart_uploads(Bucket=bucket)["Uploads"]
    assert [(u["Key"], u["UploadId"]) for u in uploads] == [(key, uid)]

    p1 = b"1" * (5 * MiB)
    p2 = b"2" * (5 * MiB)
    p3 = b"3" * 1234
    etags = {}
    # Upload out of order and overwrite a part.
    etags[3] = s3.upload_part(Bucket=bucket, Key=key, UploadId=uid, PartNumber=3, Body=p3)["ETag"]
    s3.upload_part(Bucket=bucket, Key=key, UploadId=uid, PartNumber=1, Body=b"x" * (5 * MiB))
    etags[1] = s3.upload_part(Bucket=bucket, Key=key, UploadId=uid, PartNumber=1, Body=p1)["ETag"]
    etags[2] = s3.upload_part(Bucket=bucket, Key=key, UploadId=uid, PartNumber=2, Body=p2)["ETag"]

    pages = list(
        s3.get_paginator("list_parts").paginate(Bucket=bucket, Key=key, UploadId=uid, PaginationConfig={"PageSize": 2})
    )
    listed = [(p["PartNumber"], p["ETag"], p["Size"]) for page in pages for p in page["Parts"]]
    assert listed == [(1, etags[1], len(p1)), (2, etags[2], len(p2)), (3, etags[3], len(p3))]
    assert len(pages) == 2

    # Too small non-final part.
    with pytest.raises(ClientError) as ei:
        s3.complete_multipart_upload(
            Bucket=bucket,
            Key=key,
            UploadId=uid,
            MultipartUpload={"Parts": [{"PartNumber": 3, "ETag": etags[3]}, {"PartNumber": 2, "ETag": etags[2]}]},
        )
    assert error_code(ei) == "InvalidPartOrder"
    with pytest.raises(ClientError) as ei:
        s3.complete_multipart_upload(
            Bucket=bucket,
            Key=key,
            UploadId=uid,
            MultipartUpload={"Parts": [{"PartNumber": 1, "ETag": '"0000"'}]},
        )
    assert error_code(ei) == "InvalidPart"

    done = s3.complete_multipart_upload(
        Bucket=bucket,
        Key=key,
        UploadId=uid,
        MultipartUpload={"Parts": [{"PartNumber": n, "ETag": etags[n]} for n in (1, 2, 3)]},
    )
    md5s = b"".join(hashlib.md5(p).digest() for p in (p1, p2, p3))
    assert done["ETag"] == f'"{hashlib.md5(md5s).hexdigest()}-3"'
    assert done["Key"] == key
    assert done["Bucket"] == bucket

    got = s3.get_object(Bucket=bucket, Key=key)
    assert got["Body"].read() == p1 + p2 + p3
    assert got["Metadata"] == {"m": "1"}
    assert got["ContentType"] == "text/x-mp"
    part2 = s3.get_object(Bucket=bucket, Key=key, PartNumber=2)
    assert part2["Body"].read() == p2
    assert part2["PartsCount"] == 3
    assert part2["ContentRange"] == f"bytes {len(p1)}-{len(p1) + len(p2) - 1}/{len(p1) + len(p2) + len(p3)}"

    with pytest.raises(ClientError) as ei:
        s3.list_parts(Bucket=bucket, Key=key, UploadId=uid)
    assert error_code(ei) == "NoSuchUpload"


def test_entity_too_small(s3, bucket):
    up = s3.create_multipart_upload(Bucket=bucket, Key="small")
    uid = up["UploadId"]
    e1 = s3.upload_part(Bucket=bucket, Key="small", UploadId=uid, PartNumber=1, Body=b"a")["ETag"]
    e2 = s3.upload_part(Bucket=bucket, Key="small", UploadId=uid, PartNumber=2, Body=b"b")["ETag"]
    with pytest.raises(ClientError) as ei:
        s3.complete_multipart_upload(
            Bucket=bucket,
            Key="small",
            UploadId=uid,
            MultipartUpload={"Parts": [{"PartNumber": 1, "ETag": e1}, {"PartNumber": 2, "ETag": e2}]},
        )
    assert error_code(ei) == "EntityTooSmall"
    s3.abort_multipart_upload(Bucket=bucket, Key="small", UploadId=uid)


def test_abort_and_list_uploads_paginator(s3, bucket):
    ids = {}
    for i in range(5):
        k = f"up/{i}"
        ids[k] = s3.create_multipart_upload(Bucket=bucket, Key=k)["UploadId"]
    seen = []
    for page in s3.get_paginator("list_multipart_uploads").paginate(
        Bucket=bucket, Prefix="up/", PaginationConfig={"PageSize": 2}
    ):
        seen += [(u["Key"], u["UploadId"]) for u in page.get("Uploads", [])]
    assert sorted(seen) == sorted(ids.items())
    for k, uid in ids.items():
        s3.abort_multipart_upload(Bucket=bucket, Key=k, UploadId=uid)
    assert "Uploads" not in s3.list_multipart_uploads(Bucket=bucket)
    with pytest.raises(ClientError) as ei:
        s3.upload_part(Bucket=bucket, Key="up/0", UploadId=ids["up/0"], PartNumber=1, Body=b"x")
    assert error_code(ei) == "NoSuchUpload"


def test_upload_part_copy_with_range(s3, bucket):
    src = bytes(range(256)) * (24 * 1024)  # 6 MiB
    s3.put_object(Bucket=bucket, Key="src", Body=src)
    up = s3.create_multipart_upload(Bucket=bucket, Key="dst")
    uid = up["UploadId"]
    r1 = s3.upload_part_copy(
        Bucket=bucket,
        Key="dst",
        UploadId=uid,
        PartNumber=1,
        CopySource={"Bucket": bucket, "Key": "src"},
        CopySourceRange="bytes=0-5242879",
    )
    r2 = s3.upload_part_copy(
        Bucket=bucket,
        Key="dst",
        UploadId=uid,
        PartNumber=2,
        CopySource={"Bucket": bucket, "Key": "src"},
        CopySourceRange="bytes=5242880-6291455",
    )
    parts = [
        {"PartNumber": 1, "ETag": r1["CopyPartResult"]["ETag"]},
        {"PartNumber": 2, "ETag": r2["CopyPartResult"]["ETag"]},
    ]
    s3.complete_multipart_upload(Bucket=bucket, Key="dst", UploadId=uid, MultipartUpload={"Parts": parts})
    assert s3.get_object(Bucket=bucket, Key="dst")["Body"].read() == src
