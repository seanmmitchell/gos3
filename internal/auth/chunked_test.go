package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	miniosigner "github.com/minio/minio-go/v7/pkg/signer"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// TestAWSStreamingExample verifies the AWS documentation example for
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD (66560 bytes of 'a', 64 KiB chunks).
func TestAWSStreamingExample(t *testing.T) {
	const (
		seed = "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9"
		sig1 = "ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648"
		sig2 = "0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497"
		sig3 = "b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9"
	)
	build := func(s1 string) *http.Request {
		var body bytes.Buffer
		body.WriteString("10000;chunk-signature=" + s1 + "\r\n")
		body.Write(bytes.Repeat([]byte("a"), 65536))
		body.WriteString("\r\n400;chunk-signature=" + sig2 + "\r\n")
		body.Write(bytes.Repeat([]byte("a"), 1024))
		body.WriteString("\r\n0;chunk-signature=" + sig3 + "\r\n\r\n")
		if body.Len() != 66824 {
			t.Fatalf("encoded length %d", body.Len())
		}
		r := httptest.NewRequest("PUT", "/examplebucket/chunkObject.txt", &body)
		r.Host = "s3.amazonaws.com"
		r.Header.Set("X-Amz-Date", "20130524T000000Z")
		r.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
		r.Header.Set("X-Amz-Content-Sha256", PayloadStreamingSigned)
		r.Header.Set("Content-Encoding", "aws-chunked")
		r.Header.Set("X-Amz-Decoded-Content-Length", "66560")
		r.Header.Set("Content-Length", "66824")
		r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+testAK+"/20130524/us-east-1/s3/aws4_request,"+
			"SignedHeaders=content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class,"+
			"Signature="+seed)
		return r
	}
	v := newTestVerifier(docTime)
	o := runAuth(v, build(sig1))
	if o.Code != "" {
		t.Fatalf("%s: %s (%s)", o.Code, o.Message, o.Stage)
	}
	if !bytes.Equal(o.Body, bytes.Repeat([]byte("a"), 66560)) || o.DecodedLen != 66560 || o.PayloadMode != PayloadStreamingSigned {
		t.Fatalf("unexpected outcome: len=%d decoded=%d mode=%s", len(o.Body), o.DecodedLen, o.PayloadMode)
	}
	// A wrong first chunk signature is detected while reading.
	o = runAuth(v, build(strings.Repeat("0", 64)))
	if o.Code != "SignatureDoesNotMatch" || o.Stage != "body" {
		t.Fatalf("tampered chunk signature: %+v", o.Code)
	}
}

// --- independent test encoder -------------------------------------------

type testChunkKey struct {
	key     []byte
	amzDate string
	scope   string
}

func testHMAC(key []byte, s string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return hex.EncodeToString(m.Sum(nil))
}

func testSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (k *testChunkKey) chunkSig(prev string, data []byte) string {
	return testHMAC(k.key, "AWS4-HMAC-SHA256-PAYLOAD\n"+k.amzDate+"\n"+k.scope+"\n"+prev+"\n"+
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\n"+testSHA(data))
}

func (k *testChunkKey) trailerSig(prev string, canonical string) string {
	return testHMAC(k.key, "AWS4-HMAC-SHA256-TRAILER\n"+k.amzDate+"\n"+k.scope+"\n"+prev+"\n"+testSHA([]byte(canonical)))
}

type encOpts struct {
	chunkSize  int
	key        *testChunkKey // nil: unsigned chunks
	seed       string
	trailers   [][2]string
	signTrail  bool
	minioStyle bool // minio-go trailer layout ("name:value\n" + "\r\n" ...)
}

// encodeChunked builds an aws-chunked body.
func encodeChunked(data []byte, o encOpts) []byte {
	var b bytes.Buffer
	prev := o.seed
	for off := 0; off < len(data); off += o.chunkSize {
		end := min(off+o.chunkSize, len(data))
		chunk := data[off:end]
		if o.key != nil {
			prev = o.key.chunkSig(prev, chunk)
			fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(chunk), prev)
		} else {
			fmt.Fprintf(&b, "%x\r\n", len(chunk))
		}
		b.Write(chunk)
		b.WriteString("\r\n")
	}
	if o.key != nil {
		prev = o.key.chunkSig(prev, nil)
		b.WriteString("0;chunk-signature=" + prev + "\r\n")
	} else {
		b.WriteString("0\r\n")
	}
	var canon strings.Builder
	for _, tr := range o.trailers {
		canon.WriteString(tr[0] + ":" + tr[1] + "\n")
	}
	if o.minioStyle && len(o.trailers) > 0 {
		b.WriteString(canon.String())
		b.WriteString("\r\n")
		if o.signTrail {
			b.WriteString("x-amz-trailer-signature:" + o.key.trailerSig(prev, canon.String()) + "\r\n\r\n")
		} else {
			b.WriteString("\r\n")
		}
		return b.Bytes()
	}
	for _, tr := range o.trailers {
		b.WriteString(tr[0] + ":" + tr[1] + "\r\n")
	}
	if o.signTrail {
		b.WriteString("x-amz-trailer-signature:" + o.key.trailerSig(prev, canon.String()) + "\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

type streamCase struct {
	mode       string
	data       []byte
	chunkSize  int
	trailers   [][2]string
	minioStyle bool
	header     func(h http.Header)         // before signing
	mutate     func(body []byte) []byte    // after encoding
	post       func(r *http.Request)       // after signing (server side)
	at         time.Time                   // zero: now
	encMod     func(o *encOpts)            // tweak the encoder
	declare    func(names []string) string // x-amz-trailer value
}

// streamingRequest builds a server-side aws-chunked request signed by the
// AWS SDK signer (seed) and the independent test encoder (chunks).
func streamingRequest(t *testing.T, c streamCase) *http.Request {
	t.Helper()
	at := c.at
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if c.chunkSize == 0 {
		c.chunkSize = 64 << 10
	}
	req, _ := http.NewRequest("PUT", "http://bucket.example.com/key", nil)
	req.Header.Set("X-Amz-Content-Sha256", c.mode)
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(c.data)))
	if len(c.trailers) > 0 {
		var names []string
		for _, tr := range c.trailers {
			names = append(names, tr[0])
		}
		decl := strings.Join(names, ",")
		if c.declare != nil {
			decl = c.declare(names)
		}
		if decl != "" {
			req.Header.Set("X-Amz-Trailer", decl)
		}
	}
	if c.header != nil {
		c.header(req.Header)
	}
	date := at.Format("20060102")
	key := &testChunkKey{key: SigningKey(testSK, date, "us-east-1", "s3"), amzDate: at.Format("20060102T150405Z"), scope: date + "/us-east-1/s3/aws4_request"}
	opts := encOpts{chunkSize: c.chunkSize, trailers: c.trailers, minioStyle: c.minioStyle}
	switch c.mode {
	case PayloadStreamingSigned:
		opts.key = key
	case PayloadStreamingSignedTrailer:
		opts.key, opts.signTrail = key, true
	}
	if c.encMod != nil {
		c.encMod(&opts)
	}
	opts.seed = strings.Repeat("0", 64)
	probe := encodeChunked(c.data, opts)
	if c.mutate != nil {
		// Mutations are length-deterministic; sign the final length.
		probe = c.mutate(probe)
	}
	req.ContentLength = int64(len(probe))
	sdkSign(t, req, testAK, testSK, c.mode, "us-east-1", at)
	authz := req.Header.Get("Authorization")
	opts.seed = authz[strings.LastIndex(authz, "=")+1:]
	body := encodeChunked(c.data, opts)
	if c.mutate != nil {
		body = c.mutate(body)
	}
	sr := toServer(req, body)
	if c.post != nil {
		c.post(sr)
	}
	return sr
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func TestStreamingRoundTrip(t *testing.T) {
	v := liveVerifier()
	crc := [][2]string{{"x-amz-checksum-crc32", "AAAAAA=="}}
	for _, mode := range []string{PayloadStreamingSigned, PayloadStreamingSignedTrailer, PayloadStreamingUnsignedTrailer} {
		for _, size := range []int{0, 1, 8191, 65536, 65537, 200001} {
			for _, cs := range []int{1, 8192, 65536} {
				if cs == 1 && size > 8192 {
					continue
				}
				for _, minio := range []bool{false, true} {
					var trailers [][2]string
					if mode != PayloadStreamingSigned {
						trailers = crc
					}
					name := fmt.Sprintf("%s/size=%d/chunk=%d/minio=%v", mode, size, cs, minio)
					t.Run(name, func(t *testing.T) {
						data := pattern(size)
						r := streamingRequest(t, streamCase{mode: mode, data: data, chunkSize: cs, trailers: trailers, minioStyle: minio})
						o := runAuth(v, r)
						if o.Code != "" {
							t.Fatalf("%s: %s (%s)", o.Code, o.Message, o.Stage)
						}
						if !bytes.Equal(o.Body, data) || o.DecodedLen != int64(size) || o.PayloadMode != mode {
							t.Fatalf("decoded %d bytes, decodedLen=%d mode=%s", len(o.Body), o.DecodedLen, o.PayloadMode)
						}
						if len(trailers) > 0 && (o.Trailer["X-Amz-Checksum-Crc32"] == nil || o.Trailer["X-Amz-Checksum-Crc32"][0] != "AAAAAA==") {
							t.Fatalf("trailer = %v", o.Trailer)
						}
					})
				}
			}
		}
	}
}

// TestStreamingUnsignedSingleLargeChunk: the AWS SDK for Go v2 sends a
// STREAMING-UNSIGNED-PAYLOAD-TRAILER body of known length as one chunk, so
// unsigned chunks larger than MaxChunkSize must be accepted (regression:
// every SDK upload over 64 MiB via HTTPS failed with InvalidRequest).
func TestStreamingUnsignedSingleLargeChunk(t *testing.T) {
	v := liveVerifier()
	data := pattern(MaxChunkSize + 4096)
	crc := [][2]string{{"x-amz-checksum-crc32", "AAAAAA=="}}
	r := streamingRequest(t, streamCase{mode: PayloadStreamingUnsignedTrailer, data: data, chunkSize: len(data), trailers: crc})
	o := runAuth(v, r)
	if o.Code != "" {
		t.Fatalf("%s: %s (%s)", o.Code, o.Message, o.Stage)
	}
	if !bytes.Equal(o.Body, data) || o.DecodedLen != int64(len(data)) {
		t.Fatalf("decoded %d bytes, decodedLen=%d", len(o.Body), o.DecodedLen)
	}
}

func TestStreamingMultipleTrailers(t *testing.T) {
	v := liveVerifier()
	trailers := [][2]string{{"x-amz-checksum-sha256", "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="}, {"x-amz-meta-extra", "v"}}
	for _, mode := range []string{PayloadStreamingSignedTrailer, PayloadStreamingUnsignedTrailer} {
		for _, minio := range []bool{false, true} {
			r := streamingRequest(t, streamCase{mode: mode, data: []byte("hi"), trailers: trailers, minioStyle: minio})
			o := runAuth(v, r)
			if o.Code != "" || len(o.Trailer) != 2 || o.Trailer["X-Amz-Meta-Extra"][0] != "v" {
				t.Fatalf("%s minio=%v: %+v", mode, minio, o)
			}
		}
	}
	// Undeclared trailers are accepted when x-amz-trailer is absent.
	r := streamingRequest(t, streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("hi"), trailers: trailers,
		declare: func([]string) string { return "" }})
	if o := runAuth(v, r); o.Code != "" || len(o.Trailer) != 2 {
		t.Fatalf("no declaration: %+v", o)
	}
	// Signed trailer mode with no trailers at all.
	r = streamingRequest(t, streamCase{mode: PayloadStreamingSignedTrailer, data: []byte("hi")})
	if o := runAuth(v, r); o.Code != "" || len(o.Trailer) != 0 {
		t.Fatalf("no trailers: %+v", o)
	}
}

func flipAt(i int) func([]byte) []byte {
	return func(b []byte) []byte {
		c := append([]byte(nil), b...)
		if i < 0 {
			i = len(c) + i
		}
		c[i] ^= 0x01
		return c
	}
}

func replaceOnce(old, new string) func([]byte) []byte {
	return func(b []byte) []byte { return bytes.Replace(b, []byte(old), []byte(new), 1) }
}

func TestStreamingErrors(t *testing.T) {
	v := liveVerifier()
	data := pattern(70000)
	crc := [][2]string{{"x-amz-checksum-crc32", "AAAAAA=="}}
	tests := []struct {
		name  string
		c     streamCase
		code  string
		stage string
	}{
		{"tampered data", streamCase{mode: PayloadStreamingSigned, data: data, mutate: flipAt(200)}, "SignatureDoesNotMatch", "body"},
		{"tampered last chunk", streamCase{mode: PayloadStreamingSigned, data: data, mutate: func(b []byte) []byte {
			i := bytes.LastIndex(b, []byte("\r\n0;chunk-signature="))
			return flipAt(i - 1)(b)
		}}, "SignatureDoesNotMatch", "body"},
		{"tampered chunk signature", streamCase{mode: PayloadStreamingSigned, data: data, mutate: func(b []byte) []byte {
			i := bytes.Index(b, []byte("chunk-signature=")) + len("chunk-signature=")
			c := append([]byte(nil), b...)
			if c[i] == 'a' {
				c[i] = 'b'
			} else {
				c[i] = 'a'
			}
			return c
		}}, "SignatureDoesNotMatch", "body"},
		{"tampered final signature", streamCase{mode: PayloadStreamingSigned, data: data, mutate: func(b []byte) []byte {
			i := bytes.LastIndex(b, []byte("chunk-signature=")) + len("chunk-signature=")
			c := append([]byte(nil), b...)
			c[i] = 'g'
			return c
		}}, "SignatureDoesNotMatch", "body"},
		{"missing chunk signature", streamCase{mode: PayloadStreamingSigned, data: []byte("abc"), mutate: func(b []byte) []byte {
			return append([]byte("3\r\nabc\r\n"), b[bytes.Index(b, []byte("0;")):]...)
		}}, "SignatureDoesNotMatch", "body"},
		{"reordered chunks", streamCase{mode: PayloadStreamingSigned, data: bytes.Repeat([]byte("ab"), 8), chunkSize: 2, encMod: func(o *encOpts) {}, mutate: func(b []byte) []byte {
			// Swap the first two chunks (same size): signatures chain, so this must fail.
			parts := bytes.SplitN(b, []byte("2;chunk"), 3)
			return bytes.Join([][]byte{parts[0], parts[2], parts[1]}, []byte("2;chunk"))
		}}, "", "any"},
		{"wrong trailer value", streamCase{mode: PayloadStreamingSignedTrailer, data: data, trailers: crc,
			mutate: replaceOnce("AAAAAA==", "BBBBBB==")}, "SignatureDoesNotMatch", "body"},
		{"wrong trailer value (minio)", streamCase{mode: PayloadStreamingSignedTrailer, data: data, trailers: crc, minioStyle: true,
			mutate: replaceOnce("AAAAAA==", "BBBBBB==")}, "SignatureDoesNotMatch", "body"},
		{"missing trailer signature", streamCase{mode: PayloadStreamingSignedTrailer, data: data, trailers: crc,
			encMod: func(o *encOpts) { o.signTrail = false }}, "SignatureDoesNotMatch", "body"},
		{"malformed trailer signature", streamCase{mode: PayloadStreamingSignedTrailer, data: data, trailers: crc,
			mutate: func(b []byte) []byte {
				i := bytes.Index(b, []byte("x-amz-trailer-signature:")) + len("x-amz-trailer-signature:")
				return append(append(append([]byte(nil), b[:i]...), "zz"...), b[i+2:]...)
			}}, "SignatureDoesNotMatch", "body"},
		{"undeclared trailer", streamCase{mode: PayloadStreamingUnsignedTrailer, data: data, trailers: crc,
			declare: func([]string) string { return "x-amz-checksum-sha256" }}, "MalformedTrailerError", "body"},
		{"missing declared trailer", streamCase{mode: PayloadStreamingUnsignedTrailer, data: data, trailers: crc,
			declare: func([]string) string { return "x-amz-checksum-crc32,x-amz-checksum-sha1" }}, "MalformedTrailerError", "body"},
		{"trailer in non-trailer mode", streamCase{mode: PayloadStreamingSigned, data: data, trailers: crc,
			header: func(h http.Header) { h.Del("X-Amz-Trailer") }}, "MalformedTrailerError", "body"},
		{"duplicate trailer", streamCase{mode: PayloadStreamingUnsignedTrailer, data: data,
			trailers: [][2]string{{"x-amz-checksum-crc32", "A"}, {"x-amz-checksum-crc32", "B"}}, declare: func([]string) string { return "" }}, "MalformedTrailerError", "body"},
		{"trailer without colon", streamCase{mode: PayloadStreamingUnsignedTrailer, data: data, trailers: crc,
			mutate: replaceOnce("x-amz-checksum-crc32:", "x-amz-checksum-crc32=")}, "MalformedTrailerError", "body"},
		{"invalid trailer name", streamCase{mode: PayloadStreamingUnsignedTrailer, data: data, trailers: [][2]string{{"bad name", "x"}},
			declare: func([]string) string { return "" }}, "MalformedTrailerError", "body"},
		{"too many trailers", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("x"), trailers: func() [][2]string {
			var tr [][2]string
			for i := 0; i <= maxTrailers; i++ {
				tr = append(tr, [2]string{fmt.Sprintf("x-amz-t%d", i), "v"})
			}
			return tr
		}(), declare: func([]string) string { return "" }}, "MalformedTrailerError", "body"},
		{"huge trailer", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("x"),
			trailers: [][2]string{{"x-amz-checksum-crc32", strings.Repeat("A", maxTrailerBytes)}}}, "MalformedTrailerError", "body"},
		{"decoded length too small", streamCase{mode: PayloadStreamingSigned, data: data,
			header: func(h http.Header) { h.Set("X-Amz-Decoded-Content-Length", "69999") }}, "IncompleteBody", "body"},
		{"decoded length too large", streamCase{mode: PayloadStreamingSigned, data: data,
			header: func(h http.Header) { h.Set("X-Amz-Decoded-Content-Length", "70001") }}, "IncompleteBody", "body"},
		{"decoded length invalid", streamCase{mode: PayloadStreamingSigned, data: data,
			header: func(h http.Header) { h.Set("X-Amz-Decoded-Content-Length", "-1") }}, "InvalidArgument", "auth"},
		{"decoded length missing (signed)", streamCase{mode: PayloadStreamingSigned, data: data,
			header: func(h http.Header) { h.Del("X-Amz-Decoded-Content-Length") }}, "MissingContentLength", "auth"},
		{"decoded length missing (unsigned trailer)", streamCase{mode: PayloadStreamingUnsignedTrailer, data: data, trailers: crc,
			header: func(h http.Header) { h.Del("X-Amz-Decoded-Content-Length") }}, "", ""},
		{"invalid x-amz-trailer", streamCase{mode: PayloadStreamingUnsignedTrailer, data: data, trailers: crc,
			declare: func([]string) string { return "bad name" }}, "InvalidArgument", "auth"},
		{"data not followed by CRLF", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("abc"), mutate: replaceOnce("abc\r\n", "abcX\n")}, "InvalidRequest", "body"},
		{"bad chunk size", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("abc"), mutate: replaceOnce("3\r\n", "z\r\n")}, "InvalidRequest", "body"},
		{"empty chunk size", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("abc"), mutate: replaceOnce("3\r\n", ";\r\n")}, "InvalidRequest", "body"},
		{"chunk size overflow", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("abc"), mutate: replaceOnce("3\r\n", "fffffffffffffffff\r\n")}, "InvalidRequest", "body"},
		{"signed chunk too large", streamCase{mode: PayloadStreamingSigned, data: []byte("abc"),
			header: func(h http.Header) { h.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(MaxChunkSize+1)) },
			mutate: replaceOnce("3;", strconv.FormatInt(MaxChunkSize+1, 16)+";")}, "InvalidRequest", "body"},
		{"unsigned chunk too large", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("abc"),
			header: func(h http.Header) { h.Del("X-Amz-Decoded-Content-Length") },
			mutate: replaceOnce("3\r\n", strconv.FormatInt(maxUnsignedChunkSize+1, 16)+"\r\n")}, "InvalidRequest", "body"},
		{"chunk longer than declared", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("abc"), mutate: replaceOnce("3\r\n", "4\r\n")}, "IncompleteBody", "body"},
		{"oversized chunk header", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("abc"),
			mutate: replaceOnce("3\r\n", "3;"+strings.Repeat("x", maxChunkHeaderLen)+"\r\n")}, "InvalidRequest", "body"},
		{"endless chunk header", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("abc"),
			mutate: func([]byte) []byte { return bytes.Repeat([]byte("1"), 1<<20) }}, "InvalidRequest", "body"},
		{"empty body", streamCase{mode: PayloadStreamingSigned, data: data, mutate: func([]byte) []byte { return nil }}, "IncompleteBody", "body"},
		{"extension tolerated (unsigned)", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("abc"), trailers: crc, mutate: replaceOnce("3\r\n", "3;foo=bar\r\n")}, "", ""},
		{"LF only lines tolerated", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("abc"), trailers: crc,
			mutate: func(b []byte) []byte {
				return []byte("3\nabc\r\n0\nx-amz-checksum-crc32:AAAAAA==\n\n")
			}}, "", ""},
		{"missing final CRLF tolerated", streamCase{mode: PayloadStreamingUnsignedTrailer, data: []byte("abc"),
			mutate: func(b []byte) []byte { return []byte("3\r\nabc\r\n0\r\n") }}, "", ""},
		{"trailer data after signature", streamCase{mode: PayloadStreamingSignedTrailer, data: data, trailers: crc,
			mutate: func(b []byte) []byte {
				return append(bytes.TrimSuffix(b, []byte("\r\n")), []byte("x-amz-extra:1\r\n\r\n")...)
			}}, "MalformedTrailerError", "body"},
		{"stale seed (body from another request)", streamCase{mode: PayloadStreamingSigned, data: data,
			encMod: func(o *encOpts) { o.key.scope = "20000101/us-east-1/s3/aws4_request" }}, "SignatureDoesNotMatch", "body"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := runAuth(v, streamingRequest(t, tc.c))
			if tc.stage == "any" {
				if o.Code == "" {
					t.Fatalf("accepted")
				}
				return
			}
			if o.Code != tc.code || (tc.stage != "" && o.Stage != tc.stage) {
				t.Fatalf("got code=%q stage=%q (%s), want %q at %q", o.Code, o.Stage, o.Message, tc.code, tc.stage)
			}
			if tc.code != "" && len(o.Body) != 0 {
				t.Fatalf("data returned despite error")
			}
		})
	}
}

