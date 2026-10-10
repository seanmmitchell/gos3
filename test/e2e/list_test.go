package e2e

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// nastyKeys exercise key encoding in paths, listings and XML.
var nastyKeys = []string{
	"plain.txt",
	"with space.txt",
	"plus+sign",
	"percent%20literal",
	"percent%",
	"double//slash",
	"dir/",
	"dir/file",
	"dir/sub/deep.bin",
	"dir/sub/",
	"trailing/",
	"unicode/日本語.txt",
	"unicode/émoji-😀",
	"special&<>\"'chars",
	"question?mark#hash",
	"tilde~star*paren()",
	"semi;colon:comma,eq=at@dollar$",
	"/leading-slash",
	"brackets[]{}^`|\\",
}

// populate stores every key (body = key) in bucket.
func populate(t *testing.T, e *env, bucket string, keys []string) {
	t.Helper()
	for _, k := range keys {
		e.put(t, bucket, k, []byte(k))
	}
}

func sortedCopy(keys []string) []string {
	s := slices.Clone(keys)
	sort.Strings(s) // byte order == UTF-8 binary order, as S3 sorts
	return s
}

func TestNastyKeysRoundTrip(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			b := e.newBucket(t)
			populate(t, e, b, nastyKeys)
			for _, k := range nastyKeys {
				if got := string(e.get(t, b, k)); got != k {
					t.Errorf("GET %q = %q", k, got)
				}
			}
			// Virtual-host client too (fs only has the domain).
			if e == fsEnv {
				vh := e.vhost(rootCreds)
				for _, k := range nastyKeys {
					out, err := vh.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String(k)})
					if err != nil {
						t.Errorf("vhost GET %q: %v", k, err)
						continue
					}
					if got := string(readAll(t, out.Body)); got != k {
						t.Errorf("vhost GET %q = %q", k, got)
					}
				}
			}
		})
	}
}

func TestListObjectsV2Pagination(t *testing.T) {
	e := fsEnv
	b := e.newBucket(t)
	populate(t, e, b, nastyKeys)
	want := sortedCopy(nastyKeys)

	for _, pageSize := range []int32{1, 2, 7, 1000} {
		var got []string
		var token *string
		pages := 0
		for {
			out, err := e.root().ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{
				Bucket: aws.String(b), MaxKeys: aws.Int32(pageSize), ContinuationToken: token,
			})
			must(t, err, "ListObjectsV2")
			pages++
			if int32(len(out.Contents)) > pageSize {
				t.Fatalf("page of %d > max %d", len(out.Contents), pageSize)
			}
			if aws.ToInt32(out.KeyCount) != int32(len(out.Contents)) {
				t.Errorf("KeyCount %d != %d", aws.ToInt32(out.KeyCount), len(out.Contents))
			}
			if aws.ToInt32(out.MaxKeys) != pageSize {
				t.Errorf("MaxKeys = %d", aws.ToInt32(out.MaxKeys))
			}
			for _, o := range out.Contents {
				got = append(got, aws.ToString(o.Key))
				if aws.ToString(o.ETag) == "" || o.LastModified == nil || o.Size == nil {
					t.Errorf("incomplete entry %+v", o)
				}
			}
			if !aws.ToBool(out.IsTruncated) {
				if out.NextContinuationToken != nil {
					t.Errorf("NextContinuationToken on last page")
				}
				break
			}
			if out.NextContinuationToken == nil {
				t.Fatal("truncated without NextContinuationToken")
			}
			token = out.NextContinuationToken
			if pages > 100 {
				t.Fatal("pagination does not terminate")
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("page size %d:\n got %q\nwant %q", pageSize, got, want)
		}
	}

	// StartAfter.
	out, err := e.root().ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b), StartAfter: aws.String("percent%")})
	must(t, err, "ListObjectsV2 StartAfter")
	var got []string
	for _, o := range out.Contents {
		got = append(got, aws.ToString(o.Key))
	}
	var wantAfter []string
	for _, k := range want {
		if k > "percent%" {
			wantAfter = append(wantAfter, k)
		}
	}
	if !reflect.DeepEqual(got, wantAfter) {
		t.Fatalf("StartAfter:\n got %q\nwant %q", got, wantAfter)
	}

	// max-keys=0: empty, not truncated.
	out, err = e.root().ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b), MaxKeys: aws.Int32(0)})
	must(t, err, "ListObjectsV2 max-keys=0")
	if len(out.Contents) != 0 || aws.ToBool(out.IsTruncated) {
		t.Fatalf("max-keys=0: %d entries, truncated=%v", len(out.Contents), aws.ToBool(out.IsTruncated))
	}

	// FetchOwner.
	out, err = e.root().ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b), MaxKeys: aws.Int32(1), FetchOwner: aws.Bool(true)})
	must(t, err, "ListObjectsV2 fetch-owner")
	if out.Contents[0].Owner == nil || aws.ToString(out.Contents[0].Owner.ID) == "" {
		t.Errorf("fetch-owner: no owner")
	}
	out, err = e.root().ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b), MaxKeys: aws.Int32(1)})
	must(t, err, "ListObjectsV2")
	if out.Contents[0].Owner != nil {
		t.Errorf("owner reported without fetch-owner")
	}
}

