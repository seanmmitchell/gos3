package s3

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// mpHash returns an independent implementation of algo for expectations.
func mpHash(algo string) hash.Hash {
	switch algo {
	case "CRC32":
		return crc32.NewIEEE()
	case "CRC32C":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "CRC64NVME":
		return crc64.New(crc64.MakeTable(0x9a6c9329ac4bc9b5))
	case "SHA1":
		return sha1.New()
	case "SHA256":
		return sha256.New()
	}
	panic("unknown algorithm " + algo)
}

// mpSum returns the base64 checksum of data.
func mpSum(algo string, data []byte) string {
	h := mpHash(algo)
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// mpComposite returns the expected COMPOSITE checksum of parts.
func mpComposite(algo string, parts ...[]byte) string {
	h := mpHash(algo)
	for _, p := range parts {
		ph := mpHash(algo)
		ph.Write(p)
		h.Write(ph.Sum(nil))
	}
	return fmt.Sprintf("%s-%d", base64.StdEncoding.EncodeToString(h.Sum(nil)), len(parts))
}

// mpField reads the *string field "Checksum<algo>" of an SDK struct.
func mpField(v any, algo string) string {
	rv := reflect.Indirect(reflect.ValueOf(v))
	f := rv.FieldByName("Checksum" + algo)
	if !f.IsValid() || f.IsNil() {
		return ""
	}
	return f.Elem().String()
}

// mpSetField sets the *string field "Checksum<algo>" of an SDK struct.
func mpSetField(v any, algo, val string) {
	reflect.ValueOf(v).Elem().FieldByName("Checksum" + algo).Set(reflect.ValueOf(aws.String(val)))
}

func TestMultipartChecksumModes(t *testing.T) {
	mpSetMinPartSize(t, 1024)
	cases := []struct {
		algo     types.ChecksumAlgorithm
		typ      types.ChecksumType
		wantType types.ChecksumType
	}{
		{types.ChecksumAlgorithmCrc32, "", types.ChecksumTypeComposite},
		{types.ChecksumAlgorithmCrc32, types.ChecksumTypeFullObject, types.ChecksumTypeFullObject},
		{types.ChecksumAlgorithmCrc32c, types.ChecksumTypeComposite, types.ChecksumTypeComposite},
		{types.ChecksumAlgorithmCrc32c, types.ChecksumTypeFullObject, types.ChecksumTypeFullObject},
		{types.ChecksumAlgorithmCrc64nvme, "", types.ChecksumTypeFullObject},
		{types.ChecksumAlgorithmCrc64nvme, types.ChecksumTypeFullObject, types.ChecksumTypeFullObject},
		{types.ChecksumAlgorithmSha1, "", types.ChecksumTypeComposite},
		{types.ChecksumAlgorithmSha256, types.ChecksumTypeComposite, types.ChecksumTypeComposite},
	}
	data := [][]byte{mpData(31, 1500), mpData(32, 1100), mpData(33, 700)}
	all := bytes.Join(data, nil)
	for _, transport := range []string{"http", "https"} {
		var opts []testEnvOption
		if transport == "https" {
			opts = append(opts, withTLS()) // the SDK then sends aws-chunked trailer checksums
		}
		e := newTestEnv(t, opts...)
		e.createBucket("mp")
		c := e.client
		ctx := context.Background()
		for _, tc := range cases {
			algo := string(tc.algo)
			t.Run(fmt.Sprintf("%s/%s/%s", transport, algo, tc.wantType), func(t *testing.T) {
				key := fmt.Sprintf("obj-%s-%s", algo, tc.typ)
				cout, err := c.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
					Bucket: aws.String("mp"), Key: aws.String(key), ChecksumAlgorithm: tc.algo, ChecksumType: tc.typ,
				})
				if err != nil {
					t.Fatal(err)
				}
				if cout.ChecksumAlgorithm != tc.algo || cout.ChecksumType != tc.wantType {
					t.Fatalf("create = %s %s", cout.ChecksumAlgorithm, cout.ChecksumType)
				}
				id := aws.ToString(cout.UploadId)
				var parts []types.CompletedPart
				for i, d := range data {
					out := mpMustUploadPart(t, c, "mp", key, id, int32(i+1), d, func(in *awss3.UploadPartInput) { in.ChecksumAlgorithm = tc.algo })
					if got := mpField(out, algo); got != mpSum(algo, d) {
						t.Fatalf("part %d checksum = %q, want %q", i+1, got, mpSum(algo, d))
					}
					cp := types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.ETag}
					mpSetField(&cp, algo, mpField(out, algo))
					parts = append(parts, cp)
				}

				lp, err := c.ListParts(ctx, &awss3.ListPartsInput{Bucket: aws.String("mp"), Key: aws.String(key), UploadId: aws.String(id)})
				if err != nil {
					t.Fatal(err)
				}
				if lp.ChecksumAlgorithm != tc.algo || lp.ChecksumType != tc.wantType || len(lp.Parts) != 3 {
					t.Fatalf("ListParts = %s %s %d", lp.ChecksumAlgorithm, lp.ChecksumType, len(lp.Parts))
				}
				for i, p := range lp.Parts {
					if mpField(&p, algo) != mpSum(algo, data[i]) {
						t.Fatalf("listed part %d checksum = %q", i+1, mpField(&p, algo))
					}
				}

				want := mpSum(algo, all)
				if tc.wantType == types.ChecksumTypeComposite {
					want = mpComposite(algo, data...)
				}
				out, err := mpComplete(c, "mp", key, id, parts, func(in *awss3.CompleteMultipartUploadInput) {
					in.MpuObjectSize = aws.Int64(int64(len(all)))
					in.ChecksumType = tc.wantType
				})
				if err != nil {
					t.Fatal(err)
				}
				if got := mpField(out, algo); got != want || out.ChecksumType != tc.wantType {
					t.Fatalf("object checksum = %q %s, want %q %s", got, out.ChecksumType, want, tc.wantType)
				}

				// GetObject with checksum mode: the SDK validates FULL_OBJECT
				// checksums against the data (and skips composite ones).
				g, body := mpGet(t, c, "mp", key, func(in *awss3.GetObjectInput) { in.ChecksumMode = types.ChecksumModeEnabled })
				if !bytes.Equal(body, all) || mpField(g, algo) != want || g.ChecksumType != tc.wantType {
					t.Fatalf("GetObject checksum = %q %s", mpField(g, algo), g.ChecksumType)
				}
				h, err := c.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String("mp"), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
				if err != nil || mpField(h, algo) != want {
					t.Fatalf("HeadObject = %v %v", h, err)
				}
				// Each part keeps its own checksum.
				g2, p2 := mpGet(t, c, "mp", key, func(in *awss3.GetObjectInput) {
					in.PartNumber = aws.Int32(2)
					in.ChecksumMode = types.ChecksumModeEnabled
				})
				if !bytes.Equal(p2, data[1]) || mpField(g2, algo) != mpSum(algo, data[1]) {
					t.Fatalf("part 2 checksum = %q", mpField(g2, algo))
				}
			})
		}
	}
}

