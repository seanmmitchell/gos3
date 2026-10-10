package s3

// HANDLER GUIDE (for the s3-bucket, s3-object, s3-multipart and
// s3-bucketconfig work items)
//
// # Pipeline
//
// Server.ServeHTTP (middleware.go) runs for every request:
//
//  1. sets x-amz-request-id (16 uppercase hex), x-amz-id-2, Server and Date;
//     recovers panics (InternalError); records metrics (op name, status,
//     bytes in/out) and writes the access log;
//  2. parses bucket/key (router.go: path-style or virtual-hosted-style via
//     Settings.Domains; keys are the decoded path, verbatim) and detects the
//     operation (detectOp, the table in docs/ARCHITECTURE.md);
//  3. answers OPTIONS preflights and adds CORS headers (cors.go);
//  4. validates the key (<= 1024 bytes, UTF-8), authenticates with
//     auth.Verifier (PostObject skips this: the handler sees an anonymous
//     auth.Result over the raw body) and strips "aws-chunked" from
//     Content-Encoding;
//  5. authorizes the primary resource with the op's IAM action (authz.go;
//     not for DeleteObjects/PostObject, which authorize themselves);
//  6. calls the handler from opTable (ops.go).
//
// # Handler signature
//
//	func (s *Server) handleX(w http.ResponseWriter, r *http.Request, rc *reqContext) error
//
// Write a complete success response and return nil, or return an error
// WITHOUT having written anything: the core renders it (writeError) with
// the right status, XML body (none for HEAD / 304) and identifying
// elements (BucketName, Key, VersionId, UploadId are added automatically
// for NoSuchBucket/NoSuchKey/NoSuchVersion/NoSuchUpload). Return
// *s3err.Error values or storage errors (mapped by mapStorageError); other
// errors become InternalError and are logged. Headers set before returning
// an error survive (x-amz-delete-marker, x-amz-version-id; ETag and
// Last-Modified only for 304) except Content-* ones.
//
// # reqContext (context.go)
//
//	rc.op / rc.opName      operation spec / name
//	rc.bucket, rc.key      addressed resource (decoded, verbatim)
//	rc.query               parsed query (rc.versionID() = versionId)
//	rc.identity            auth.Identity (rc.userName(): "" anonymous, "root", user)
//	rc.auth                *auth.Result (Method, PayloadMode, ...)
//	rc.body                request payload (== r.Body); verifying reader
//	rc.contentLength()     decoded payload length or -1
//	rc.trailer()           aws-chunked trailers (after EOF), never nil
//	rc.requestID, rc.start request ID and arrival time
//	rc.addLog(attrs...)    extra access log attributes
//	s.bucketInfo(ctx, rc)  cached GetBucket (NoSuchBucket with BucketName)
//	s.versioning(ctx, rc), s.isVersioned(ctx, rc)
//	rc.forgetBucket()      drop the cache after changing bucket state
//	s.lookupBucket(ctx, b) uncached lookup of any bucket
//
// # Responses (response.go, xml.go)
//
//	writeXML(w, status, v)       XML declaration + v (set the S3 namespace:
//	                             XMLName tag "http://s3.amazonaws.com/doc/2006-03-01/ Name"
//	                             or an XMLNS `xml:"xmlns,attr"` field = s3Namespace)
//	writeNoContent(w), writeEmpty(w, status), writeBody(w, status, ct, body)
//	s.writeError(w, r, rc, err)  only if you must render an error yourself
//	mapStorageError(err)         storage sentinel -> S3 error (ErrConfigNotFound
//	                             is NOT mapped: use the config-specific code)
//	bucketError(err, bucket)     same, adding BucketName to NoSuchBucket
//	readBody(r, max), decodeXMLBody(r, max, &v), decodeXML(data, &v)
//	                             bounded reads; MalformedXML / MissingRequestBodyError
//	formatXMLTime(t), formatHTTPTime(t), parseHTTPTime(s), parseISOTime(s)
//	parseEncodingType(q), s3URLEncode(s), maybeURLEncode(s, enc)
//	Owner, ownerFor(user)        ID = hex sha256(user), DisplayName = user ("" -> root)
//	Tagging/TagSet/Tag, taggingFromMap, validateObjectTags (<=10),
//	validateBucketTags (<=50), validateTag, parseTaggingHeader
//	PublicAccessBlockConfiguration, parsePublicAccessBlock, s.publicAccessBlock(ctx, b)
//	AccessControlPolicy/Grant/Grantee (xsi:type handled), aclPolicyForCanned,
//	cannedFromACLPolicy, cannedACLFromConfig (storage.ConfigACL bytes)
//	CORSConfiguration/CORSRule, parseCORSConfiguration (validates)
//
// # Validation (validate.go)
//
//	validBucketName / validateBucketName, validateObjectKey,
//	normalizeStorageClass, storageClassOrStandard, validateCannedACL(v, bucket),
//	parseMaxKeys(q, name, def, limit), parsePartNumber, and the limits
//	maxKeyLength, maxUserMetaSize, maxListKeys, maxDeleteObjects,
//	min/maxPartNumber, minPartSize.
//
// # Object headers (headers.go)
//
//	s.objectMetaFromHeaders(r.Header) -> storage.ObjectMeta (validated; see doc)
//	requireObjectLock(meta, bucketInfo)   InvalidRequest if lock wanted but not enabled
//	checkSSECHeaders(h)                   NotImplemented for SSE-C (also copy-source)
//	writeObjectHeaders(w, info, versioned, checksumModeEnabled(r.Header))
//
// # Conditions, ranges, copy sources (conditional.go)
//
//	checkConditions(r.Header, etag, lastModified)  nil / PreconditionFailed / NotModified
//	checkCopySourceConditions(r.Header, etag, lm)  x-amz-copy-source-if-*, 412 only
//	parseRange(r.Header.Get("Range"), size)        *byteRange or nil (full object)
//	byteRange.ContentRange(size)
//	parseCopySource(h.Get("X-Amz-Copy-Source"))    {Bucket, Key, VersionID}
//	parseCopySourceRange(v, size)
//	trimETag, quoteETag, etagListMatches
//
// # Payloads and checksums (checksum.go)
//
//	p, err := s.newPayloadReader(r, rc, payloadOptions{RequireLength: true})
//	io.Copy(storageWriter, p)  -> on ANY error: writer.Abort() and return the error
//	p.ETag(), p.Size(), p.Checksums(), p.ChecksumType(), p.Algorithm()
//
// The payload reader always computes MD5 plus the requested checksum,
// enforces MaxObjectSize (or payloadOptions.MaxSize) and the declared
// length, and validates Content-MD5, x-amz-checksum-* headers and trailer
// checksums at EOF. Multipart helpers: multipartETag(partETags),
// compositeChecksum(algo, partChecksums) ("<b64>-N"),
// combineFullObjectChecksum(algo, []checksumPart{...}), parseChecksumType,
// validateChecksumType(algo, typ), defaultChecksumType, supportsFullObject,
// normalizeChecksumAlgorithm, checksumHeader(algo), computeChecksum.
//
// # Authorization (authz.go)
//
// The primary resource is already authorized when the handler runs. For a
// second resource call
//
//	s.authorize(r, rc, action, bucket, key, versionID)
//
// e.g. s.authorize(r, rc, objectReadAction(src.VersionID), src.Bucket,
// src.Key, src.VersionID) for CopyObject/UploadPartCopy sources, and
// s.authorize(r, rc, "s3:DeleteObject"/"s3:DeleteObjectVersion", bucket,
// key, vid) for each DeleteObjects entry. For extra condition keys use
// s.authorizeTarget(ctx, r, rc, authzTarget{...}); when the attributes of
// the object written come from a body, form or multipart upload rather
// than from request headers, set authzTarget.ObjectAttrs (see
// objectAttrConditions) so header-derived keys are replaced, not merged.
// After changing a policy,
// CORS, ACL or public access block document (or deleting a bucket) call
// s.invalidateBucketConfig(bucket).
//
// # Tests (testutil_test.go)
//
//	e := newTestEnv(t, opts...)             httptest server running New() over memory.New(),
//	                                        a real auth.Verifier backed by an in-memory iam.Manager
//	                                        (root = testRoot), metrics registry, debug JSON logs
//	e.client                                aws-sdk-go-v2 *s3.Client (root, path-style, us-east-1,
//	                                        BaseEndpoint = server URL, retries off)
//	e.clientFor(creds)                      SDK client for other credentials
//	e.signed(method, path, body, hdr)       raw SigV4 request as root -> *testResponse
//	e.signedAs(creds, ...), e.anon(...)     signed as another user / unauthenticated
//	e.newRequest(...), e.do(req), e.url(p)  build / send arbitrary requests (hdr "Host"
//	                                        sets the Host, e.g. for virtual-host tests)
//	signRoot(req), authCreds(ak, sk)        signing helpers
//	e.createBucket(name), e.putObject(bucket, key, data, meta), e.putBucketConfig(b, key, doc)
//	                                        direct backend setup (no handler needed)
//	e.createUser(name, policies...)         IAM user + access key -> auth.Credentials
//	e.putPolicy(name, json)                 custom identity policy
//	e.override(op, handlerFunc)             replace a handler in this env only; okHandler
//	                                        answers 200 "<op> <bucket> <key>"
//	requireAuthz(t, e)                      skip when the IAM engine is still a stub
//	e.backend, e.iam, e.settings, e.metrics, e.logs.String(), e.server
//	resp.StatusCode, resp.Header, resp.Data, resp.code() (S3 error code)
//
// Options: withTLS() (the SDK then sends aws-chunked trailer checksums),
// withSettings(func(*config.Settings)), withOptions(func(*Options)).
