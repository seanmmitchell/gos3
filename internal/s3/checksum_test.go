package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

func TestChecksumCheckValues(t *testing.T) {
	data := []byte("123456789")
	for algo, wantHex := range map[string]string{
		ChecksumCRC32:     "cbf43926",
		ChecksumCRC32C:    "e3069283",
		ChecksumCRC64NVME: "ae8b14860a799888",
		ChecksumSHA1:      "f7c3bc1d808e04732adf679965ccc34ca7ae3441",
		ChecksumSHA256:    "15e2b0d3c33891ebb0f1ef609ec419420c20e320ce94c65fbc8c3312448eb225",
	} {
		raw, _ := hex.DecodeString(wantHex)
		if got := computeChecksum(algo, data); got != base64.StdEncoding.EncodeToString(raw) {
			t.Errorf("%s: %s, want %s", algo, got, base64.StdEncoding.EncodeToString(raw))
		}
		if checksumSize(algo) != len(raw) {
			t.Errorf("%s size %d", algo, checksumSize(algo))
		}
		if _, ok := decodeChecksum(algo, base64.StdEncoding.EncodeToString(raw)); !ok {
			t.Errorf("%s decode", algo)
		}
		if _, ok := decodeChecksum(algo, base64.StdEncoding.EncodeToString(raw[1:])); ok {
			t.Errorf("%s short value accepted", algo)
		}
	}
	if newChecksumHash("MD4") != nil || computeChecksum("nope", data) != "" || checksumSize("x") != 0 {
		t.Fatal("unknown algorithm")
	}
	for in, want := range map[string]string{"crc32": "CRC32", " Crc64Nvme ": "CRC64NVME", "sha256": "SHA256"} {
		if got, ok := normalizeChecksumAlgorithm(in); !ok || got != want {
			t.Errorf("%q => %q", in, got)
		}
	}
	if _, ok := normalizeChecksumAlgorithm("md5"); ok {
		t.Fatal("md5 accepted")
	}
	if checksumHeader(ChecksumCRC64NVME) != "x-amz-checksum-crc64nvme" {
		t.Fatal(checksumHeader(ChecksumCRC64NVME))
	}
}