func TestMultipartChecksumErrors(t *testing.T) {
	mpSetMinPartSize(t, 4)
	e := newTestEnv(t)
	e.createBucket("mp")
	c := e.client
	p1, p2 := []byte("first part"), []byte("second")

	upload := func(key string, algo types.ChecksumAlgorithm, typ types.ChecksumType) (string, []types.CompletedPart) {
		t.Helper()
		id := mpCreate(t, c, "mp", key, func(in *awss3.CreateMultipartUploadInput) {
			in.ChecksumAlgorithm, in.ChecksumType = algo, typ
		})
		var parts []types.CompletedPart
		for i, d := range [][]byte{p1, p2} {
			out := mpMustUploadPart(t, c, "mp", key, id, int32(i+1), d, func(in *awss3.UploadPartInput) { in.ChecksumAlgorithm = algo })
			cp := types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.ETag}
			if algo != "" {
				mpSetField(&cp, string(algo), mpField(out, string(algo)))
			}
			parts = append(parts, cp)
		}
		return id, parts
	}

	t.Run("part algorithm mismatch", func(t *testing.T) {
		id := mpCreate(t, c, "mp", "m", func(in *awss3.CreateMultipartUploadInput) { in.ChecksumAlgorithm = types.ChecksumAlgorithmSha256 })
		_, err := mpUploadPart(c, "mp", "m", id, 1, p1, func(in *awss3.UploadPartInput) { in.ChecksumAlgorithm = types.ChecksumAlgorithmCrc32 })
		mpExpectCode(t, err, "InvalidRequest")
	})

	t.Run("server computes the declared checksum", func(t *testing.T) {
		id := mpRawCreate(t, e, "mp", "raw", http.Header{"X-Amz-Checksum-Algorithm": {"SHA1"}})
		resp := e.signed(http.MethodPut, mpPath("mp", "raw", mpQuery("uploadId", id, "partNumber", "1")), p1, nil)
		if resp.StatusCode != 200 || resp.Header.Get("X-Amz-Checksum-Sha1") != mpSum("SHA1", p1) {
			t.Fatalf("UploadPart = %d %v", resp.StatusCode, resp.Header)
		}
		bad := http.Header{"X-Amz-Checksum-Sha1": {mpSum("SHA1", p2)}}
		resp = e.signed(http.MethodPut, mpPath("mp", "raw", mpQuery("uploadId", id, "partNumber", "1")), p1, bad)
		if resp.code() != "BadDigest" {
			t.Fatalf("wrong part checksum: %d %s", resp.StatusCode, resp.Data)
		}
		// A COMPOSITE completion must list each part's checksum.
		body := []byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"` + md5Hex(p1) + `"</ETag><ChecksumSHA1>` +
			mpSum("SHA1", p1) + `</ChecksumSHA1></Part></CompleteMultipartUpload>`)
		resp = e.signed(http.MethodPost, mpPath("mp", "raw", mpQuery("uploadId", id)), body, nil)
		if resp.StatusCode != 200 || !strings.Contains(string(resp.Data), "<ChecksumSHA1>"+mpComposite("SHA1", p1)+"</ChecksumSHA1><ChecksumType>COMPOSITE</ChecksumType>") {
			t.Fatalf("Complete = %d %s", resp.StatusCode, resp.Data)
		}
	})

	t.Run("composite requires part checksums", func(t *testing.T) {
		id, parts := upload("c1", types.ChecksumAlgorithmSha256, "")
		parts[1].ChecksumSHA256 = nil
		_, err := mpComplete(c, "mp", "c1", id, parts, nil)
		mpExpectCode(t, err, "InvalidRequest")
	})

	t.Run("wrong part checksum", func(t *testing.T) {
		id, parts := upload("c2", types.ChecksumAlgorithmCrc32, "")
		parts[0].ChecksumCRC32 = parts[1].ChecksumCRC32
		_, err := mpComplete(c, "mp", "c2", id, parts, nil)
		mpExpectCode(t, err, "InvalidPart")
	})

	t.Run("part checksum of another algorithm", func(t *testing.T) {
		id, parts := upload("c3", types.ChecksumAlgorithmCrc32, "")
		parts[0].ChecksumSHA1 = aws.String(mpSum("SHA1", p1))
		_, err := mpComplete(c, "mp", "c3", id, parts, nil)
		mpExpectCode(t, err, "InvalidRequest")
	})

	t.Run("composite object checksum header", func(t *testing.T) {
		id, parts := upload("c4", types.ChecksumAlgorithmSha256, "")
		_, err := mpComplete(c, "mp", "c4", id, parts, func(in *awss3.CompleteMultipartUploadInput) {
			in.ChecksumSHA256 = aws.String(mpSum("SHA256", p1))
		})
		mpExpectCode(t, err, "BadDigest")
		want := mpComposite("SHA256", p1, p2)
		out, err := mpComplete(c, "mp", "c4", id, parts, func(in *awss3.CompleteMultipartUploadInput) { in.ChecksumSHA256 = aws.String(want) })
		if err != nil || aws.ToString(out.ChecksumSHA256) != want {
			t.Fatalf("Complete = %v %v", out, err)
		}
	})

	t.Run("full object without part checksums", func(t *testing.T) {
		id, parts := upload("f1", types.ChecksumAlgorithmCrc32c, types.ChecksumTypeFullObject)
		for i := range parts {
			parts[i].ChecksumCRC32C = nil
		}
		full := mpSum("CRC32C", append(append([]byte{}, p1...), p2...))
		_, err := mpComplete(c, "mp", "f1", id, parts, func(in *awss3.CompleteMultipartUploadInput) {
			in.ChecksumCRC32C = aws.String(mpSum("CRC32C", p1))
		})
		mpExpectCode(t, err, "BadDigest")
		_, err = mpComplete(c, "mp", "f1", id, parts, func(in *awss3.CompleteMultipartUploadInput) { in.ChecksumCRC32 = aws.String(mpSum("CRC32", p1)) })
		mpExpectCode(t, err, "InvalidRequest")
		_, err = mpComplete(c, "mp", "f1", id, parts, func(in *awss3.CompleteMultipartUploadInput) { in.ChecksumType = types.ChecksumTypeComposite })
		mpExpectCode(t, err, "InvalidRequest")
		_, err = mpComplete(c, "mp", "f1", id, parts, func(in *awss3.CompleteMultipartUploadInput) { in.MpuObjectSize = aws.Int64(3) })
		mpExpectCode(t, err, "InvalidRequest")
		out, err := mpComplete(c, "mp", "f1", id, parts, func(in *awss3.CompleteMultipartUploadInput) { in.ChecksumCRC32C = aws.String(full) })
		if err != nil || aws.ToString(out.ChecksumCRC32C) != full || out.ChecksumType != types.ChecksumTypeFullObject {
			t.Fatalf("Complete = %+v %v", out, err)
		}
	})

	t.Run("no declared algorithm", func(t *testing.T) {
		id, parts := upload("n1", "", "")
		_, err := mpComplete(c, "mp", "n1", id, parts, func(in *awss3.CompleteMultipartUploadInput) { in.ChecksumCRC32 = aws.String(mpSum("CRC32", p1)) })
		mpExpectCode(t, err, "InvalidRequest")
		_, err = mpComplete(c, "mp", "n1", id, parts, func(in *awss3.CompleteMultipartUploadInput) { in.ChecksumType = types.ChecksumTypeFullObject })
		mpExpectCode(t, err, "InvalidRequest")
		full := mpSum("CRC64NVME", append(append([]byte{}, p1...), p2...))
		out, err := mpComplete(c, "mp", "n1", id, parts, func(in *awss3.CompleteMultipartUploadInput) { in.ChecksumCRC64NVME = aws.String(full) })
		if err != nil || aws.ToString(out.ChecksumCRC64NVME) != full || out.ChecksumType != types.ChecksumTypeFullObject {
			t.Fatalf("Complete = %+v %v", out, err)
		}
	})

	t.Run("malformed complete checksum header", func(t *testing.T) {
		id, parts := upload("x1", types.ChecksumAlgorithmCrc32, types.ChecksumTypeFullObject)
		_, err := mpComplete(c, "mp", "x1", id, parts, func(in *awss3.CompleteMultipartUploadInput) { in.ChecksumCRC32 = aws.String("!!") })
		mpExpectCode(t, err, "InvalidRequest")
		resp := e.signed(http.MethodPost, mpPath("mp", "x1", mpQuery("uploadId", id)),
			mpCompleteBody("1", aws.ToString(parts[0].ETag), "2", aws.ToString(parts[1].ETag)), http.Header{"X-Amz-Mp-Object-Size": {"abc"}})
		if resp.code() != "InvalidArgument" {
			t.Fatalf("bad mp-object-size: %d %s", resp.StatusCode, resp.Data)
		}
	})
}

// TestMultipartCombineUnevenParts cross-checks the FULL_OBJECT combination
// of uneven parts against a direct computation for every CRC algorithm.
func TestMultipartCombineUnevenParts(t *testing.T) {
	for _, algo := range []string{ChecksumCRC32, ChecksumCRC32C, ChecksumCRC64NVME} {
		sizes := []int{1, 0, 4097, 13, 65536}
		var parts []checksumPart
		var all []byte
		for i, n := range sizes {
			d := mpData(int64(i+50), n)
			parts = append(parts, checksumPart{Checksum: mpSum(algo, d), Size: int64(n)})
			all = append(all, d...)
		}
		got, err := combineFullObjectChecksum(algo, parts)
		if err != nil || got != mpSum(algo, all) {
			t.Fatalf("%s: combined %q (%v), want %q", algo, got, err, mpSum(algo, all))
		}
	}
}
