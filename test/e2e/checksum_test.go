package e2e

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// crc64NVMETable is CRC-64/NVME (reflected polynomial 0x9a6c9329ac4bc9b5).
var crc64NVMETable = crc64.MakeTable(0x9a6c9329ac4bc9b5)

var algorithms = []s3types.ChecksumAlgorithm{
	s3types.ChecksumAlgorithmCrc32,
	s3types.ChecksumAlgorithmCrc32c,
	s3types.ChecksumAlgorithmCrc64nvme,
	s3types.ChecksumAlgorithmSha1,
	s3types.ChecksumAlgorithmSha256,
}

func newHash(algo s3types.ChecksumAlgorithm) hash.Hash {
	switch algo {
	case s3types.ChecksumAlgorithmCrc32:
		return crc32.NewIEEE()
	case s3types.ChecksumAlgorithmCrc32c:
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case s3types.ChecksumAlgorithmCrc64nvme:
		return crc64.New(crc64NVMETable)
	case s3types.ChecksumAlgorithmSha1:
		return sha1.New()
	case s3types.ChecksumAlgorithmSha256:
		return sha256.New()
	}
	panic("unknown algorithm " + string(algo))
}

func rawChecksum(algo s3types.ChecksumAlgorithm, data []byte) []byte {
	h := newHash(algo)
	h.Write(data)
	return h.Sum(nil)
}

func checksumB64(algo s3types.ChecksumAlgorithm, data []byte) string {
	return base64.StdEncoding.EncodeToString(rawChecksum(algo, data))
}

// compositeB64 is the multipart COMPOSITE checksum of parts.
func compositeB64(algo s3types.ChecksumAlgorithm, parts [][]byte) string {
	var cat []byte
	for _, p := range parts {
		cat = append(cat, rawChecksum(algo, p)...)
	}
	return fmt.Sprintf("%s-%d", checksumB64(algo, cat), len(parts))
}

// checksumOf picks the checksum of algo from the SDK output fields.
func checksumOf(algo s3types.ChecksumAlgorithm, crc32v, crc32c, crc64v, sha1v, sha256v *string) string {
	switch algo {
	case s3types.ChecksumAlgorithmCrc32:
		return aws.ToString(crc32v)
	case s3types.ChecksumAlgorithmCrc32c:
		return aws.ToString(crc32c)
	case s3types.ChecksumAlgorithmCrc64nvme:
		return aws.ToString(crc64v)
	case s3types.ChecksumAlgorithmSha1:
		return aws.ToString(sha1v)
	case s3types.ChecksumAlgorithmSha256:
		return aws.ToString(sha256v)
	}
	return ""
}

func checksumHeaderName(algo s3types.ChecksumAlgorithm) string {
	return "x-amz-checksum-" + strings.ToLower(string(algo))
}

func TestCRC64NVMECheckValue(t *testing.T) {
	if got := binary.BigEndian.Uint64(rawChecksum(s3types.ChecksumAlgorithmCrc64nvme, []byte("123456789"))); got != 0xae8b14860a799888 {
		t.Fatalf("CRC64NVME check value = %#x", got)
	}
}