func TestListObjectsDelimiter(t *testing.T) {
	e := memEnv
	b := e.newBucket(t)
	populate(t, e, b, nastyKeys)

	type listing struct {
		keys, prefixes []string
	}
	listV2 := func(prefix, delim string, max int32) listing {
		var l listing
		var token *string
		for {
			out, err := e.root().ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{
				Bucket: aws.String(b), Prefix: aws.String(prefix), Delimiter: aws.String(delim),
				MaxKeys: aws.Int32(max), ContinuationToken: token,
			})
			must(t, err, "ListObjectsV2")
			if n := len(out.Contents) + len(out.CommonPrefixes); int32(n) > max || aws.ToInt32(out.KeyCount) != int32(n) {
				t.Fatalf("page has %d entries, KeyCount %d, max %d", n, aws.ToInt32(out.KeyCount), max)
			}
			for _, o := range out.Contents {
				l.keys = append(l.keys, aws.ToString(o.Key))
			}
			for _, p := range out.CommonPrefixes {
				l.prefixes = append(l.prefixes, aws.ToString(p.Prefix))
			}
			if !aws.ToBool(out.IsTruncated) {
				return l
			}
			token = out.NextContinuationToken
		}
	}
	listV1 := func(prefix, delim string, max int32) listing {
		var l listing
		var marker *string
		for {
			out, err := e.root().ListObjects(ctx(t), &s3.ListObjectsInput{
				Bucket: aws.String(b), Prefix: aws.String(prefix), Delimiter: aws.String(delim),
				MaxKeys: aws.Int32(max), Marker: marker,
			})
			must(t, err, "ListObjects")
			for _, o := range out.Contents {
				l.keys = append(l.keys, aws.ToString(o.Key))
			}
			for _, p := range out.CommonPrefixes {
				l.prefixes = append(l.prefixes, aws.ToString(p.Prefix))
			}
			if !aws.ToBool(out.IsTruncated) {
				if out.NextMarker != nil {
					t.Errorf("NextMarker on the last page")
				}
				return l
			}
			// AWS returns NextMarker only when a delimiter is given;
			// otherwise clients continue after the last key.
			if delim != "" {
				if out.NextMarker == nil {
					t.Fatal("truncated V1 listing with delimiter has no NextMarker")
				}
				marker = out.NextMarker
			} else {
				marker = out.Contents[len(out.Contents)-1].Key
			}
		}
	}

	cases := []struct {
		prefix, delim string
		want          listing
	}{
		{"", "/", listing{
			keys: []string{"brackets[]{}^`|\\", "percent%", "percent%20literal", "plain.txt", "plus+sign",
				"question?mark#hash", "semi;colon:comma,eq=at@dollar$", "special&<>\"'chars", "tilde~star*paren()", "with space.txt"},
			prefixes: []string{"/", "dir/", "double/", "trailing/", "unicode/"},
		}},
		{"dir/", "/", listing{keys: []string{"dir/", "dir/file"}, prefixes: []string{"dir/sub/"}}},
		{"dir/sub/", "/", listing{keys: []string{"dir/sub/", "dir/sub/deep.bin"}}},
		{"double/", "/", listing{prefixes: []string{"double//"}}},
		{"double//", "/", listing{keys: []string{"double//slash"}}},
		{"", "%", listing{
			keys:     slices.DeleteFunc(sortedCopy(nastyKeys), func(k string) bool { return strings.HasPrefix(k, "percent%") }),
			prefixes: []string{"percent%"},
		}},
		{"unicode/", "", listing{keys: []string{"unicode/émoji-😀", "unicode/日本語.txt"}}},
		{"nomatch", "/", listing{}},
	}
	for _, tc := range cases {
		want := tc.want
		for _, max := range []int32{1, 2, 1000} {
			name := fmt.Sprintf("prefix=%q delim=%q max=%d", tc.prefix, tc.delim, max)
			for ver, got := range map[string]listing{"v2": listV2(tc.prefix, tc.delim, max), "v1": listV1(tc.prefix, tc.delim, max)} {
				if !slices.Equal(got.keys, want.keys) || !slices.Equal(got.prefixes, want.prefixes) {
					t.Errorf("%s %s:\n got keys %q prefixes %q\nwant keys %q prefixes %q",
						ver, name, got.keys, got.prefixes, want.keys, want.prefixes)
				}
			}
		}
	}
}

