package s3

import (
	"encoding/xml"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestValidBucketName(t *testing.T) {
	good := []string{"abc", "my-bucket", "my.bucket.name", "a1b2c3", strings.Repeat("a", 63), "1.2.3", "bucket-1.2.3.4"}
	bad := []string{"", "ab", strings.Repeat("a", 64), "MyBucket", "my_bucket", "-abc", "abc-", ".abc", "abc.",
		"a..b", "a.-b", "a-.b", "192.168.1.1", "xn--abc", "abc-s3alias", "abc--ol-s3", "sthree-x", "abc.mrap",
		"abc--x-s3", "amzn-s3-demo-bucket", "ab c", "ünï", "_gos3"}
	for _, n := range good {
		if !validBucketName(n) || validateBucketName(n) != nil {
			t.Errorf("%q should be valid", n)
		}
	}
	for _, n := range bad {
		if validBucketName(n) {
			t.Errorf("%q should be invalid", n)
		}
		if codeOf(validateBucketName(n)) != "InvalidBucketName" {
			t.Errorf("%q error code", n)
		}
	}
}

func TestValidateObjectKey(t *testing.T) {
	if err := validateObjectKey(strings.Repeat("k", 1024)); err != nil {
		t.Fatal(err)
	}
	if codeOf(validateObjectKey(strings.Repeat("k", 1025))) != "KeyTooLongError" {
		t.Fatal("long key")
	}
	if codeOf(validateObjectKey("a\xffb")) != "InvalidArgument" || codeOf(validateObjectKey("")) != "InvalidArgument" {
		t.Fatal("invalid key")
	}
	if err := validateObjectKey(strings.Repeat("日", 341)); err != nil { // 1023 bytes
		t.Fatal(err)
	}
}

func TestStorageClassAndACL(t *testing.T) {
	if v, err := normalizeStorageClass("GLACIER_IR"); err != nil || v != "GLACIER_IR" {
		t.Fatal(v, err)
	}
	if _, err := normalizeStorageClass("glacier"); codeOf(err) != "InvalidStorageClass" {
		t.Fatal("lowercase class accepted")
	}
	if storageClassOrStandard("") != "STANDARD" || storageClassOrStandard("GLACIER") != "GLACIER" {
		t.Fatal("storageClassOrStandard")
	}
	for _, acl := range []string{"public-read", "public-read-write", "authenticated-read", "aws-exec-read", "bucket-owner-read", "bucket-owner-full-control"} {
		if v, err := validateCannedACL(acl, false); err != nil || v != acl {
			t.Errorf("%s: %v", acl, err)
		}
	}
	if v, err := validateCannedACL("log-delivery-write", true); err != nil || v != "log-delivery-write" {
		t.Fatal(v, err)
	}
	if _, err := validateCannedACL("log-delivery-write", false); err == nil {
		t.Fatal("log-delivery-write on object")
	}
	if _, err := validateCannedACL("everyone", true); codeOf(err) != "InvalidArgument" {
		t.Fatal("bad acl")
	}
}

func TestParseMaxKeysAndPartNumber(t *testing.T) {
	q := url.Values{"max-keys": {"5"}, "big": {"5000"}, "neg": {"-1"}, "nan": {"x"}, "zero": {"0"}}
	for name, want := range map[string]int{"max-keys": 5, "big": 1000, "zero": 0, "absent": 1000} {
		if got, err := parseMaxKeys(q, name, 1000, 1000); err != nil || got != want {
			t.Errorf("%s: %d %v", name, got, err)
		}
	}
	for _, name := range []string{"neg", "nan"} {
		if _, err := parseMaxKeys(q, name, 1000, 1000); codeOf(err) != "InvalidArgument" {
			t.Errorf("%s accepted", name)
		}
	}
	if n, err := parsePartNumber("10000"); err != nil || n != 10000 {
		t.Fatal(n, err)
	}
	for _, v := range []string{"0", "10001", "", "x", "-1"} {
		if _, err := parsePartNumber(v); codeOf(err) != "InvalidArgument" {
			t.Errorf("part %q accepted", v)
		}
	}
}

func TestTags(t *testing.T) {
	m, err := validateObjectTags([]Tag{{"a", "1"}, {"b c", ""}, {"ü:/=+-@._", "x y"}})
	if err != nil || len(m) != 3 {
		t.Fatal(m, err)
	}
	for name, tags := range map[string][]Tag{
		"dup":        {{"a", "1"}, {"a", "2"}},
		"empty key":  {{"", "1"}},
		"long key":   {{strings.Repeat("k", 129), ""}},
		"long value": {{"k", strings.Repeat("v", 257)}},
		"aws prefix": {{"AWS:x", "1"}},
		"bad char":   {{"a<b", "1"}},
		"bad value":  {{"a", "1;2"}},
		"too many":   make([]Tag, 11),
	} {
		if name == "too many" {
			for i := range tags {
				tags[i] = Tag{Key: string(rune('a' + i)), Value: "v"}
			}
		}
		if _, err := validateObjectTags(tags); codeOf(err) != "InvalidTag" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := validateObjectTags([]Tag{{strings.Repeat("ü", 128), strings.Repeat("ü", 256)}}); err != nil {
		t.Fatalf("unicode lengths count characters: %v", err)
	}
	many := make([]Tag, 50)
	for i := range many {
		many[i] = Tag{Key: "k" + strings.Repeat("x", i), Value: "v"}
	}
	if _, err := validateBucketTags(many); err != nil {
		t.Fatal(err)
	}
	if _, err := validateBucketTags(append(many, Tag{"z", "1"})); codeOf(err) != "InvalidTag" {
		t.Fatal("51 bucket tags")
	}
	tg := taggingFromMap(map[string]string{"b": "2", "a": "1"})
	out, _ := xml.Marshal(tg)
	if string(out) != `<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>b</Key><Value>2</Value></Tag></TagSet></Tagging>` {
		t.Fatalf("%s", out)
	}
	var back Tagging
	if err := decodeXML(out, &back); err != nil || len(back.TagSet.Tags) != 2 {
		t.Fatal(back, err)
	}
	if m, err := parseTaggingHeader(""); err != nil || len(m) != 0 {
		t.Fatal(m, err)
	}
}

func TestXMLHelpers(t *testing.T) {
	if s3URLEncode("a b/c+d%é~_.-") != "a+b/c%2Bd%25%C3%A9~_.-" {
		t.Fatal(s3URLEncode("a b/c+d%é~_.-"))
	}
	if maybeURLEncode("a b", false) != "a b" {
		t.Fatal("maybeURLEncode")
	}
	for v, want := range map[string]bool{"": false, "url": true, "URL": true} {
		if got, err := parseEncodingType(url.Values{"encoding-type": {v}}); err != nil || got != want {
			t.Errorf("%q: %v %v", v, got, err)
		}
	}
	if _, err := parseEncodingType(url.Values{"encoding-type": {"base64"}}); codeOf(err) != "InvalidArgument" {
		t.Fatal("bad encoding type")
	}
	ts := time.Date(2024, 1, 2, 3, 4, 5, 678e6, time.FixedZone("x", 3600))
	if formatXMLTime(ts) != "2024-01-02T02:04:05.678Z" || formatHTTPTime(ts) != "Tue, 02 Jan 2024 02:04:05 GMT" {
		t.Fatal(formatXMLTime(ts), formatHTTPTime(ts))
	}
	if tm, ok := parseHTTPTime("Tue, 02 Jan 2024 02:04:05 GMT"); !ok || tm.Hour() != 2 {
		t.Fatal(tm)
	}
	for _, v := range []string{"2030-01-02T03:04:05Z", "2030-01-02T03:04:05.123Z", "2030-01-02T04:04:05+01:00", "20300102T030405Z"} {
		if tm, ok := parseISOTime(v); !ok || tm.Year() != 2030 || tm.Hour() != 3 {
			t.Errorf("%q: %v %v", v, tm, ok)
		}
	}
	if _, ok := parseISOTime("nope"); ok {
		t.Fatal("garbage time")
	}
	o := ownerFor("")
	if o.DisplayName != "root" || len(o.ID) != 64 || ownerFor("root") != o || ownerFor("alice").ID == o.ID {
		t.Fatalf("%+v", o)
	}
	if err := decodeXML([]byte("  "), &Tagging{}); codeOf(err) != "MissingRequestBodyError" {
		t.Fatal(err)
	}
	if err := decodeXMLBody(strings.NewReader(strings.Repeat("x", 11)), 10, &Tagging{}); codeOf(err) != "MaxMessageLengthExceeded" {
		t.Fatal(err)
	}
	if err := decodeXMLBody(strings.NewReader("<Other/>"), 0, &Tagging{}); codeOf(err) != "MalformedXML" {
		t.Fatal(err)
	}
}

func TestACLHelpers(t *testing.T) {
	owner := ownerFor("alice")
	p := aclPolicyForCanned("public-read-write", owner)
	out, err := xml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`<AccessControlPolicy xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>` + owner.ID,
		`<Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser"><ID>`,
		`<Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group"><URI>http://acs.amazonaws.com/groups/global/AllUsers</URI></Grantee><Permission>WRITE</Permission>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	var back AccessControlPolicy
	if err := decodeXML(out, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Grants) != 3 || back.Grants[1].Grantee.Type != "Group" || back.Owner.DisplayName != "alice" {
		t.Fatalf("%+v", back)
	}
	for canned, want := range map[string]string{"private": "", "public-read": "public-read", "public-read-write": "public-read-write",
		"authenticated-read": "authenticated-read", "log-delivery-write": "", "bucket-owner-full-control": ""} {
		p := aclPolicyForCanned(canned, owner)
		if got := cannedFromACLPolicy(&p); got != want {
			t.Errorf("%s => %q, want %q", canned, got, want)
		}
	}
	for raw, want := range map[string]string{"": "", "private": "", " Public-Read\n": "public-read", "<bad": "",
		string(out): "public-read-write"} {
		if got := cannedACLFromConfig([]byte(raw)); got != want {
			t.Errorf("%q => %q, want %q", raw, got, want)
		}
	}
	if !aclsDisabledByOwnership([]byte(`<OwnershipControls><Rule><ObjectOwnership>BucketOwnerEnforced</ObjectOwnership></Rule></OwnershipControls>`)) ||
		aclsDisabledByOwnership([]byte(`<OwnershipControls><Rule><ObjectOwnership>ObjectWriter</ObjectOwnership></Rule></OwnershipControls>`)) ||
		aclsDisabledByOwnership(nil) {
		t.Fatal("ownership")
	}
	if pab, err := parsePublicAccessBlock([]byte(`<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls></PublicAccessBlockConfiguration>`)); err != nil || !pab.BlockPublicAcls || pab.IgnorePublicAcls {
		t.Fatal(pab, err)
	}
	if effectiveACL("Public-Read", PublicAccessBlockConfiguration{IgnorePublicAcls: true}) != "" || effectiveACL("public-read", PublicAccessBlockConfiguration{}) != "public-read" {
		t.Fatal("effectiveACL")
	}
}