// TestChecksumAlgorithms uploads with every algorithm through the SDK. Over
// HTTP (fs server) the SDK sends the checksum as a header with a signed
// payload hash; over HTTPS (memory server) it uses its default
// aws-chunked STREAMING-UNSIGNED-PAYLOAD-TRAILER encoding with the
// checksum in a trailer.
func TestChecksumAlgorithms(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			hc, rec := e.recordingClient()
			c := e.root(func(o *s3.Options) { o.HTTPClient = hc })
			b := e.newBucket(t)
			data := randomData(200_001, 3)
			for _, algo := range algorithms {
				key := "obj-" + string(algo)
				want := checksumB64(algo, data)
				out, err := c.PutObject(ctx(t), &s3.PutObjectInput{
					Bucket: aws.String(b), Key: aws.String(key), Body: bytes.NewReader(data), ChecksumAlgorithm: algo,
				})
				must(t, err, "PutObject "+string(algo))
				sent := rec.last(http.MethodPut)
				mode := sent.header.Get("X-Amz-Content-Sha256")
				if e.tls {
					if mode != "STREAMING-UNSIGNED-PAYLOAD-TRAILER" || !strings.EqualFold(sent.header.Get("X-Amz-Trailer"), checksumHeaderName(algo)) {
						t.Errorf("%s over TLS: payload mode %q trailer %q", algo, mode, sent.header.Get("X-Amz-Trailer"))
					}
				} else if len(mode) != 64 || sent.header.Get(checksumHeaderName(algo)) != want {
					t.Errorf("%s over HTTP: payload mode %q header %q", algo, mode, sent.header.Get(checksumHeaderName(algo)))
				}
				if got := checksumOf(algo, out.ChecksumCRC32, out.ChecksumCRC32C, out.ChecksumCRC64NVME, out.ChecksumSHA1, out.ChecksumSHA256); got != want {
					t.Errorf("%s: PutObject checksum %q, want %q", algo, got, want)
				}
				if out.ChecksumType != s3types.ChecksumTypeFullObject {
					t.Errorf("%s: PutObject checksum type %q", algo, out.ChecksumType)
				}
				if aws.ToString(out.ETag) != quoted(md5Hex(data)) {
					t.Errorf("%s: ETag %s", algo, aws.ToString(out.ETag))
				}

				head, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String(key), ChecksumMode: s3types.ChecksumModeEnabled})
				must(t, err, "HeadObject")
				if got := checksumOf(algo, head.ChecksumCRC32, head.ChecksumCRC32C, head.ChecksumCRC64NVME, head.ChecksumSHA1, head.ChecksumSHA256); got != want {
					t.Errorf("%s: HEAD checksum %q, want %q", algo, got, want)
				}
				if head.ChecksumType != s3types.ChecksumTypeFullObject {
					t.Errorf("%s: HEAD checksum type %q", algo, head.ChecksumType)
				}
				plain, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String(key)})
				must(t, err, "HeadObject without checksum mode")
				if checksumOf(algo, plain.ChecksumCRC32, plain.ChecksumCRC32C, plain.ChecksumCRC64NVME, plain.ChecksumSHA1, plain.ChecksumSHA256) != "" {
					t.Errorf("%s: checksum returned without x-amz-checksum-mode", algo)
				}

				// GET with checksum mode: the SDK validates the body against it.
				get, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String(key), ChecksumMode: s3types.ChecksumModeEnabled})
				must(t, err, "GetObject")
				body := readAll(t, get.Body)
				if !bytes.Equal(body, data) {
					t.Fatalf("%s: body differs", algo)
				}
				if got := checksumOf(algo, get.ChecksumCRC32, get.ChecksumCRC32C, get.ChecksumCRC64NVME, get.ChecksumSHA1, get.ChecksumSHA256); got != want {
					t.Errorf("%s: GET checksum %q, want %q", algo, got, want)
				}

				// Precomputed value sent by the caller.
				_, err = c.PutObject(ctx(t), &s3.PutObjectInput{
					Bucket: aws.String(b), Key: aws.String(key + "-pre"), Body: bytes.NewReader(data),
					ChecksumCRC32: pick(algo == s3types.ChecksumAlgorithmCrc32, want), ChecksumCRC32C: pick(algo == s3types.ChecksumAlgorithmCrc32c, want),
					ChecksumCRC64NVME: pick(algo == s3types.ChecksumAlgorithmCrc64nvme, want), ChecksumSHA1: pick(algo == s3types.ChecksumAlgorithmSha1, want),
					ChecksumSHA256: pick(algo == s3types.ChecksumAlgorithmSha256, want),
				})
				must(t, err, "PutObject precomputed "+string(algo))

				// A wrong value is rejected and nothing is stored.
				wrong := checksumB64(algo, []byte("something else"))
				resp := e.signed(t, rootCreds, http.MethodPut, "/"+b+"/bad-"+string(algo), data, map[string]string{checksumHeaderName(algo): wrong})
				if resp.status != http.StatusBadRequest || resp.code() != "BadDigest" {
					t.Errorf("%s: wrong checksum: %d %s", algo, resp.status, resp.code())
				}
				resp = e.signed(t, rootCreds, http.MethodPut, "/"+b+"/bad-"+string(algo), data, map[string]string{checksumHeaderName(algo): "not base64!"})
				if resp.status != http.StatusBadRequest || resp.code() != "InvalidRequest" {
					t.Errorf("%s: malformed checksum: %d %s", algo, resp.status, resp.code())
				}
				_, err = c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String("bad-" + string(algo))})
				if httpStatus(err) != http.StatusNotFound {
					t.Errorf("%s: object stored despite bad checksum: %v", algo, err)
				}
			}

			// No checksum from the client: the server stores a CRC64NVME.
			raw := e.signed(t, rootCreds, http.MethodPut, "/"+b+"/default", data, nil)
			if raw.status != 200 || raw.header.Get("x-amz-checksum-crc64nvme") != checksumB64(s3types.ChecksumAlgorithmCrc64nvme, data) ||
				raw.header.Get("x-amz-checksum-type") != "FULL_OBJECT" {
				t.Errorf("default checksum: %d %v", raw.status, raw.header)
			}
		})
	}
}

