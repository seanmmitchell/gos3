"""Tagging, bucket policy + anonymous access, CORS and lifecycle via boto3."""

import json
import urllib.error
import urllib.request

import pytest
from botocore.exceptions import ClientError

from conftest import error_code, make_client, status_code

# --- tagging ------------------------------------------------------------------


def test_object_tagging(s3, bucket):
    s3.put_object(Bucket=bucket, Key="t", Body=b"x", Tagging="a=1&b=two%20words")
    tags = s3.get_object_tagging(Bucket=bucket, Key="t")["TagSet"]
    assert sorted((t["Key"], t["Value"]) for t in tags) == [("a", "1"), ("b", "two words")]
    assert s3.get_object(Bucket=bucket, Key="t")["TagCount"] == 2

    s3.put_object_tagging(Bucket=bucket, Key="t", Tagging={"TagSet": [{"Key": "env", "Value": "prod"}]})
    assert s3.get_object_tagging(Bucket=bucket, Key="t")["TagSet"] == [{"Key": "env", "Value": "prod"}]

    s3.delete_object_tagging(Bucket=bucket, Key="t")
    assert s3.get_object_tagging(Bucket=bucket, Key="t")["TagSet"] == []

    with pytest.raises(ClientError) as ei:
        s3.put_object_tagging(
            Bucket=bucket,
            Key="t",
            Tagging={"TagSet": [{"Key": f"k{i}", "Value": "v"} for i in range(11)]},
        )
    assert error_code(ei) in ("BadRequest", "InvalidTag")

    with pytest.raises(ClientError) as ei:
        s3.get_object_tagging(Bucket=bucket, Key="missing")
    assert error_code(ei) == "NoSuchKey"


def test_tagging_copy_directive(s3, bucket):
    s3.put_object(Bucket=bucket, Key="src", Body=b"x", Tagging="a=1")
    s3.copy_object(Bucket=bucket, Key="copy", CopySource=f"{bucket}/src")
    assert s3.get_object_tagging(Bucket=bucket, Key="copy")["TagSet"] == [{"Key": "a", "Value": "1"}]
    s3.copy_object(Bucket=bucket, Key="replace", CopySource=f"{bucket}/src", TaggingDirective="REPLACE", Tagging="b=2")
    assert s3.get_object_tagging(Bucket=bucket, Key="replace")["TagSet"] == [{"Key": "b", "Value": "2"}]


def test_versioned_object_tagging(s3, versioned_bucket):
    bucket = versioned_bucket
    v1 = s3.put_object(Bucket=bucket, Key="t", Body=b"1", Tagging="v=1")["VersionId"]
    s3.put_object(Bucket=bucket, Key="t", Body=b"2", Tagging="v=2")
    r = s3.get_object_tagging(Bucket=bucket, Key="t", VersionId=v1)
    assert r["TagSet"] == [{"Key": "v", "Value": "1"}]
    assert r["VersionId"] == v1
    s3.put_object_tagging(Bucket=bucket, Key="t", VersionId=v1, Tagging={"TagSet": [{"Key": "v", "Value": "one"}]})
    assert s3.get_object_tagging(Bucket=bucket, Key="t", VersionId=v1)["TagSet"] == [{"Key": "v", "Value": "one"}]
    assert s3.get_object_tagging(Bucket=bucket, Key="t")["TagSet"] == [{"Key": "v", "Value": "2"}]


def test_bucket_tagging(s3, bucket):
    with pytest.raises(ClientError) as ei:
        s3.get_bucket_tagging(Bucket=bucket)
    assert error_code(ei) == "NoSuchTagSet"
    s3.put_bucket_tagging(Bucket=bucket, Tagging={"TagSet": [{"Key": "team", "Value": "storage"}]})
    assert s3.get_bucket_tagging(Bucket=bucket)["TagSet"] == [{"Key": "team", "Value": "storage"}]
    s3.delete_bucket_tagging(Bucket=bucket)
    with pytest.raises(ClientError) as ei:
        s3.get_bucket_tagging(Bucket=bucket)
    assert error_code(ei) == "NoSuchTagSet"


# --- bucket policy + anonymous access -----------------------------------------


def _public_read_policy(bucket, prefix="public/"):
    return json.dumps(
        {
            "Version": "2012-10-17",
            "Statement": [
                {
                    "Sid": "PublicRead",
                    "Effect": "Allow",
                    "Principal": "*",
                    "Action": ["s3:GetObject"],
                    "Resource": [f"arn:aws:s3:::{bucket}/{prefix}*"],
                },
                {
                    "Sid": "PublicList",
                    "Effect": "Allow",
                    "Principal": {"AWS": ["*"]},
                    "Action": "s3:ListBucket",
                    "Resource": f"arn:aws:s3:::{bucket}",
                    "Condition": {"StringLike": {"s3:prefix": [f"{prefix}*"]}},
                },
            ],
        }
    )


