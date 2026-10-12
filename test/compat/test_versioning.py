"""Bucket versioning through boto3."""

import pytest
from botocore.exceptions import ClientError

from conftest import error_code, status_code


def test_versioning_status_round_trip(s3, bucket):
    assert "Status" not in s3.get_bucket_versioning(Bucket=bucket)
    s3.put_bucket_versioning(Bucket=bucket, VersioningConfiguration={"Status": "Enabled"})
    assert s3.get_bucket_versioning(Bucket=bucket)["Status"] == "Enabled"
    s3.put_bucket_versioning(Bucket=bucket, VersioningConfiguration={"Status": "Suspended"})
    assert s3.get_bucket_versioning(Bucket=bucket)["Status"] == "Suspended"
    with pytest.raises(ClientError) as ei:
        s3.put_bucket_versioning(Bucket=bucket, VersioningConfiguration={"Status": "Bogus"})
    assert error_code(ei) in ("MalformedXML", "IllegalVersioningConfigurationException")


def test_unversioned_has_no_version_id(s3, bucket):
    resp = s3.put_object(Bucket=bucket, Key="k", Body=b"x")
    assert "VersionId" not in resp
    assert "VersionId" not in s3.head_object(Bucket=bucket, Key="k")
    versions = s3.list_object_versions(Bucket=bucket)["Versions"]
    assert versions[0]["VersionId"] == "null"
    assert versions[0]["IsLatest"] is True


def test_versions_and_delete_markers(s3, versioned_bucket):
    bucket = versioned_bucket
    v1 = s3.put_object(Bucket=bucket, Key="doc", Body=b"one")["VersionId"]
    v2 = s3.put_object(Bucket=bucket, Key="doc", Body=b"two")["VersionId"]
    assert v1 and v2 and v1 != v2 and "null" not in (v1, v2)

    assert s3.get_object(Bucket=bucket, Key="doc")["Body"].read() == b"two"
    got = s3.get_object(Bucket=bucket, Key="doc", VersionId=v1)
    assert got["Body"].read() == b"one"
    assert got["VersionId"] == v1
    assert s3.head_object(Bucket=bucket, Key="doc", VersionId=v1)["ContentLength"] == 3

    d = s3.delete_object(Bucket=bucket, Key="doc")
    assert d["DeleteMarker"] is True
    marker = d["VersionId"]

    with pytest.raises(ClientError) as ei:
        s3.get_object(Bucket=bucket, Key="doc")
    assert error_code(ei) == "NoSuchKey"
    assert ei.value.response["ResponseMetadata"]["HTTPHeaders"].get("x-amz-delete-marker") == "true"
    with pytest.raises(ClientError) as ei:
        s3.head_object(Bucket=bucket, Key="doc")
    assert status_code(ei) == 404
    with pytest.raises(ClientError) as ei:
        s3.get_object(Bucket=bucket, Key="doc", VersionId=marker)
    assert status_code(ei) == 405

    resp = s3.list_object_versions(Bucket=bucket)
    assert [(v["VersionId"], v["IsLatest"]) for v in resp["Versions"]] == [(v2, False), (v1, False)]
    assert [(m["VersionId"], m["IsLatest"]) for m in resp["DeleteMarkers"]] == [(marker, True)]
    assert "Contents" not in s3.list_objects_v2(Bucket=bucket)

    # Removing the delete marker restores the object.
    r = s3.delete_object(Bucket=bucket, Key="doc", VersionId=marker)
    assert r["DeleteMarker"] is True
    assert r["VersionId"] == marker
    assert s3.get_object(Bucket=bucket, Key="doc")["Body"].read() == b"two"

    # Deleting a specific version.
    s3.delete_object(Bucket=bucket, Key="doc", VersionId=v2)
    assert s3.get_object(Bucket=bucket, Key="doc")["Body"].read() == b"one"
    with pytest.raises(ClientError) as ei:
        s3.get_object(Bucket=bucket, Key="doc", VersionId=v2)
    assert error_code(ei) == "NoSuchVersion"


def test_suspended_null_version(s3, versioned_bucket):
    bucket = versioned_bucket
    v1 = s3.put_object(Bucket=bucket, Key="k", Body=b"v1")["VersionId"]
    s3.put_bucket_versioning(Bucket=bucket, VersioningConfiguration={"Status": "Suspended"})
    r = s3.put_object(Bucket=bucket, Key="k", Body=b"null-1")
    assert r.get("VersionId", "null") == "null"
    s3.put_object(Bucket=bucket, Key="k", Body=b"null-2")
    versions = s3.list_object_versions(Bucket=bucket)["Versions"]
    assert [v["VersionId"] for v in versions] == ["null", v1]
    assert s3.get_object(Bucket=bucket, Key="k", VersionId="null")["Body"].read() == b"null-2"


def test_list_object_versions_paginator(s3, versioned_bucket):
    bucket = versioned_bucket
    expected = []
    for k in ("a", "b", "c"):
        for i in range(4):
            expected.append((k, s3.put_object(Bucket=bucket, Key=k, Body=f"{k}{i}".encode())["VersionId"]))
    s3.delete_object(Bucket=bucket, Key="b")
    versions, markers = [], []
    pages = 0
    for page in s3.get_paginator("list_object_versions").paginate(Bucket=bucket, PaginationConfig={"PageSize": 5}):
        pages += 1
        versions += [(v["Key"], v["VersionId"]) for v in page.get("Versions", [])]
        markers += [m["Key"] for m in page.get("DeleteMarkers", [])]
    assert pages >= 3
    assert markers == ["b"]
    # Newest first within a key.
    want = []
    for k in ("a", "b", "c"):
        want += list(reversed([e for e in expected if e[0] == k]))
    assert versions == want

    resp = s3.list_object_versions(Bucket=bucket, Prefix="c", MaxKeys=2)
    assert resp["IsTruncated"] is True
    assert resp["NextKeyMarker"] == "c"
    assert resp["NextVersionIdMarker"]
    resp2 = s3.list_object_versions(
        Bucket=bucket, Prefix="c", KeyMarker=resp["NextKeyMarker"], VersionIdMarker=resp["NextVersionIdMarker"]
    )
    assert len(resp2["Versions"]) == 2


def test_copy_specific_version(s3, versioned_bucket):
    bucket = versioned_bucket
    v1 = s3.put_object(Bucket=bucket, Key="src", Body=b"first")["VersionId"]
    s3.put_object(Bucket=bucket, Key="src", Body=b"second")
    r = s3.copy_object(Bucket=bucket, Key="dst", CopySource={"Bucket": bucket, "Key": "src", "VersionId": v1})
    assert r["CopySourceVersionId"] == v1
    assert r["VersionId"]
    assert s3.get_object(Bucket=bucket, Key="dst")["Body"].read() == b"first"