func pick(ok bool, v string) *string {
	if ok {
		return aws.String(v)
	}
	return nil
}

func TestContentMD5AndPayloadHash(t *testing.T) {
	e := fsEnv
	b := e.newBucket(t)
	data := []byte("content md5 body")
	sum := md5Sum(data)
	good := base64.StdEncoding.EncodeToString(sum[:])
	resp := e.signed(t, rootCreds, http.MethodPut, "/"+b+"/md5", data, map[string]string{"Content-MD5": good})
	if resp.status != 200 {
		t.Fatalf("good Content-MD5: %d %s", resp.status, resp.body)
	}
	bad := base64.StdEncoding.EncodeToString(make([]byte, 16))
	resp = e.signed(t, rootCreds, http.MethodPut, "/"+b+"/md5", data, map[string]string{"Content-MD5": bad})
	if resp.status != 400 || resp.code() != "BadDigest" {
		t.Errorf("wrong Content-MD5: %d %s", resp.status, resp.code())
	}
	resp = e.signed(t, rootCreds, http.MethodPut, "/"+b+"/md5", data, map[string]string{"Content-MD5": "abc"})
	if resp.status != 400 || resp.code() != "InvalidDigest" {
		t.Errorf("malformed Content-MD5: %d %s", resp.status, resp.code())
	}
	// Signed payload hash that does not match the body.
	resp = e.do(t, rawRequest{method: http.MethodPut, path: "/" + b + "/sha", body: data, creds: &rootCreds,
		payloadHash: strings.Repeat("ab", 32)})
	if resp.status != 400 || resp.code() != "XAmzContentSHA256Mismatch" {
		t.Errorf("payload hash mismatch: %d %s", resp.status, resp.code())
	}
	if got := string(e.get(t, b, "md5")); got != string(data) {
		t.Errorf("object = %q", got)
	}
}

// awsChunkedUnsigned encodes data as an aws-chunked body with unsigned
// chunks and the given trailer lines.
func awsChunkedUnsigned(data []byte, chunk int, trailers ...string) []byte {
	var buf bytes.Buffer
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		fmt.Fprintf(&buf, "%x\r\n", end-off)
		buf.Write(data[off:end])
		buf.WriteString("\r\n")
	}
	buf.WriteString("0\r\n")
	for _, tr := range trailers {
		buf.WriteString(tr + "\r\n")
	}
	buf.WriteString("\r\n")
	return buf.Bytes()
}

// TestUnsignedTrailerRaw sends hand-made STREAMING-UNSIGNED-PAYLOAD-TRAILER
// requests: a correct trailer checksum is stored; a wrong one is BadDigest.
func TestUnsignedTrailerRaw(t *testing.T) {
	e := fsEnv
	b := e.newBucket(t)
	data := randomData(150_000, 4)
	for _, algo := range algorithms {
		name := checksumHeaderName(algo)
		send := func(key, value string) response {
			body := awsChunkedUnsigned(data, 64*1024, name+":"+value)
			return e.do(t, rawRequest{
				method: http.MethodPut, path: "/" + b + "/" + key, body: body, creds: &rootCreds,
				payloadHash: "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
				header: map[string]string{
					"Content-Encoding":             "aws-chunked",
					"X-Amz-Decoded-Content-Length": strconv.Itoa(len(data)),
					"X-Amz-Trailer":                name,
				},
			})
		}
		resp := send("ok-"+string(algo), checksumB64(algo, data))
		if resp.status != 200 || resp.header.Get(name) != checksumB64(algo, data) {
			t.Errorf("%s trailer: %d %s %v", algo, resp.status, resp.body, resp.header)
			continue
		}
		if !bytes.Equal(e.get(t, b, "ok-"+string(algo)), data) {
			t.Errorf("%s trailer: stored data differs", algo)
		}
		head, err := e.root().HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String("ok-" + string(algo))})
		must(t, err, "HeadObject")
		if aws.ToString(head.ContentEncoding) != "" {
			t.Errorf("%s: aws-chunked kept in Content-Encoding: %q", algo, aws.ToString(head.ContentEncoding))
		}
		resp = send("bad-"+string(algo), checksumB64(algo, []byte("nope")))
		if resp.status != 400 || resp.code() != "BadDigest" {
			t.Errorf("%s wrong trailer: %d %s", algo, resp.status, resp.code())
		}
	}
}