// TestStreamingTruncation cuts valid bodies at every offset: decoding must
// fail (never succeed with partial data, never panic).
func TestStreamingTruncation(t *testing.T) {
	v := liveVerifier()
	crc := [][2]string{{"x-amz-checksum-crc32", "AAAAAA=="}}
	for _, mode := range []string{PayloadStreamingSigned, PayloadStreamingSignedTrailer, PayloadStreamingUnsignedTrailer} {
		var trailers [][2]string
		if mode != PayloadStreamingSigned {
			trailers = crc
		}
		full := streamingRequest(t, streamCase{mode: mode, data: pattern(100), chunkSize: 40, trailers: trailers})
		body, _ := io.ReadAll(full.Body)
		for cut := 0; cut < len(body); cut++ {
			r := streamingRequest(t, streamCase{mode: mode, data: pattern(100), chunkSize: 40, trailers: trailers,
				mutate: func(b []byte) []byte { return b[:cut] }})
			o := runAuth(v, r)
			// Cutting only the final CRLF of a trailer-less body is tolerated.
			if o.Code == "" && !(mode == PayloadStreamingSigned && cut >= len(body)-2) {
				t.Fatalf("%s: truncated body (%d/%d bytes) accepted", mode, cut, len(body))
			}
		}
	}
}

