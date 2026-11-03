"""Two gos3 cluster voters exercised through separate boto3 S3 clients."""

import os

import pytest
from botocore.exceptions import ClientError

from conftest import empty_and_delete, error_code, make_client, unique_name


@pytest.fixture(scope="session")
def second_s3(region, credentials):
    endpoint = os.environ.get("GOS3_COMPAT_SECOND_ENDPOINT")
    if not endpoint:
        pytest.skip("the cluster compatibility harness was not selected")
    return make_client(endpoint, region, credentials)


def test_cross_node_versions_conditions_multipart_and_delete(s3, second_s3):
    bucket = unique_name("cluster")
    s3.create_bucket(Bucket=bucket)
    try:
        second_s3.put_bucket_versioning(
            Bucket=bucket, VersioningConfiguration={"Status": "Enabled"}
        )
        assert s3.get_bucket_versioning(Bucket=bucket)["Status"] == "Enabled"

        first = s3.put_object(Bucket=bucket, Key="versioned", Body=b"first")
        assert first["VersionId"]
        assert second_s3.get_object(Bucket=bucket, Key="versioned")["Body"].read() == b"first"

        second = second_s3.put_object(
            Bucket=bucket, Key="versioned", Body=b"second", IfMatch=first["ETag"]
        )
        assert second["VersionId"] != first["VersionId"]
        with pytest.raises(ClientError) as exc:
            s3.put_object(Bucket=bucket, Key="versioned", Body=b"stale", IfMatch=first["ETag"])
        assert error_code(exc) == "PreconditionFailed"
        assert s3.get_object(Bucket=bucket, Key="versioned")["Body"].read() == b"second"

        upload = s3.create_multipart_upload(Bucket=bucket, Key="multipart")
        uid = upload["UploadId"]
        try:
            part = second_s3.upload_part(
                Bucket=bucket, Key="multipart", UploadId=uid, PartNumber=1, Body=b"part-body"
            )
            s3.complete_multipart_upload(
                Bucket=bucket,
                Key="multipart",
                UploadId=uid,
                MultipartUpload={"Parts": [{"PartNumber": 1, "ETag": part["ETag"]}]},
            )
        except Exception:
            s3.abort_multipart_upload(Bucket=bucket, Key="multipart", UploadId=uid)
            raise
        assert second_s3.get_object(Bucket=bucket, Key="multipart")["Body"].read() == b"part-body"

        marker = second_s3.delete_object(Bucket=bucket, Key="versioned")
        assert marker["DeleteMarker"] is True
        with pytest.raises(ClientError) as exc:
            s3.get_object(Bucket=bucket, Key="versioned")
        assert error_code(exc) == "NoSuchKey"
        assert second_s3.get_object(
            Bucket=bucket, Key="versioned", VersionId=first["VersionId"]
        )["Body"].read() == b"first"
    finally:
        empty_and_delete(s3, bucket)