// listBucketResult is the raw XML of ListObjects V1/V2 with encoding-type=url.
type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Name                  string
	Prefix                string
	Delimiter             string
	Marker                string
	NextMarker            string
	StartAfter            string
	ContinuationToken     string
	NextContinuationToken string
	EncodingType          string
	KeyCount              int
	MaxKeys               int
	IsTruncated           bool
	Contents              []struct{ Key string }
	CommonPrefixes        []struct{ Prefix string }
}

// s3URLEncode is the encoding S3 applies with encoding-type=url: like
// url.QueryEscape but keeping '/' (space becomes '+').
func s3URLEncode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "%2F", "/")
}

func TestListEncodingTypeURL(t *testing.T) {
	e := fsEnv
	b := e.newBucket(t)
	populate(t, e, b, nastyKeys)
	want := sortedCopy(nastyKeys)

	// V2, no delimiter, paged by 3 with every value URL-encoded.
	var got []string
	token := ""
	for page := 0; ; page++ {
		q := url.Values{"list-type": {"2"}, "encoding-type": {"url"}, "max-keys": {"3"}, "start-after": {" "}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp := e.signed(t, rootCreds, http.MethodGet, "/"+b+"?"+q.Encode(), nil, nil)
		if resp.status != http.StatusOK {
			t.Fatalf("list: %d %s", resp.status, resp.body)
		}
		var res listBucketResult
		must(t, xml.Unmarshal(resp.body, &res), "decode")
		if res.EncodingType != "url" {
			t.Fatalf("EncodingType = %q", res.EncodingType)
		}
		if res.StartAfter != "+" {
			t.Errorf("StartAfter = %q, want %q (encoded space)", res.StartAfter, "+")
		}
		for _, c := range res.Contents {
			dec, err := url.QueryUnescape(c.Key)
			must(t, err, "unescape "+c.Key)
			if c.Key != s3URLEncode(dec) {
				t.Errorf("key %q is not encoded as S3 does (%q)", c.Key, s3URLEncode(dec))
			}
			got = append(got, dec)
		}
		if !res.IsTruncated {
			break
		}
		token = res.NextContinuationToken
		if page > 20 {
			t.Fatal("no termination")
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("decoded keys:\n got %q\nwant %q", got, want)
	}

	// V1 with prefix + delimiter: Prefix, Delimiter, Marker, NextMarker and
	// CommonPrefixes are encoded too.
	q := url.Values{"encoding-type": {"url"}, "prefix": {"unicode/"}, "delimiter": {"/"}, "max-keys": {"1"}, "marker": {"unicode/a b"}}
	resp := e.signed(t, rootCreds, http.MethodGet, "/"+b+"?"+q.Encode(), nil, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("v1 list: %d %s", resp.status, resp.body)
	}
	var res listBucketResult
	must(t, xml.Unmarshal(resp.body, &res), "decode v1")
	if res.Prefix != "unicode/" || res.Delimiter != "%2F" && res.Delimiter != "/" || res.Marker != "unicode/a+b" {
		t.Errorf("v1 echo: prefix %q delimiter %q marker %q", res.Prefix, res.Delimiter, res.Marker)
	}
	if len(res.Contents) != 1 || res.Contents[0].Key != s3URLEncode("unicode/émoji-😀") {
		t.Fatalf("v1 contents = %+v", res.Contents)
	}
	if !res.IsTruncated || res.NextMarker != s3URLEncode("unicode/émoji-😀") {
		t.Errorf("v1 truncated %v NextMarker %q", res.IsTruncated, res.NextMarker)
	}

	// Common prefixes with characters needing encoding.
	q = url.Values{"list-type": {"2"}, "encoding-type": {"url"}, "delimiter": {" "}}
	resp = e.signed(t, rootCreds, http.MethodGet, "/"+b+"?"+q.Encode(), nil, nil)
	res = listBucketResult{}
	must(t, xml.Unmarshal(resp.body, &res), "decode cp")
	if len(res.CommonPrefixes) != 1 || res.CommonPrefixes[0].Prefix != "with+" {
		t.Errorf("common prefixes = %+v", res.CommonPrefixes)
	}
	if res.Delimiter != "+" {
		t.Errorf("Delimiter = %q, want +", res.Delimiter)
	}

	// The SDK with EncodingType=url still works end to end (raw values).
	out, err := e.root().ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b), Prefix: aws.String("with"), EncodingType: s3types.EncodingTypeUrl})
	must(t, err, "SDK list encoding-type=url")
	if len(out.Contents) != 1 {
		t.Fatalf("SDK list = %d entries", len(out.Contents))
	}
	if k := aws.ToString(out.Contents[0].Key); k != "with space.txt" && k != "with+space.txt" {
		t.Errorf("SDK key = %q", k)
	}

	// Invalid encoding type.
	resp = e.signed(t, rootCreds, http.MethodGet, "/"+b+"?list-type=2&encoding-type=base64", nil, nil)
	if resp.status != http.StatusBadRequest || resp.code() != "InvalidArgument" {
		t.Errorf("encoding-type=base64: %d %s", resp.status, resp.code())
	}
}