func TestStreamingSmallReads(t *testing.T) {
	v := liveVerifier()
	data := pattern(5000)
	r := streamingRequest(t, streamCase{mode: PayloadStreamingSignedTrailer, data: data, chunkSize: 1000,
		trailers: [][2]string{{"x-amz-checksum-crc32c", "sOO8/Q=="}}})
	res, err := v.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(iotest.OneByteReader(res.Body))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("one-byte reads: err=%v len=%d", err, len(got))
	}
	if res.Trailer().Get("X-Amz-Checksum-Crc32c") != "sOO8/Q==" {
		t.Fatalf("trailer: %v", res.Trailer())
	}
	// Errors are sticky and EOF repeats.
	if n, err := res.Body.Read(make([]byte, 10)); n != 0 || err != io.EOF {
		t.Fatalf("read after EOF: %d %v", n, err)
	}
	if n, err := res.Body.Read(nil); n != 0 || err != io.EOF {
		t.Fatalf("empty read after EOF: %d %v", n, err)
	}
	// Underlying reader errors mid-stream surface unchanged.
	r = streamingRequest(t, streamCase{mode: PayloadStreamingSigned, data: data})
	boom := errors.New("boom")
	r.Body = io.NopCloser(io.MultiReader(io.LimitReader(r.Body, 500), iotest.ErrReader(boom)))
	res, err = v.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(res.Body); !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
	// io.ErrUnexpectedEOF from the transport becomes IncompleteBody.
	r = streamingRequest(t, streamCase{mode: PayloadStreamingSigned, data: data})
	r.Body = io.NopCloser(io.MultiReader(io.LimitReader(r.Body, 500), iotest.ErrReader(io.ErrUnexpectedEOF)))
	res, _ = v.Authenticate(r)
	if _, err := io.ReadAll(res.Body); errCode(err) != "IncompleteBody" {
		t.Fatalf("got %v", err)
	}
}

