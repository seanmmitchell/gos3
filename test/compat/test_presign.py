"""Presigned URLs and presigned (browser) POST uploads, exercised with
plain HTTP clients (urllib, requests) that know nothing about SigV4."""

import time
import urllib.error
import urllib.parse
import urllib.request

import pytest
import requests

from conftest import make_client


def _open(method, url, data=None, headers=None):
    """Returns (status, headers (case-insensitive), body)."""
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


def test_presigned_get(s3, bucket):
    s3.put_object(Bucket=bucket, Key="dl/file name.txt", Body=b"presigned body")
    url = s3.generate_presigned_url("get_object", Params={"Bucket": bucket, "Key": "dl/file name.txt"}, ExpiresIn=300)
    assert "X-Amz-Signature=" in url
    code, headers, body = _open("GET", url)
    assert code == 200
    assert body == b"presigned body"

    # Ranged GET with an unsigned Range header still matches the signature.
    code, _, body = _open("GET", url, headers={"Range": "bytes=0-8"})
    assert code == 206
    assert body == b"presigned"

    # HEAD is a different method: a GET signature does not cover it.
    code, _, _ = _open("HEAD", url)
    assert code == 403

    head_url = s3.generate_presigned_url("head_object", Params={"Bucket": bucket, "Key": "dl/file name.txt"})
    code, headers, _ = _open("HEAD", head_url)
    assert code == 200
    assert headers["Content-Length"] == "14"


def test_presigned_get_response_overrides(s3, bucket):
    s3.put_object(Bucket=bucket, Key="o.bin", Body=b"override")
    url = s3.generate_presigned_url(
        "get_object",
        Params={
            "Bucket": bucket,
            "Key": "o.bin",
            "ResponseContentDisposition": 'attachment; filename="download.txt"',
            "ResponseContentType": "text/plain",
        },
    )
    code, headers, body = _open("GET", url)
    assert code == 200
    assert headers["Content-Disposition"] == 'attachment; filename="download.txt"'
    assert headers["Content-Type"] == "text/plain"


def test_presigned_tampered_and_expired(s3, bucket):
    s3.put_object(Bucket=bucket, Key="t", Body=b"x")
    url = s3.generate_presigned_url("get_object", Params={"Bucket": bucket, "Key": "t"})
    code, _, body = _open("GET", url.replace("/t?", "/u?"))
    assert code == 403
    assert b"SignatureDoesNotMatch" in body

    url = s3.generate_presigned_url("get_object", Params={"Bucket": bucket, "Key": "t"}, ExpiresIn=1)
    time.sleep(2.1)
    code, _, body = _open("GET", url)
    assert code == 403
    assert b"AccessDenied" in body
    assert b"expired" in body.lower()


def test_presigned_put(s3, bucket):
    url = s3.generate_presigned_url(
        "put_object",
        Params={"Bucket": bucket, "Key": "up/presigned.txt", "ContentType": "text/plain"},
        ExpiresIn=300,
    )
    code, headers, _ = _open("PUT", url, data=b"uploaded via presigned PUT", headers={"Content-Type": "text/plain"})
    assert code == 200, _
    assert headers["ETag"]
    got = s3.get_object(Bucket=bucket, Key="up/presigned.txt")
    assert got["Body"].read() == b"uploaded via presigned PUT"
    assert got["ContentType"] == "text/plain"

    # requests (chunked=False, with Content-Length) too.
    url = s3.generate_presigned_url("put_object", Params={"Bucket": bucket, "Key": "up/requests.bin"})
    r = requests.put(url, data=b"\x00\x01\x02" * 1000, timeout=30)
    assert r.status_code == 200, r.text
    assert s3.head_object(Bucket=bucket, Key="up/requests.bin")["ContentLength"] == 3000