func TestCombineFullObjectChecksum(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	data := make([]byte, 100000)
	rng.Read(data)
	for _, algo := range []string{ChecksumCRC32, ChecksumCRC32C, ChecksumCRC64NVME} {
		for trial := 0; trial < 20; trial++ {
			// Random split points, including empty parts.
			n := 1 + rng.Intn(6)
			cuts := []int{0}
			for i := 0; i < n-1; i++ {
				cuts = append(cuts, rng.Intn(len(data)+1))
			}
			cuts = append(cuts, len(data))
			sortInts(cuts)
			var parts []checksumPart
			for i := 0; i+1 < len(cuts); i++ {
				p := data[cuts[i]:cuts[i+1]]
				parts = append(parts, checksumPart{Checksum: computeChecksum(algo, p), Size: int64(len(p))})
			}
			got, err := combineFullObjectChecksum(algo, parts)
			if err != nil || got != computeChecksum(algo, data) {
				t.Fatalf("%s cuts %v: %s %v, want %s", algo, cuts, got, err, computeChecksum(algo, data))
			}
		}
		if got, _ := combineFullObjectChecksum(algo, nil); got != computeChecksum(algo, nil) {
			t.Errorf("%s empty: %s", algo, got)
		}
	}
	if _, err := combineFullObjectChecksum(ChecksumSHA256, nil); codeOf(err) != "InvalidRequest" {
		t.Fatal("sha256 full object accepted")
	}
	if _, err := combineFullObjectChecksum(ChecksumCRC32, []checksumPart{{Checksum: "bad", Size: 1}}); codeOf(err) != "InvalidPart" {
		t.Fatal("bad part checksum accepted")
	}
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func TestCompositeChecksumAndETag(t *testing.T) {
	p1, p2 := []byte("hello "), []byte("world")
	c1, c2 := computeChecksum(ChecksumCRC32, p1), computeChecksum(ChecksumCRC32, p2)
	r1, _ := base64.StdEncoding.DecodeString(c1)
	r2, _ := base64.StdEncoding.DecodeString(c2)
	sum := crc32.ChecksumIEEE(append(append([]byte{}, r1...), r2...))
	var want [4]byte
	binary.BigEndian.PutUint32(want[:], sum)
	got, err := compositeChecksum(ChecksumCRC32, []string{c1, c2})
	if err != nil || got != base64.StdEncoding.EncodeToString(want[:])+"-2" {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := compositeChecksum(ChecksumSHA256, []string{c1}); codeOf(err) != "InvalidPart" {
		t.Fatal("wrong-size part checksum accepted")
	}
	if _, err := compositeChecksum("X", nil); err == nil {
		t.Fatal("unknown algorithm")
	}

	m1, m2 := md5.Sum(p1), md5.Sum(p2)
	all := md5.Sum(append(m1[:], m2[:]...))
	etag, err := multipartETag([]string{hex.EncodeToString(m1[:]), `"` + hex.EncodeToString(m2[:]) + `"`})
	if err != nil || etag != hex.EncodeToString(all[:])+"-2" {
		t.Fatalf("%s %v", etag, err)
	}
	if _, err := multipartETag([]string{"zz"}); codeOf(err) != "InvalidPart" {
		t.Fatal("bad etag accepted")
	}
}

func TestChecksumTypes(t *testing.T) {
	for _, tc := range []struct {
		algo, typ, want string
		err             bool
	}{
		{"", "", "", false},
		{"", "COMPOSITE", "", true},
		{ChecksumCRC32, "", ChecksumTypeComposite, false},
		{ChecksumCRC64NVME, "", ChecksumTypeFullObject, false},
		{ChecksumCRC32C, ChecksumTypeFullObject, ChecksumTypeFullObject, false},
		{ChecksumSHA256, ChecksumTypeFullObject, "", true},
		{ChecksumCRC64NVME, ChecksumTypeComposite, "", true},
		{ChecksumSHA1, ChecksumTypeComposite, ChecksumTypeComposite, false},
	} {
		got, err := validateChecksumType(tc.algo, tc.typ)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("%+v: %q %v", tc, got, err)
		}
	}
	if v, err := parseChecksumType("full_object"); err != nil || v != ChecksumTypeFullObject {
		t.Fatal(v, err)
	}
	if _, err := parseChecksumType("bogus"); err == nil {
		t.Fatal("bogus type")
	}
}

// payloadCase builds a payload reader for a body with headers.
func newTestPayload(t *testing.T, body []byte, hdr map[string]string, declared int64, trailer http.Header, opts payloadOptions) (*payloadReader, error) {
	t.Helper()
	s := New(Options{Backend: memory.New(), MaxObjectSize: 1 << 20})
	r := httptest.NewRequest("PUT", "/b/k", bytes.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if trailer == nil {
		trailer = http.Header{}
	}
	rc := &reqContext{
		body: io.NopCloser(bytes.NewReader(body)),
		auth: &auth.Result{DecodedContentLength: declared, Trailer: func() http.Header { return trailer }},
	}
	return s.newPayloadReader(r, rc, opts)
}

func TestPayloadReader(t *testing.T) {
	body := []byte("123456789")
	md5b64 := func(b []byte) string { s := md5.Sum(b); return base64.StdEncoding.EncodeToString(s[:]) }
	n := int64(len(body))
	type tc struct {
		name     string
		hdr      map[string]string
		declared int64
		trailer  http.Header
		opts     payloadOptions
		setupErr string
		readErr  string
		sums     []string // algorithms expected in Checksums()
	}
	cases := []tc{
		{name: "plain", declared: n},
		{name: "unknown length", declared: -1},
		{name: "content-md5 ok", hdr: map[string]string{"Content-MD5": md5b64(body)}, declared: n},
		{name: "content-md5 mismatch", hdr: map[string]string{"Content-MD5": md5b64([]byte("x"))}, declared: n, readErr: "BadDigest"},
		{name: "content-md5 malformed", hdr: map[string]string{"Content-MD5": "nope!"}, declared: n, setupErr: "InvalidDigest"},
		{name: "content-md5 wrong length", hdr: map[string]string{"Content-MD5": "AAAA"}, declared: n, setupErr: "InvalidDigest"},
		{name: "two checksum headers", hdr: map[string]string{"x-amz-checksum-crc32": computeChecksum(ChecksumCRC32, body), "x-amz-checksum-sha1": computeChecksum(ChecksumSHA1, body)}, declared: n, setupErr: "InvalidRequest"},
		{name: "malformed checksum header", hdr: map[string]string{"x-amz-checksum-crc32": "AAAAAAAA"}, declared: n, setupErr: "InvalidRequest"},
		{name: "sdk algorithm mismatch", hdr: map[string]string{"x-amz-checksum-crc32": computeChecksum(ChecksumCRC32, body), "x-amz-sdk-checksum-algorithm": "SHA256"}, declared: n, setupErr: "InvalidRequest"},
		{name: "sdk algorithm unknown", hdr: map[string]string{"x-amz-sdk-checksum-algorithm": "MD5"}, declared: n, setupErr: "InvalidRequest"},
		{name: "sdk algorithm only", hdr: map[string]string{"x-amz-sdk-checksum-algorithm": "crc32c"}, declared: n, sums: []string{ChecksumCRC32C}},
		{name: "unsupported trailer", hdr: map[string]string{"x-amz-trailer": "x-amz-foo"}, declared: n, setupErr: "InvalidRequest"},
		{name: "trailer + header", hdr: map[string]string{"x-amz-trailer": "x-amz-checksum-crc32", "x-amz-checksum-crc32": computeChecksum(ChecksumCRC32, body)}, declared: n, setupErr: "InvalidRequest"},
		{name: "trailer ok", hdr: map[string]string{"x-amz-trailer": "x-amz-checksum-crc64nvme"}, declared: n,
			trailer: http.Header{"X-Amz-Checksum-Crc64nvme": {computeChecksum(ChecksumCRC64NVME, body)}}, sums: []string{ChecksumCRC64NVME}},
		{name: "trailer mismatch", hdr: map[string]string{"x-amz-trailer": "x-amz-checksum-crc32"}, declared: n,
			trailer: http.Header{"X-Amz-Checksum-Crc32": {computeChecksum(ChecksumCRC32, []byte("x"))}}, readErr: "BadDigest"},
		{name: "trailer malformed", hdr: map[string]string{"x-amz-trailer": "x-amz-checksum-sha256"}, declared: n,
			trailer: http.Header{"X-Amz-Checksum-Sha256": {"AAAA"}}, readErr: "InvalidRequest"},
		{name: "trailer missing", hdr: map[string]string{"x-amz-trailer": "x-amz-checksum-crc32"}, declared: n, readErr: "MalformedTrailerError"},
		{name: "declared too large", declared: 2 << 20, setupErr: "EntityTooLarge"},
		{name: "max size option", declared: n, opts: payloadOptions{MaxSize: 5}, setupErr: "EntityTooLarge"},
		{name: "streamed too large", declared: -1, opts: payloadOptions{MaxSize: 5}, readErr: "EntityTooLarge"},
		{name: "short body", declared: n + 1, readErr: "IncompleteBody"},
		{name: "long body", declared: n - 1, readErr: "IncompleteBody"},
		{name: "require length", declared: -1, opts: payloadOptions{RequireLength: true}, setupErr: "MissingContentLength"},
		{name: "extra algorithms", declared: n, opts: payloadOptions{Algorithms: []string{"crc32", "sha256", "bogus"}}, sums: []string{ChecksumCRC32, ChecksumSHA256}},
	}
	for _, algo := range checksumAlgorithms {
		cases = append(cases,
			tc{name: "header " + algo, hdr: map[string]string{checksumHeader(algo): computeChecksum(algo, body)}, declared: n, sums: []string{algo}},
			tc{name: "header mismatch " + algo, hdr: map[string]string{checksumHeader(algo): computeChecksum(algo, []byte("other"))}, declared: n, readErr: "BadDigest"},
			tc{name: "trailer " + algo, hdr: map[string]string{"x-amz-trailer": checksumHeader(algo)}, declared: n,
				trailer: http.Header{http.CanonicalHeaderKey(checksumHeader(algo)): {computeChecksum(algo, body)}}, sums: []string{algo}},
		)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := newTestPayload(t, body, c.hdr, c.declared, c.trailer, c.opts)
			if c.setupErr != "" {
				if codeOf(err) != c.setupErr {
					t.Fatalf("setup: %v, want %s", err, c.setupErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("setup: %v", err)
			}
			got, err := io.ReadAll(iotestOneByte(p))
			if c.readErr != "" {
				if codeOf(err) != c.readErr {
					t.Fatalf("read: %v, want %s", err, c.readErr)
				}
				// Sticky.
				if _, err2 := p.Read(make([]byte, 1)); codeOf(err2) != c.readErr {
					t.Fatalf("not sticky: %v", err2)
				}
				return
			}
			if err != nil || !bytes.Equal(got, body[:min(len(body), len(got))]) {
				t.Fatalf("read: %v %q", err, got)
			}
			if p.Size() != int64(len(got)) || p.ETag() != md5Hex(got) || !bytes.Equal(p.MD5(), md5Sum16(got)) {
				t.Fatalf("size/etag %d %s", p.Size(), p.ETag())
			}
			sums := p.Checksums()
			if len(sums) != len(c.sums) {
				t.Fatalf("sums %v, want %v", sums, c.sums)
			}
			for _, a := range c.sums {
				if sums[a] != computeChecksum(a, got) {
					t.Fatalf("%s = %s", a, sums[a])
				}
			}
			if (len(c.sums) > 0) != (p.ChecksumType() == ChecksumTypeFullObject) {
				t.Fatalf("type %q", p.ChecksumType())
			}
			if n, err := p.Read(make([]byte, 4)); n != 0 || err != io.EOF {
				t.Fatalf("after EOF: %d %v", n, err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func md5Sum16(b []byte) []byte { s := md5.Sum(b); return s[:] }

// iotestOneByte reads one byte at a time to exercise boundary handling.
func iotestOneByte(r io.Reader) io.Reader { return &oneByteReader{r} }

type oneByteReader struct{ r io.Reader }

func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

// uploadRecorder is a PutObject override storing what the payload reader saw.
type uploadRecorder struct {
	mu   sync.Mutex
	body []byte
	sums map[string]string
	mode string
	err  error
}

func (u *uploadRecorder) handler(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	p, err := s.newPayloadReader(r, rc, payloadOptions{RequireLength: true})
	if err != nil {
		return err
	}
	data, err := io.ReadAll(p)
	u.mu.Lock()
	u.body, u.sums, u.mode, u.err = data, p.Checksums(), rc.auth.PayloadMode, err
	u.mu.Unlock()
	if err != nil {
		return err
	}
	if ce := r.Header.Get("Content-Encoding"); strings.Contains(ce, "aws-chunked") {
		return s3err.ErrInternalError.WithMessage("aws-chunked not stripped")
	}
	w.Header().Set("ETag", quoteETag(p.ETag()))
	for a, v := range p.Checksums() {
		w.Header().Set(checksumHeader(a), v)
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

func TestSDKUploadChecksums(t *testing.T) {
	data := bytes.Repeat([]byte("gos3-payload-"), 20000)
	sdkAlgos := map[types.ChecksumAlgorithm]string{
		types.ChecksumAlgorithmCrc32:     ChecksumCRC32,
		types.ChecksumAlgorithmCrc32c:    ChecksumCRC32C,
		types.ChecksumAlgorithmCrc64nvme: ChecksumCRC64NVME,
		types.ChecksumAlgorithmSha1:      ChecksumSHA1,
		types.ChecksumAlgorithmSha256:    ChecksumSHA256,
	}
	for _, tls := range []bool{false, true} {
		var opts []testEnvOption
		if tls {
			opts = append(opts, withTLS())
		}
		e := newTestEnv(t, opts...)
		e.createBucket("bkt")
		rec := &uploadRecorder{}
		e.override("PutObject", rec.handler)
		for sdkAlgo, algo := range sdkAlgos {
			name := algo + " tls=" + strconv.FormatBool(tls)
			_, err := e.client.PutObject(context.Background(), &awss3.PutObjectInput{
				Bucket: aws.String("bkt"), Key: aws.String("k"), Body: bytes.NewReader(data), ChecksumAlgorithm: sdkAlgo,
			})
			rec.mu.Lock()
			if err != nil || rec.err != nil {
				t.Errorf("%s: %v / %v", name, err, rec.err)
			} else if !bytes.Equal(rec.body, data) || rec.sums[algo] != computeChecksum(algo, data) {
				t.Errorf("%s: body %d bytes, sums %v", name, len(rec.body), rec.sums)
			} else {
				t.Logf("%s: payload mode %s", name, rec.mode)
			}
			rec.mu.Unlock()
		}
		// A wrong declared checksum is rejected with BadDigest.
		bad := computeChecksum(ChecksumCRC32, []byte("other"))
		_, err := e.client.PutObject(context.Background(), &awss3.PutObjectInput{
			Bucket: aws.String("bkt"), Key: aws.String("k"), Body: bytes.NewReader(data), ChecksumCRC32: aws.String(bad),
		})
		if err == nil || !strings.Contains(err.Error(), "BadDigest") {
			t.Errorf("tls=%v wrong checksum: %v", tls, err)
		}
	}
}