type closeTracker struct {
	io.Reader
	mu     sync.Mutex
	closed bool
}

func (c *closeTracker) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func TestBodyCloseClosesRequestBody(t *testing.T) {
	v := liveVerifier()
	modes := map[string]*http.Request{
		"streaming": streamingRequest(t, streamCase{mode: PayloadStreamingSigned, data: []byte("x")}),
		"hex":       signedReq(t, "PUT", "/k", []byte("x"), time.Now(), nil),
		"unsigned": signedReq(t, "PUT", "/k", []byte("x"), time.Now(), func(r *http.Request) {
			r.Header.Set("X-Amz-Content-Sha256", PayloadUnsigned)
		}),
		"anonymous": httptest.NewRequest("PUT", "/k", strings.NewReader("x")),
	}
	for name, r := range modes {
		ct := &closeTracker{Reader: r.Body}
		r.Body = ct
		res, err := v.Authenticate(r)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = res.Body.Close()
		if !ct.closed {
			t.Errorf("%s: Close not propagated", name)
		}
	}
}

// --- genuine third-party encoders ----------------------------------------

type sha256Hasher struct{ hash.Hash }

func (sha256Hasher) Close() {}

func TestMinioGoStreamingSigner(t *testing.T) {
	srv := newAuthServer(t, liveVerifier())
	data := pattern(150000)
	t.Run("signed", func(t *testing.T) {
		req, _ := http.NewRequest("PUT", srv.URL+"/bucket/obj%20name", bytes.NewReader(data))
		req = miniosigner.StreamingSignV4(req, testAK, testSK, "", "us-east-1", int64(len(data)), time.Now().UTC(), sha256Hasher{sha256.New()})
		o := send(t, srv.Client(), req)
		if o.Code != "" || !bytes.Equal(o.Body, data) || o.PayloadMode != PayloadStreamingSigned {
			t.Fatalf("%s %s len=%d", o.Code, o.Message, len(o.Body))
		}
	})
	t.Run("signed trailer", func(t *testing.T) {
		req, _ := http.NewRequest("PUT", srv.URL+"/bucket/obj", bytes.NewReader(data))
		req.Trailer = http.Header{"X-Amz-Checksum-Crc32c": {"sOO8/Q=="}}
		req = miniosigner.StreamingSignV4(req, testAK, testSK, "", "us-east-1", int64(len(data)), time.Now().UTC(), sha256Hasher{sha256.New()})
		o := send(t, srv.Client(), req)
		if o.Code != "" || !bytes.Equal(o.Body, data) || o.PayloadMode != PayloadStreamingSignedTrailer {
			t.Fatalf("%s %s len=%d", o.Code, o.Message, len(o.Body))
		}
		if o.Trailer["X-Amz-Checksum-Crc32c"][0] != "sOO8/Q==" {
			t.Fatalf("trailer %v", o.Trailer)
		}
	})
	t.Run("unsigned trailer", func(t *testing.T) {
		req, _ := http.NewRequest("PUT", srv.URL+"/bucket/obj", bytes.NewReader(data))
		req.Header.Set("X-Amz-Content-Sha256", PayloadStreamingUnsignedTrailer)
		req = miniosigner.SignV4Trailer(*req, testAK, testSK, "", "us-east-1", http.Header{"X-Amz-Checksum-Crc32": {"AAAAAA=="}})
		o := send(t, srv.Client(), req)
		if o.Code != "" || !bytes.Equal(o.Body, data) || o.PayloadMode != PayloadStreamingUnsignedTrailer {
			t.Fatalf("%s %s len=%d", o.Code, o.Message, len(o.Body))
		}
		if o.Trailer["X-Amz-Checksum-Crc32"][0] != "AAAAAA==" {
			t.Fatalf("trailer %v", o.Trailer)
		}
	})
	t.Run("anonymous unsigned trailer", func(t *testing.T) {
		req, _ := http.NewRequest("PUT", srv.URL+"/bucket/obj", bytes.NewReader(data))
		req.Header.Set("X-Amz-Content-Sha256", PayloadStreamingUnsignedTrailer)
		req = miniosigner.UnsignedTrailer(*req, http.Header{"X-Amz-Checksum-Crc32": {"AAAAAA=="}})
		o := send(t, srv.Client(), req)
		if o.Code != "" || !o.Anonymous || !bytes.Equal(o.Body, data) || o.Trailer["X-Amz-Checksum-Crc32"][0] != "AAAAAA==" {
			t.Fatalf("%s %s len=%d trailer=%v", o.Code, o.Message, len(o.Body), o.Trailer)
		}
	})
	t.Run("presigned", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/bucket/some%20key?versionId=x", nil)
		req = miniosigner.PreSignV4(*req, testAK, testSK, "", "us-east-1", 300)
		o := send(t, srv.Client(), req)
		if o.Code != "" || o.Method != MethodPresignedV4 {
			t.Fatalf("%s %s", o.Code, o.Message)
		}
	})
	t.Run("sigv2", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/bucket/key", nil)
		req = miniosigner.SignV2(*req, testAK, testSK, false)
		o := send(t, srv.Client(), req)
		if o.Code != "AccessDenied" || !strings.Contains(o.Message, "Signature Version 2") {
			t.Fatalf("%s %s", o.Code, o.Message)
		}
	})
}