def test_bucket_policy_anonymous_access(s3, s3_anon, endpoint, bucket):
    s3.put_object(Bucket=bucket, Key="public/hello.txt", Body=b"hi there")
    s3.put_object(Bucket=bucket, Key="private/secret.txt", Body=b"secret")

    with pytest.raises(ClientError) as ei:
        s3_anon.get_object(Bucket=bucket, Key="public/hello.txt")
    assert error_code(ei) == "AccessDenied"

    with pytest.raises(ClientError) as ei:
        s3.get_bucket_policy(Bucket=bucket)
    assert error_code(ei) == "NoSuchBucketPolicy"

    policy = _public_read_policy(bucket)
    s3.put_bucket_policy(Bucket=bucket, Policy=policy)
    got = json.loads(s3.get_bucket_policy(Bucket=bucket)["Policy"])
    assert got["Statement"][0]["Sid"] == "PublicRead"
    assert s3.get_bucket_policy_status(Bucket=bucket)["PolicyStatus"]["IsPublic"] is True

    assert s3_anon.get_object(Bucket=bucket, Key="public/hello.txt")["Body"].read() == b"hi there"
    assert s3_anon.head_object(Bucket=bucket, Key="public/hello.txt")["ContentLength"] == 8
    with pytest.raises(ClientError) as ei:
        s3_anon.get_object(Bucket=bucket, Key="private/secret.txt")
    assert error_code(ei) == "AccessDenied"
    with pytest.raises(ClientError) as ei:
        s3_anon.put_object(Bucket=bucket, Key="public/new", Body=b"x")
    assert error_code(ei) == "AccessDenied"

    keys = [o["Key"] for o in s3_anon.list_objects_v2(Bucket=bucket, Prefix="public/")["Contents"]]
    assert keys == ["public/hello.txt"]
    with pytest.raises(ClientError) as ei:
        s3_anon.list_objects_v2(Bucket=bucket)
    assert error_code(ei) == "AccessDenied"

    # Plain unsigned HTTP (e.g. a browser) works too.
    with urllib.request.urlopen(f"{endpoint}/{bucket}/public/hello.txt") as r:
        assert r.read() == b"hi there"
    with pytest.raises(urllib.error.HTTPError) as hi:
        urllib.request.urlopen(f"{endpoint}/{bucket}/private/secret.txt")
    assert hi.value.code == 403

    s3.delete_bucket_policy(Bucket=bucket)
    with pytest.raises(ClientError) as ei:
        s3_anon.get_object(Bucket=bucket, Key="public/hello.txt")
    assert error_code(ei) == "AccessDenied"


def test_bucket_policy_deny_wins(s3, s3_anon, bucket):
    # gos3's root identity bypasses policies by design (AWS would apply an
    # explicit bucket-policy Deny even to the account root), so this checks
    # Deny-over-Allow with anonymous requests.
    policy = {
        "Version": "2012-10-17",
        "Statement": [
            {
                "Effect": "Allow",
                "Principal": "*",
                "Action": "s3:GetObject",
                "Resource": f"arn:aws:s3:::{bucket}/*",
            },
            {
                "Effect": "Deny",
                "Principal": "*",
                "Action": "s3:GetObject",
                "Resource": f"arn:aws:s3:::{bucket}/locked/*",
            },
        ],
    }
    s3.put_bucket_policy(Bucket=bucket, Policy=json.dumps(policy))
    s3.put_object(Bucket=bucket, Key="locked/a", Body=b"x")
    s3.put_object(Bucket=bucket, Key="free/a", Body=b"x")
    assert s3_anon.get_object(Bucket=bucket, Key="free/a")["Body"].read() == b"x"
    with pytest.raises(ClientError) as ei:
        s3_anon.get_object(Bucket=bucket, Key="locked/a")
    assert error_code(ei) == "AccessDenied"


def test_malformed_policy(s3, bucket):
    with pytest.raises(ClientError) as ei:
        s3.put_bucket_policy(Bucket=bucket, Policy="{not json")
    assert error_code(ei) == "MalformedPolicy"
    with pytest.raises(ClientError) as ei:
        s3.put_bucket_policy(
            Bucket=bucket,
            Policy=json.dumps(
                {
                    "Version": "2012-10-17",
                    "Statement": [
                        {
                            "Effect": "Allow",
                            "Principal": "*",
                            "Action": "s3:GetObject",
                            "Resource": "arn:aws:s3:::some-other-bucket/*",
                        }
                    ],
                }
            ),
        )
    assert error_code(ei) == "MalformedPolicy"