// TestMultipartChecksums covers COMPOSITE and FULL_OBJECT multipart
// checksums for every algorithm, including the invalid combinations.
func TestMultipartChecksums(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			b := e.newBucket(t)
			parts := [][]byte{randomData(5<<20, 5), randomData(5<<20+3, 6), randomData(1234, 7)}
			var whole []byte
			for _, p := range parts {
				whole = append(whole, p...)
			}
			for _, algo := range algorithms {
				for _, typ := range []s3types.ChecksumType{s3types.ChecksumTypeComposite, s3types.ChecksumTypeFullObject} {
					name := fmt.Sprintf("%s/%s", algo, typ)
					key := "mp-" + string(algo) + "-" + string(typ)
					isCRC := algo != s3types.ChecksumAlgorithmSha1 && algo != s3types.ChecksumAlgorithmSha256
					valid := (typ == s3types.ChecksumTypeComposite && algo != s3types.ChecksumAlgorithmCrc64nvme) ||
						(typ == s3types.ChecksumTypeFullObject && isCRC)
					cr, err := c.CreateMultipartUpload(ctx(t), &s3.CreateMultipartUploadInput{
						Bucket: aws.String(b), Key: aws.String(key), ChecksumAlgorithm: algo, ChecksumType: typ,
					})
					if !valid {
						if apiCode(err) != "InvalidRequest" {
							t.Errorf("%s: want InvalidRequest, got %v", name, err)
						}
						continue
					}
					must(t, err, "CreateMultipartUpload "+name)
					if cr.ChecksumAlgorithm != algo || cr.ChecksumType != typ {
						t.Errorf("%s: create echoed %q %q", name, cr.ChecksumAlgorithm, cr.ChecksumType)
					}
					var done []s3types.CompletedPart
					for i, p := range parts {
						up, err := c.UploadPart(ctx(t), &s3.UploadPartInput{
							Bucket: aws.String(b), Key: aws.String(key), UploadId: cr.UploadId, PartNumber: aws.Int32(int32(i + 1)),
							Body: bytes.NewReader(p), ChecksumAlgorithm: algo,
						})
						must(t, err, "UploadPart "+name)
						if got := checksumOf(algo, up.ChecksumCRC32, up.ChecksumCRC32C, up.ChecksumCRC64NVME, up.ChecksumSHA1, up.ChecksumSHA256); got != checksumB64(algo, p) {
							t.Errorf("%s part %d: checksum %q", name, i+1, got)
						}
						cp := s3types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(int32(i + 1))}
						v := aws.String(checksumB64(algo, p))
						switch algo {
						case s3types.ChecksumAlgorithmCrc32:
							cp.ChecksumCRC32 = v
						case s3types.ChecksumAlgorithmCrc32c:
							cp.ChecksumCRC32C = v
						case s3types.ChecksumAlgorithmCrc64nvme:
							cp.ChecksumCRC64NVME = v
						case s3types.ChecksumAlgorithmSha1:
							cp.ChecksumSHA1 = v
						case s3types.ChecksumAlgorithmSha256:
							cp.ChecksumSHA256 = v
						}
						done = append(done, cp)
					}
					// ListParts reports the part checksums.
					lp, err := c.ListParts(ctx(t), &s3.ListPartsInput{Bucket: aws.String(b), Key: aws.String(key), UploadId: cr.UploadId})
					must(t, err, "ListParts")
					if lp.ChecksumAlgorithm != algo || lp.ChecksumType != typ || len(lp.Parts) != len(parts) {
						t.Errorf("%s: ListParts algo %q type %q parts %d", name, lp.ChecksumAlgorithm, lp.ChecksumType, len(lp.Parts))
					}
					for i, p := range lp.Parts {
						if got := checksumOf(algo, p.ChecksumCRC32, p.ChecksumCRC32C, p.ChecksumCRC64NVME, p.ChecksumSHA1, p.ChecksumSHA256); got != checksumB64(algo, parts[i]) {
							t.Errorf("%s: ListParts part %d checksum %q", name, i+1, got)
						}
					}

					want := compositeB64(algo, parts)
					if typ == s3types.ChecksumTypeFullObject {
						want = checksumB64(algo, whole)
					}
					comp, err := c.CompleteMultipartUpload(ctx(t), &s3.CompleteMultipartUploadInput{
						Bucket: aws.String(b), Key: aws.String(key), UploadId: cr.UploadId,
						MultipartUpload: &s3types.CompletedMultipartUpload{Parts: done},
					})
					must(t, err, "CompleteMultipartUpload "+name)
					if got := checksumOf(algo, comp.ChecksumCRC32, comp.ChecksumCRC32C, comp.ChecksumCRC64NVME, comp.ChecksumSHA1, comp.ChecksumSHA256); got != want {
						t.Errorf("%s: complete checksum %q, want %q", name, got, want)
					}
					if comp.ChecksumType != typ {
						t.Errorf("%s: complete checksum type %q", name, comp.ChecksumType)
					}
					head, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String(key), ChecksumMode: s3types.ChecksumModeEnabled})
					must(t, err, "HeadObject "+name)
					if got := checksumOf(algo, head.ChecksumCRC32, head.ChecksumCRC32C, head.ChecksumCRC64NVME, head.ChecksumSHA1, head.ChecksumSHA256); got != want || head.ChecksumType != typ {
						t.Errorf("%s: HEAD checksum %q type %q, want %q", name, got, head.ChecksumType, want)
					}
					if aws.ToInt64(head.ContentLength) != int64(len(whole)) || !strings.HasSuffix(aws.ToString(head.ETag), `-3"`) {
						t.Errorf("%s: HEAD size %d etag %s", name, aws.ToInt64(head.ContentLength), aws.ToString(head.ETag))
					}
					// Per-part checksum via partNumber.
					pg, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String(key), PartNumber: aws.Int32(2), ChecksumMode: s3types.ChecksumModeEnabled})
					must(t, err, "GetObject partNumber")
					if body := readAll(t, pg.Body); !bytes.Equal(body, parts[1]) {
						t.Errorf("%s: part 2 body differs", name)
					}
					if aws.ToInt32(pg.PartsCount) != 3 {
						t.Errorf("%s: PartsCount %d", name, aws.ToInt32(pg.PartsCount))
					}
					// The whole object reads back with full validation by the SDK
					// for FULL_OBJECT checksums.
					get, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String(key), ChecksumMode: s3types.ChecksumModeEnabled})
					must(t, err, "GetObject "+name)
					if body, err := io.ReadAll(get.Body); err != nil || !bytes.Equal(body, whole) {
						t.Errorf("%s: GET body differs (%v)", name, err)
					}
					get.Body.Close()
				}
			}
		})
	}
}