def test_presigned_put_with_metadata_header_signed(s3, bucket):
    url = s3.generate_presigned_url(
        "put_object", Params={"Bucket": bucket, "Key": "m", "Metadata": {"owner": "alice"}}
    )
    # With SigV4, x-amz-meta-* are signed headers the uploader must send.
    assert "x-amz-meta-owner" in urllib.parse.unquote(url)
    code, _, body = _open("PUT", url, data=b"meta")
    assert code == 403
    assert b"SignatureDoesNotMatch" in body
    code, _, body = _open("PUT", url, data=b"meta", headers={"x-amz-meta-owner": "alice"})
    assert code == 200, body
    assert s3.head_object(Bucket=bucket, Key="m")["Metadata"] == {"owner": "alice"}


def test_presigned_multipart_upload_part(s3, bucket):
    uid = s3.create_multipart_upload(Bucket=bucket, Key="mp")["UploadId"]
    part = b"p" * (5 * 1024 * 1024)
    etags = []
    for n, body in ((1, part), (2, b"last")):
        url = s3.generate_presigned_url(
            "upload_part", Params={"Bucket": bucket, "Key": "mp", "UploadId": uid, "PartNumber": n}
        )
        r = requests.put(url, data=body, timeout=60)
        assert r.status_code == 200, r.text
        etags.append(r.headers["ETag"])
    s3.complete_multipart_upload(
        Bucket=bucket,
        Key="mp",
        UploadId=uid,
        MultipartUpload={"Parts": [{"PartNumber": i + 1, "ETag": e} for i, e in enumerate(etags)]},
    )
    assert s3.get_object(Bucket=bucket, Key="mp")["Body"].read() == part + b"last"


def test_presigned_list_and_delete(s3, bucket):
    s3.put_object(Bucket=bucket, Key="a/1", Body=b"1")
    url = s3.generate_presigned_url("list_objects_v2", Params={"Bucket": bucket, "Prefix": "a/"})
    code, _, body = _open("GET", url)
    assert code == 200
    assert b"<Key>a/1</Key>" in body
    url = s3.generate_presigned_url("delete_object", Params={"Bucket": bucket, "Key": "a/1"})
    code, _, _ = _open("DELETE", url)
    assert code == 204
    assert s3.list_objects_v2(Bucket=bucket)["KeyCount"] == 0


def test_presigned_with_other_region_scope(endpoint, credentials, bucket, s3):
    # gos3 accepts any credential-scope region unless strict_region is set.
    other = make_client(endpoint, "eu-west-1", credentials)
    s3.put_object(Bucket=bucket, Key="r", Body=b"region")
    url = other.generate_presigned_url("get_object", Params={"Bucket": bucket, "Key": "r"})
    code, _, body = _open("GET", url)
    assert code == 200
    assert body == b"region"


# --- presigned POST -----------------------------------------------------------


def test_presigned_post_requests(s3, bucket):
    post = s3.generate_presigned_post(
        Bucket=bucket,
        Key="uploads/${filename}",
        Fields={"acl": "private", "Content-Type": "image/png", "success_action_status": "201"},
        Conditions=[
            {"acl": "private"},
            ["starts-with", "$Content-Type", "image/"],
            {"success_action_status": "201"},
            ["content-length-range", 1, 1024 * 1024],
        ],
        ExpiresIn=600,
    )
    data = b"\x89PNG\r\n\x1a\n" + b"\x00" * 100
    r = requests.post(post["url"], data=post["fields"], files={"file": ("pic.png", data, "image/png")}, timeout=30)
    assert r.status_code == 201, r.text
    assert "<Key>uploads/pic.png</Key>" in r.text
    assert f"<Bucket>{bucket}</Bucket>" in r.text
    assert r.headers.get("ETag")
    got = s3.get_object(Bucket=bucket, Key="uploads/pic.png")
    assert got["Body"].read() == data
    assert got["ContentType"] == "image/png"


