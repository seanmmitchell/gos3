"""Object lock (retention, legal hold) through boto3."""

import datetime

import pytest
from botocore.exceptions import ClientError

from conftest import empty_and_delete, error_code, unique_name


@pytest.fixture
def lock_bucket(s3):
    name = unique_name("lock")
    s3.create_bucket(Bucket=name, ObjectLockEnabledForBucket=True)
    yield name
    # Governance retention can be bypassed by root; legal holds must be lifted.
    for page in s3.get_paginator("list_object_versions").paginate(Bucket=name):
        for v in page.get("Versions", []):
            try:
                s3.put_object_legal_hold(
                    Bucket=name, Key=v["Key"], VersionId=v["VersionId"], LegalHold={"Status": "OFF"}
                )
            except ClientError:
                pass
            s3.delete_object(Bucket=name, Key=v["Key"], VersionId=v["VersionId"], BypassGovernanceRetention=True)
    empty_and_delete(s3, name)


def test_object_lock_configuration(s3, lock_bucket):
    assert s3.get_bucket_versioning(Bucket=lock_bucket)["Status"] == "Enabled"
    cfg = s3.get_object_lock_configuration(Bucket=lock_bucket)["ObjectLockConfiguration"]
    assert cfg["ObjectLockEnabled"] == "Enabled"
    s3.put_object_lock_configuration(
        Bucket=lock_bucket,
        ObjectLockConfiguration={
            "ObjectLockEnabled": "Enabled",
            "Rule": {"DefaultRetention": {"Mode": "GOVERNANCE", "Days": 1}},
        },
    )
    cfg = s3.get_object_lock_configuration(Bucket=lock_bucket)["ObjectLockConfiguration"]
    assert cfg["Rule"]["DefaultRetention"] == {"Mode": "GOVERNANCE", "Days": 1}

    v = s3.put_object(Bucket=lock_bucket, Key="default", Body=b"x")["VersionId"]
    head = s3.head_object(Bucket=lock_bucket, Key="default")
    assert head["ObjectLockMode"] == "GOVERNANCE"
    assert head["ObjectLockRetainUntilDate"] > datetime.datetime.now(datetime.timezone.utc)
    with pytest.raises(ClientError) as ei:
        s3.delete_object(Bucket=lock_bucket, Key="default", VersionId=v)
    assert error_code(ei) == "AccessDenied"
    s3.delete_object(Bucket=lock_bucket, Key="default", VersionId=v, BypassGovernanceRetention=True)

    with pytest.raises(ClientError) as ei:
        s3.put_bucket_versioning(Bucket=lock_bucket, VersioningConfiguration={"Status": "Suspended"})
    assert error_code(ei) == "InvalidBucketState"


def test_retention_and_legal_hold(s3, lock_bucket):
    until = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=1)
    v = s3.put_object(
        Bucket=lock_bucket,
        Key="r",
        Body=b"x",
        ObjectLockMode="GOVERNANCE",
        ObjectLockRetainUntilDate=until,
    )["VersionId"]
    ret = s3.get_object_retention(Bucket=lock_bucket, Key="r")["Retention"]
    assert ret["Mode"] == "GOVERNANCE"
    assert abs((ret["RetainUntilDate"] - until).total_seconds()) < 1

    # Shortening governance retention needs the bypass header.
    with pytest.raises(ClientError) as ei:
        s3.put_object_retention(
            Bucket=lock_bucket,
            Key="r",
            Retention={"Mode": "GOVERNANCE", "RetainUntilDate": until - datetime.timedelta(minutes=30)},
        )
    assert error_code(ei) == "AccessDenied"
    s3.put_object_retention(
        Bucket=lock_bucket,
        Key="r",
        Retention={"Mode": "GOVERNANCE", "RetainUntilDate": until - datetime.timedelta(minutes=30)},
        BypassGovernanceRetention=True,
    )

    s3.put_object_legal_hold(Bucket=lock_bucket, Key="r", LegalHold={"Status": "ON"})
    assert s3.get_object_legal_hold(Bucket=lock_bucket, Key="r")["LegalHold"]["Status"] == "ON"
    assert s3.head_object(Bucket=lock_bucket, Key="r")["ObjectLockLegalHoldStatus"] == "ON"
    with pytest.raises(ClientError) as ei:
        s3.delete_object(Bucket=lock_bucket, Key="r", VersionId=v, BypassGovernanceRetention=True)
    assert error_code(ei) == "AccessDenied"
    s3.put_object_legal_hold(Bucket=lock_bucket, Key="r", LegalHold={"Status": "OFF"})
    s3.delete_object(Bucket=lock_bucket, Key="r", VersionId=v, BypassGovernanceRetention=True)


def test_object_lock_requires_lock_bucket(s3, bucket):
    with pytest.raises(ClientError) as ei:
        s3.put_object(
            Bucket=bucket,
            Key="x",
            Body=b"x",
            ObjectLockMode="GOVERNANCE",
            ObjectLockRetainUntilDate=datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(days=1),
        )
    assert error_code(ei) == "InvalidRequest"
    with pytest.raises(ClientError) as ei:
        s3.get_object_lock_configuration(Bucket=bucket)
    assert error_code(ei) == "ObjectLockConfigurationNotFoundError"