// s3Handler fronts the auth handler with minimal S3 responses so the real
// AWS SDK client accepts them.
type s3Recorder struct {
	mu   sync.Mutex
	last outcome
}

func (rec *s3Recorder) handler(v *Verifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := runAuth(v, r)
		rec.mu.Lock()
		rec.last = o
		rec.mu.Unlock()
		if o.Code != "" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>x</Message></Error>`, o.Code)
			return
		}
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
		w.WriteHeader(http.StatusOK)
	})
}

func TestAWSSDKPutObject(t *testing.T) {
	rec := &s3Recorder{}
	srv := httptest.NewTLSServer(rec.handler(liveVerifier()))
	defer srv.Close()
	httpClient := srv.Client()
	httpClient.Transport.(*http.Transport).TLSClientConfig.MinVersion = tls.VersionTLS12
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(testAK, testSK, ""),
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		HTTPClient:   httpClient,
	})
	data := pattern(300000)
	for _, tc := range []struct {
		name string
		in   *s3.PutObjectInput
	}{
		{"crc32 trailer", &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("dir/ünï cødé+key"),
			Body: bytes.NewReader(data), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32}},
		{"sha256 trailer, unseekable", &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("k"),
			Body: io.NopCloser(bytes.NewReader(data)), ContentLength: aws.Int64(int64(len(data))), ChecksumAlgorithm: types.ChecksumAlgorithmSha256}},
		{"default", &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("a b/c=d"),
			Body: bytes.NewReader(data), Metadata: map[string]string{"x": "  y  z "}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.PutObject(context.Background(), tc.in); err != nil {
				rec.mu.Lock()
				defer rec.mu.Unlock()
				t.Fatalf("PutObject: %v (%+v)", err, rec.last.Code+" "+rec.last.Message)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if !bytes.Equal(rec.last.Body, data) {
				t.Fatalf("body mismatch (%d bytes, mode %s)", len(rec.last.Body), rec.last.PayloadMode)
			}
			t.Logf("payload mode %q trailer %v", rec.last.PayloadMode, rec.last.Trailer)
		})
	}
	// Plain operations with query strings and empty bodies.
	if _, err := client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String("bucket"), Prefix: aws.String("a b+c/日本"), Delimiter: aws.String("/")}); err != nil {
		// The response body is not valid XML for ListObjectsV2; only auth matters.
		rec.mu.Lock()
		code := rec.last.Code
		rec.mu.Unlock()
		if code != "" {
			t.Fatalf("ListObjectsV2 auth failed: %s", code)
		}
	}
	// Wrong secret is rejected.
	bad := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(testAK, "wrong", ""),
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		HTTPClient:   httpClient,
	})
	if _, err := bad.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("k"), Body: bytes.NewReader(data)}); err == nil {
		t.Fatal("wrong secret accepted")
	}
	// Presigned GET from the SDK presign client.
	ps := s3.NewPresignClient(client)
	preq, err := ps.PresignGetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String("bucket"), Key: aws.String("dir/ünï cødé+key")})
	if err != nil {
		t.Fatal(err)
	}
	get, _ := http.NewRequest(preq.Method, preq.URL, nil)
	for k, vals := range preq.SignedHeader {
		if !strings.EqualFold(k, "host") {
			get.Header[k] = vals
		}
	}
	resp, err := httpClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		t.Fatalf("presigned GET: %d %s", resp.StatusCode, rec.last.Code)
	}
}

// FuzzChunkedReader feeds arbitrary bodies to the decoder: it must never
// panic, never return more bytes than the input, and a successful decode
// must report exactly the declared length.
func FuzzChunkedReader(f *testing.F) {
	f.Add([]byte("3\r\nabc\r\n0\r\n\r\n"), int64(3), false)
	f.Add([]byte("3\r\nabc\r\n0\r\nx-amz-checksum-crc32:AAAA\r\n\r\n"), int64(3), false)
	f.Add([]byte("3;chunk-signature="+strings.Repeat("a", 64)+"\r\nabc\r\n0;chunk-signature="+strings.Repeat("b", 64)+"\r\n\r\n"), int64(3), true)
	f.Add([]byte("ffffffff\r\n"), int64(-1), false)
	f.Add([]byte("0\r\nx:y\n\r\nx-amz-trailer-signature:abc\r\n\r\n"), int64(0), true)
	f.Fuzz(func(t *testing.T, body []byte, declared int64, signed bool) {
		if declared < -1 {
			declared = -1
		}
		var s *chunkSigner
		if signed {
			s = &chunkSigner{key: []byte("k"), amzDate: "20240101T000000Z", scope: "20240101/us-east-1/s3/aws4_request", prevSig: strings.Repeat("0", 64)}
		}
		th := &trailerHolder{}
		cr := newChunkedReader(io.NopCloser(bytes.NewReader(body)), s, true, declared, nil, th)
		out, err := io.ReadAll(cr)
		if len(out) > len(body) {
			t.Fatalf("decoded more than input")
		}
		if err == nil && declared >= 0 && int64(len(out)) != declared {
			t.Fatalf("decoded %d, declared %d", len(out), declared)
		}
		if err != nil {
			var e *s3err.Error
			if !errors.As(err, &e) {
				t.Fatalf("non-S3 error %v", err)
			}
		}
		_ = th.get()
	})
}