// TestMultipartFullObjectExpectedChecksum checks that a full-object
// checksum supplied with CompleteMultipartUpload is verified.
func TestMultipartFullObjectExpectedChecksum(t *testing.T) {
	e := fsEnv
	c := e.root()
	b := e.newBucket(t)
	parts := [][]byte{randomData(5<<20, 8), randomData(100, 9)}
	algo := s3types.ChecksumAlgorithmCrc32c
	for _, correct := range []bool{false, true} {
		key := fmt.Sprintf("full-%v", correct)
		cr, err := c.CreateMultipartUpload(ctx(t), &s3.CreateMultipartUploadInput{Bucket: aws.String(b), Key: aws.String(key),
			ChecksumAlgorithm: algo, ChecksumType: s3types.ChecksumTypeFullObject})
		must(t, err, "CreateMultipartUpload")
		var done []s3types.CompletedPart
		var whole []byte
		for i, p := range parts {
			whole = append(whole, p...)
			up, err := c.UploadPart(ctx(t), &s3.UploadPartInput{Bucket: aws.String(b), Key: aws.String(key), UploadId: cr.UploadId,
				PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(p), ChecksumAlgorithm: algo})
			must(t, err, "UploadPart")
			done = append(done, s3types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(int32(i + 1)), ChecksumCRC32C: up.ChecksumCRC32C})
		}
		expected := checksumB64(algo, whole)
		if !correct {
			expected = checksumB64(algo, []byte("wrong"))
		}
		_, err = c.CompleteMultipartUpload(ctx(t), &s3.CompleteMultipartUploadInput{Bucket: aws.String(b), Key: aws.String(key), UploadId: cr.UploadId,
			ChecksumCRC32C: aws.String(expected), ChecksumType: s3types.ChecksumTypeFullObject,
			MultipartUpload: &s3types.CompletedMultipartUpload{Parts: done}})
		if correct {
			must(t, err, "CompleteMultipartUpload with full-object checksum")
		} else if apiCode(err) != "BadDigest" && apiCode(err) != "InvalidRequest" {
			t.Errorf("wrong full-object checksum: %v", err)
		}
	}
}

func md5Sum(b []byte) [16]byte {
	var out [16]byte
	copy(out[:], rawMD5(b))
	return out
}