def test_public_read_acl(s3, s3_anon, bucket):
    s3.put_object(Bucket=bucket, Key="acl/obj", Body=b"acl", ACL="public-read")
    assert s3_anon.get_object(Bucket=bucket, Key="acl/obj")["Body"].read() == b"acl"
    acl = s3.get_object_acl(Bucket=bucket, Key="acl/obj")
    assert any(g["Permission"] == "READ" and g["Grantee"].get("URI", "").endswith("AllUsers") for g in acl["Grants"])
    s3.put_object_acl(Bucket=bucket, Key="acl/obj", ACL="private")
    with pytest.raises(ClientError) as ei:
        s3_anon.get_object(Bucket=bucket, Key="acl/obj")
    assert error_code(ei) == "AccessDenied"


def test_anonymous_list_buckets_denied(s3_anon):
    with pytest.raises(ClientError) as ei:
        s3_anon.list_buckets()
    assert error_code(ei) == "AccessDenied"


def test_bad_credentials(endpoint, region, bucket):
    bad = make_client(endpoint, region, ("compatroot", "wrong-secret-key"))
    with pytest.raises(ClientError) as ei:
        bad.list_objects_v2(Bucket=bucket)
    assert error_code(ei) == "SignatureDoesNotMatch"
    unknown = make_client(endpoint, region, ("no-such-access-key", "whatever-secret"))
    with pytest.raises(ClientError) as ei:
        unknown.list_objects_v2(Bucket=bucket)
    assert error_code(ei) == "InvalidAccessKeyId"


# --- CORS ---------------------------------------------------------------------

CORS_CONFIG = {
    "CORSRules": [
        {
            "ID": "app",
            "AllowedOrigins": ["https://app.example.com"],
            "AllowedMethods": ["GET", "PUT", "HEAD"],
            "AllowedHeaders": ["*"],
            "ExposeHeaders": ["ETag", "x-amz-meta-color"],
            "MaxAgeSeconds": 600,
        },
        {"AllowedOrigins": ["*"], "AllowedMethods": ["GET"]},
    ]
}


