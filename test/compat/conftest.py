"""Shared fixtures for the gos3 boto3 compatibility suite.

The server is started by run.sh, which exports:

    GOS3_COMPAT_ENDPOINT    e.g. http://127.0.0.1:41234
    GOS3_COMPAT_ACCESS_KEY  root access key
    GOS3_COMPAT_SECRET_KEY  root secret key
    GOS3_COMPAT_REGION      region (us-east-1)

Every test that needs a bucket gets a fresh, uniquely named one which is
emptied (all versions, delete markers and multipart uploads) and deleted
afterwards.
"""

import os
import uuid

import boto3
import pytest
from botocore import UNSIGNED
from botocore.config import Config


def _env(name):
    value = os.environ.get(name)
    if not value:
        pytest.exit(f"{name} is not set; run the suite via test/compat/run.sh", returncode=2)
    return value


@pytest.fixture(scope="session")
def endpoint():
    return _env("GOS3_COMPAT_ENDPOINT")


@pytest.fixture(scope="session")
def region():
    return os.environ.get("GOS3_COMPAT_REGION", "us-east-1")


@pytest.fixture(scope="session")
def credentials():
    return _env("GOS3_COMPAT_ACCESS_KEY"), _env("GOS3_COMPAT_SECRET_KEY")


def make_client(endpoint, region, credentials, **config):
    """A path-style boto3 S3 client against gos3."""
    ak, sk = credentials
    cfg = {
        "s3": {"addressing_style": "path"},
        "retries": {"max_attempts": 1, "mode": "standard"},
        "signature_version": "s3v4",
    }
    cfg.update(config)
    return boto3.client(
        "s3",
        endpoint_url=endpoint,
        region_name=region,
        aws_access_key_id=ak,
        aws_secret_access_key=sk,
        config=Config(**cfg),
    )


@pytest.fixture(scope="session")
def s3(endpoint, region, credentials):
    return make_client(endpoint, region, credentials)


@pytest.fixture(scope="session")
def s3_anon(endpoint, region):
    """An unsigned (anonymous) client."""
    return boto3.client(
        "s3",
        endpoint_url=endpoint,
        region_name=region,
        config=Config(
            signature_version=UNSIGNED,
            s3={"addressing_style": "path"},
            retries={"max_attempts": 1, "mode": "standard"},
        ),
    )


@pytest.fixture(scope="session")
def s3_resource(endpoint, region, credentials):
    ak, sk = credentials
    return boto3.resource(
        "s3",
        endpoint_url=endpoint,
        region_name=region,
        aws_access_key_id=ak,
        aws_secret_access_key=sk,
        config=Config(s3={"addressing_style": "path"}, retries={"max_attempts": 1}),
    )


def unique_name(prefix="compat"):
    return f"{prefix}-{uuid.uuid4().hex[:16]}"


def empty_and_delete(s3, bucket):
    """Remove every version, delete marker and upload, then the bucket."""
    try:
        s3.delete_bucket_policy(Bucket=bucket)
    except Exception:
        pass
    for page in s3.get_paginator("list_multipart_uploads").paginate(Bucket=bucket):
        for up in page.get("Uploads", []):
            s3.abort_multipart_upload(Bucket=bucket, Key=up["Key"], UploadId=up["UploadId"])
    for page in s3.get_paginator("list_object_versions").paginate(Bucket=bucket):
        objs = [
            {"Key": v["Key"], "VersionId": v["VersionId"]}
            for v in page.get("Versions", []) + page.get("DeleteMarkers", [])
        ]
        if objs:
            s3.delete_objects(Bucket=bucket, Delete={"Objects": objs, "Quiet": True})
    s3.delete_bucket(Bucket=bucket)


@pytest.fixture
def bucket(s3):
    name = unique_name()
    s3.create_bucket(Bucket=name)
    yield name
    empty_and_delete(s3, name)


@pytest.fixture
def versioned_bucket(s3, bucket):
    s3.put_bucket_versioning(Bucket=bucket, VersioningConfiguration={"Status": "Enabled"})
    return bucket


def error_code(exc_info):
    """The S3 error code of a botocore ClientError captured by pytest.raises."""
    return exc_info.value.response["Error"]["Code"]


def status_code(exc_info):
    return exc_info.value.response["ResponseMetadata"]["HTTPStatusCode"]