func TestListObjectVersionsPagination(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			b := e.newVersionedBucket(t)
			keys := []string{"a", "a b", "dir/x", "dir/y", "z+"}
			type ver struct{ key, id string }
			var all []ver
			for i := 0; i < 3; i++ {
				for _, k := range keys {
					out := e.put(t, b, k, []byte(fmt.Sprintf("%s-%d", k, i)))
					all = append(all, ver{k, aws.ToString(out.VersionId)})
				}
			}
			// One delete marker on "dir/y".
			dm, err := e.root().DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("dir/y")})
			must(t, err, "DeleteObject")
			if !aws.ToBool(dm.DeleteMarker) || aws.ToString(dm.VersionId) == "" {
				t.Fatalf("delete marker response %+v", dm)
			}
			all = append(all, ver{"dir/y", aws.ToString(dm.VersionId)})

			// Expected order: key ascending, newest version first.
			var want []ver
			for _, k := range keys {
				var vs []ver
				for _, v := range all {
					if v.key == k {
						vs = append(vs, v)
					}
				}
				slices.Reverse(vs)
				want = append(want, vs...)
			}

			for _, page := range []int32{1, 2, 4, 1000} {
				var got []ver
				var km, vm *string
				latest := map[string]int{}
				for n := 0; ; n++ {
					out, err := e.root().ListObjectVersions(ctx(t), &s3.ListObjectVersionsInput{
						Bucket: aws.String(b), MaxKeys: aws.Int32(page), KeyMarker: km, VersionIdMarker: vm,
					})
					must(t, err, "ListObjectVersions")
					// Merge versions and delete markers in listing order.
					type entry struct {
						v      ver
						latest bool
					}
					var es []entry
					for _, v := range out.Versions {
						es = append(es, entry{ver{aws.ToString(v.Key), aws.ToString(v.VersionId)}, aws.ToBool(v.IsLatest)})
					}
					for _, d := range out.DeleteMarkers {
						es = append(es, entry{ver{aws.ToString(d.Key), aws.ToString(d.VersionId)}, aws.ToBool(d.IsLatest)})
					}
					if int32(len(es)) > page {
						t.Fatalf("page has %d entries > %d", len(es), page)
					}
					// Order inside the page follows want (versions and markers are
					// separate XML lists, so sort by position in want).
					pos := func(v ver) int { return slices.Index(want, v) }
					sort.Slice(es, func(i, j int) bool { return pos(es[i].v) < pos(es[j].v) })
					for _, en := range es {
						got = append(got, en.v)
						if en.latest {
							latest[en.v.key]++
						}
					}
					if !aws.ToBool(out.IsTruncated) {
						break
					}
					km, vm = out.NextKeyMarker, out.NextVersionIdMarker
					if km == nil {
						t.Fatal("truncated without NextKeyMarker")
					}
					if n > 100 {
						t.Fatal("no termination")
					}
				}
				if !slices.Equal(got, want) {
					t.Fatalf("page %d:\n got %v\nwant %v", page, got, want)
				}
				for _, k := range keys {
					if latest[k] != 1 {
						t.Errorf("page %d: key %q has %d latest entries", page, k, latest[k])
					}
				}
			}

			// Prefix + delimiter.
			out, err := e.root().ListObjectVersions(ctx(t), &s3.ListObjectVersionsInput{Bucket: aws.String(b), Delimiter: aws.String("/")})
			must(t, err, "ListObjectVersions delimiter")
			if len(out.CommonPrefixes) != 1 || aws.ToString(out.CommonPrefixes[0].Prefix) != "dir/" || len(out.DeleteMarkers) != 0 || len(out.Versions) != 9 {
				t.Fatalf("delimiter listing: %d prefixes, %d versions, %d markers", len(out.CommonPrefixes), len(out.Versions), len(out.DeleteMarkers))
			}

			// version-id-marker without key-marker is invalid.
			resp := e.signed(t, rootCreds, http.MethodGet, "/"+b+"?versions&version-id-marker="+url.QueryEscape(all[0].id), nil, nil)
			if resp.status != http.StatusBadRequest || resp.code() != "InvalidArgument" {
				t.Errorf("version-id-marker alone: %d %s", resp.status, resp.code())
			}

			// encoding-type=url on versions.
			resp = e.signed(t, rootCreds, http.MethodGet, "/"+b+"?versions&encoding-type=url&prefix=a%20", nil, nil)
			var vr struct {
				Prefix       string
				EncodingType string
				Version      []struct{ Key string }
			}
			must(t, xml.Unmarshal(resp.body, &vr), "decode versions")
			if vr.EncodingType != "url" || vr.Prefix != "a+" || len(vr.Version) != 3 || vr.Version[0].Key != "a+b" {
				t.Errorf("versions encoding-type=url: %+v", vr)
			}
		})
	}
}