def _request(method, url, headers):
    req = urllib.request.Request(url, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, {k.lower(): v for k, v in r.headers.items()}
    except urllib.error.HTTPError as e:
        return e.code, {k.lower(): v for k, v in e.headers.items()}


def test_cors_round_trip_and_preflight(s3, endpoint, region, credentials, bucket):
    with pytest.raises(ClientError) as ei:
        s3.get_bucket_cors(Bucket=bucket)
    assert error_code(ei) == "NoSuchCORSConfiguration"

    s3.put_bucket_cors(Bucket=bucket, CORSConfiguration=CORS_CONFIG)
    rules = s3.get_bucket_cors(Bucket=bucket)["CORSRules"]
    assert len(rules) == 2
    assert rules[0]["ID"] == "app"
    assert rules[0]["AllowedOrigins"] == ["https://app.example.com"]
    assert sorted(rules[0]["AllowedMethods"]) == ["GET", "HEAD", "PUT"]
    assert rules[0]["ExposeHeaders"] == ["ETag", "x-amz-meta-color"]
    assert rules[0]["MaxAgeSeconds"] == 600

    url = f"{endpoint}/{bucket}/some/key"
    code, h = _request(
        "OPTIONS",
        url,
        {
            "Origin": "https://app.example.com",
            "Access-Control-Request-Method": "PUT",
            "Access-Control-Request-Headers": "content-type,x-amz-meta-color",
        },
    )
    assert code == 200
    assert h["access-control-allow-origin"] == "https://app.example.com"
    assert "PUT" in h["access-control-allow-methods"]
    assert "x-amz-meta-color" in h["access-control-allow-headers"].lower()
    assert h["access-control-max-age"] == "600"

    code, h = _request(
        "OPTIONS", url, {"Origin": "https://evil.example.com", "Access-Control-Request-Method": "PUT"}
    )
    assert code == 403

    code, h = _request(
        "OPTIONS", url, {"Origin": "https://other.example.com", "Access-Control-Request-Method": "GET"}
    )
    assert code == 200
    assert h["access-control-allow-origin"] == "*"

    # Actual (signed) request: CORS headers on the response.
    s3.put_object(Bucket=bucket, Key="cors.txt", Body=b"x", Metadata={"color": "red"})
    c = make_client(endpoint, region, credentials)
    c.meta.events.register(
        "before-sign.s3.GetObject",
        lambda request, **kw: request.headers.__setitem__("Origin", "https://app.example.com"),
    )
    resp = c.get_object(Bucket=bucket, Key="cors.txt")
    hdrs = resp["ResponseMetadata"]["HTTPHeaders"]
    assert hdrs.get("access-control-allow-origin") == "https://app.example.com"
    assert "x-amz-meta-color" in hdrs.get("access-control-expose-headers", "").lower()

    s3.delete_bucket_cors(Bucket=bucket)
    with pytest.raises(ClientError) as ei:
        s3.get_bucket_cors(Bucket=bucket)
    assert error_code(ei) == "NoSuchCORSConfiguration"


def test_cors_invalid(s3, bucket):
    with pytest.raises(ClientError) as ei:
        s3.put_bucket_cors(
            Bucket=bucket,
            CORSConfiguration={"CORSRules": [{"AllowedOrigins": ["*"], "AllowedMethods": ["PATCH"]}]},
        )
    assert error_code(ei) in ("InvalidRequest", "MalformedXML")


# --- lifecycle ----------------------------------------------------------------


def test_lifecycle_round_trip(s3, bucket):
    with pytest.raises(ClientError) as ei:
        s3.get_bucket_lifecycle_configuration(Bucket=bucket)
    assert error_code(ei) == "NoSuchLifecycleConfiguration"

    config = {
        "Rules": [
            {
                "ID": "expire-logs",
                "Filter": {"Prefix": "logs/"},
                "Status": "Enabled",
                "Expiration": {"Days": 30},
            },
            {
                "ID": "tagged",
                "Filter": {"And": {"Prefix": "tmp/", "Tags": [{"Key": "temp", "Value": "yes"}]}},
                "Status": "Disabled",
                "Expiration": {"Days": 1},
            },
            {
                "ID": "noncurrent",
                "Filter": {},
                "Status": "Enabled",
                "NoncurrentVersionExpiration": {"NoncurrentDays": 7},
                "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 3},
            },
            {
                "ID": "markers",
                "Filter": {"Prefix": "old/"},
                "Status": "Enabled",
                "Expiration": {"ExpiredObjectDeleteMarker": True},
            },
        ]
    }
    s3.put_bucket_lifecycle_configuration(Bucket=bucket, LifecycleConfiguration=config)
    rules = {r["ID"]: r for r in s3.get_bucket_lifecycle_configuration(Bucket=bucket)["Rules"]}
    assert set(rules) == {"expire-logs", "tagged", "noncurrent", "markers"}
    assert rules["expire-logs"]["Filter"] == {"Prefix": "logs/"}
    assert rules["expire-logs"]["Expiration"] == {"Days": 30}
    assert rules["expire-logs"]["Status"] == "Enabled"
    assert rules["tagged"]["Status"] == "Disabled"
    assert rules["tagged"]["Filter"]["And"]["Tags"] == [{"Key": "temp", "Value": "yes"}]
    assert rules["noncurrent"]["NoncurrentVersionExpiration"]["NoncurrentDays"] == 7
    assert rules["noncurrent"]["AbortIncompleteMultipartUpload"] == {"DaysAfterInitiation": 3}
    assert rules["markers"]["Expiration"] == {"ExpiredObjectDeleteMarker": True}

    # Objects matching an expiration rule report x-amz-expiration.
    s3.put_object(Bucket=bucket, Key="logs/a.log", Body=b"x")
    head = s3.head_object(Bucket=bucket, Key="logs/a.log")
    assert 'rule-id="expire-logs"' in head.get("Expiration", "")

    s3.delete_bucket_lifecycle(Bucket=bucket)
    with pytest.raises(ClientError) as ei:
        s3.get_bucket_lifecycle_configuration(Bucket=bucket)
    assert error_code(ei) == "NoSuchLifecycleConfiguration"


def test_lifecycle_legacy_prefix_and_date(s3, bucket):
    config = {
        "Rules": [
            {
                "ID": "dated",
                "Prefix": "legacy/",
                "Status": "Enabled",
                "Expiration": {"Date": "2030-01-01T00:00:00Z"},
            }
        ]
    }
    s3.put_bucket_lifecycle_configuration(Bucket=bucket, LifecycleConfiguration=config)
    rule = s3.get_bucket_lifecycle_configuration(Bucket=bucket)["Rules"][0]
    assert rule["ID"] == "dated"
    assert rule.get("Prefix", rule.get("Filter", {}).get("Prefix")) == "legacy/"
    assert rule["Expiration"]["Date"].year == 2030


def test_lifecycle_invalid(s3, bucket):
    with pytest.raises(ClientError) as ei:
        s3.put_bucket_lifecycle_configuration(
            Bucket=bucket,
            LifecycleConfiguration={
                "Rules": [{"ID": "x", "Filter": {"Prefix": ""}, "Status": "Enabled", "Expiration": {"Days": 0}}]
            },
        )
    assert error_code(ei) in ("InvalidArgument", "MalformedXML")
    assert status_code(ei) == 400