def test_presigned_post_default_204_and_redirect(s3, bucket):
    post = s3.generate_presigned_post(Bucket=bucket, Key="plain.txt")
    r = requests.post(post["url"], data=post["fields"], files={"file": ("x.txt", b"hello")}, timeout=30)
    assert r.status_code == 204, r.text
    assert s3.get_object(Bucket=bucket, Key="plain.txt")["Body"].read() == b"hello"

    post = s3.generate_presigned_post(
        Bucket=bucket,
        Key="redir.txt",
        Fields={"success_action_redirect": "https://example.com/done"},
        Conditions=[{"success_action_redirect": "https://example.com/done"}],
    )
    r = requests.post(
        post["url"], data=post["fields"], files={"file": ("x.txt", b"r")}, timeout=30, allow_redirects=False
    )
    assert r.status_code == 303, r.text
    loc = urllib.parse.urlparse(r.headers["Location"])
    assert loc.netloc == "example.com"
    q = urllib.parse.parse_qs(loc.query)
    assert q["bucket"] == [bucket]
    assert q["key"] == ["redir.txt"]
    assert q["etag"]


def test_presigned_post_policy_violations(s3, bucket):
    post = s3.generate_presigned_post(
        Bucket=bucket,
        Key="limited/obj",
        Conditions=[["content-length-range", 1, 10]],
    )
    r = requests.post(post["url"], data=post["fields"], files={"file": ("f", b"x" * 11)}, timeout=30)
    assert r.status_code == 400, r.text
    assert "EntityTooLarge" in r.text

    # Changing the key breaks the policy's key condition.
    fields = dict(post["fields"], key="limited/other")
    r = requests.post(post["url"], data=fields, files={"file": ("f", b"x")}, timeout=30)
    assert r.status_code == 403, r.text
    assert "AccessDenied" in r.text

    # Tampered signature.
    fields = dict(post["fields"], **{"x-amz-signature": "0" * 64})
    r = requests.post(post["url"], data=fields, files={"file": ("f", b"x")}, timeout=30)
    assert r.status_code == 403, r.text
    assert "SignatureDoesNotMatch" in r.text


def test_presigned_post_urllib_multipart(s3, bucket):
    """Hand-built multipart/form-data body sent with urllib."""
    post = s3.generate_presigned_post(
        Bucket=bucket,
        Key="manual/${filename}",
        Fields={"x-amz-meta-uploader": "urllib"},
        Conditions=[{"x-amz-meta-uploader": "urllib"}, ["starts-with", "$key", "manual/"]],
    )
    boundary = "----gos3compatboundary7MA4YWxkTrZu0gW"
    parts = []
    for k, v in post["fields"].items():
        parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode())
    parts.append(
        f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="hello.txt"\r\n'
        "Content-Type: text/plain\r\n\r\n".encode()
        + b"hello from urllib\r\n"
    )
    parts.append(f"--{boundary}--\r\n".encode())
    body = b"".join(parts)
    code, headers, resp = _open(
        "POST", post["url"], data=body, headers={"Content-Type": f"multipart/form-data; boundary={boundary}"}
    )
    assert code == 204, resp
    got = s3.get_object(Bucket=bucket, Key="manual/hello.txt")
    assert got["Body"].read() == b"hello from urllib"
    assert got["Metadata"] == {"uploader": "urllib"}


@pytest.mark.parametrize("expires", [3600, 7 * 24 * 3600])
def test_presigned_expiry_limits(s3, bucket, expires):
    s3.put_object(Bucket=bucket, Key="e", Body=b"e")
    url = s3.generate_presigned_url("get_object", Params={"Bucket": bucket, "Key": "e"}, ExpiresIn=expires)
    assert _open("GET", url)[0] == 200


def test_presigned_expiry_too_long(s3, bucket):
    s3.put_object(Bucket=bucket, Key="e", Body=b"e")
    url = s3.generate_presigned_url("get_object", Params={"Bucket": bucket, "Key": "e"}, ExpiresIn=7 * 24 * 3600 + 1)
    code, _, body = _open("GET", url)
    assert code in (400, 403)
    assert b"AuthorizationQueryParametersError" in body or b"AccessDenied" in body
