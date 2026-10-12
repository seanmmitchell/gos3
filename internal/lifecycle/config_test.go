package lifecycle

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

func ptr[T any](v T) *T { return &v }

// doc wraps rule bodies in a LifecycleConfiguration document.
func doc(rules ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	for _, r := range rules {
		b.WriteString("<Rule>" + r + "</Rule>")
	}
	b.WriteString(`</LifecycleConfiguration>`)
	return b.String()
}

func TestParseValid(t *testing.T) {
	tests := []struct {
		name string
		xml  string
		want []Rule
	}{
		{
			name: "aws example expiration days with prefix filter",
			xml: doc(`<ID>id1</ID><Filter><Prefix>logs/</Prefix></Filter><Status>Enabled</Status>
				<Expiration><Days>365</Days></Expiration>`),
			want: []Rule{{ID: "id1", Filter: &Filter{Prefix: ptr("logs/")}, Status: "Enabled",
				Expiration: &Expiration{Days: ptr(365)}}},
		},
		{
			name: "legacy top-level prefix",
			xml:  doc(`<ID>legacy</ID><Prefix>tmp/</Prefix><Status>Disabled</Status><Expiration><Days>1</Days></Expiration>`),
			want: []Rule{{ID: "legacy", Prefix: ptr("tmp/"), Status: "Disabled", Expiration: &Expiration{Days: ptr(1)}}},
		},
		{
			name: "empty filter and date",
			xml:  doc(`<ID>d</ID><Filter/><Status>Enabled</Status><Expiration><Date>2030-01-01T00:00:00.000Z</Date></Expiration>`),
			want: []Rule{{ID: "d", Filter: &Filter{}, Status: "Enabled",
				Expiration: &Expiration{Date: &Date{time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}}}},
		},
		{
			name: "plain date and offset date",
			xml: doc(`<ID>a</ID><Filter></Filter><Status>Enabled</Status><Expiration><Date>2030-01-01</Date></Expiration>`,
				`<ID>b</ID><Filter></Filter><Status>Enabled</Status><Expiration><Date>2030-01-01T02:00:00+02:00</Date></Expiration>`),
			want: []Rule{
				{ID: "a", Filter: &Filter{}, Status: "Enabled", Expiration: &Expiration{Date: &Date{time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}}},
				{ID: "b", Filter: &Filter{}, Status: "Enabled", Expiration: &Expiration{Date: &Date{time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}}},
			},
		},
		{
			name: "tag filter and noncurrent",
			xml: doc(`<ID>t</ID><Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter><Status>Enabled</Status>
				<NoncurrentVersionExpiration><NoncurrentDays>30</NoncurrentDays><NewerNoncurrentVersions>3</NewerNoncurrentVersions></NoncurrentVersionExpiration>`),
			want: []Rule{{ID: "t", Filter: &Filter{Tag: &Tag{Key: "k", Value: "v"}}, Status: "Enabled",
				NoncurrentVersionExpiration: &NoncurrentVersionExpiration{NoncurrentDays: ptr(30), NewerNoncurrentVersions: ptr(3)}}},
		},
		{
			name: "and filter with all predicates",
			xml: doc(`<ID>and</ID><Filter><And><Prefix>p/</Prefix><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>b</Key><Value></Value></Tag>
				<ObjectSizeGreaterThan>10</ObjectSizeGreaterThan><ObjectSizeLessThan>100</ObjectSizeLessThan></And></Filter>
				<Status>Enabled</Status><Expiration><Days>2</Days></Expiration>`),
			want: []Rule{{ID: "and", Filter: &Filter{And: &And{Prefix: ptr("p/"), Tags: []Tag{{"a", "1"}, {"b", ""}},
				ObjectSizeGreaterThan: ptr[int64](10), ObjectSizeLessThan: ptr[int64](100)}},
				Status: "Enabled", Expiration: &Expiration{Days: ptr(2)}}},
		},
		{
			name: "size filters",
			xml: doc(`<ID>gt</ID><Filter><ObjectSizeGreaterThan>0</ObjectSizeGreaterThan></Filter><Status>Enabled</Status><Expiration><Days>2</Days></Expiration>`,
				`<ID>lt</ID><Filter><ObjectSizeLessThan>5</ObjectSizeLessThan></Filter><Status>Enabled</Status><Expiration><Days>2</Days></Expiration>`),
			want: []Rule{
				{ID: "gt", Filter: &Filter{ObjectSizeGreaterThan: ptr[int64](0)}, Status: "Enabled", Expiration: &Expiration{Days: ptr(2)}},
				{ID: "lt", Filter: &Filter{ObjectSizeLessThan: ptr[int64](5)}, Status: "Enabled", Expiration: &Expiration{Days: ptr(2)}},
			},
		},
		{
			name: "expired object delete marker and abort",
			xml: doc(`<ID>m</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status>
				<Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>
				<AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation></AbortIncompleteMultipartUpload>`),
			want: []Rule{{ID: "m", Filter: &Filter{Prefix: ptr("")}, Status: "Enabled",
				Expiration:                     &Expiration{ExpiredObjectDeleteMarker: ptr(true)},
				AbortIncompleteMultipartUpload: &AbortIncompleteMultipartUpload{DaysAfterInitiation: ptr(7)}}},
		},
		{
			name: "transitions stored",
			xml: doc(`<ID>tr</ID><Filter/><Status>Enabled</Status>
				<Transition><Days>30</Days><StorageClass>STANDARD_IA</StorageClass></Transition>
				<Transition><Days>90</Days><StorageClass>GLACIER</StorageClass></Transition>
				<NoncurrentVersionTransition><NoncurrentDays>10</NoncurrentDays><StorageClass>GLACIER</StorageClass></NoncurrentVersionTransition>
				<Expiration><Days>365</Days></Expiration>
				<NoncurrentVersionExpiration><NoncurrentDays>20</NoncurrentDays></NoncurrentVersionExpiration>`),
			want: []Rule{{ID: "tr", Filter: &Filter{}, Status: "Enabled",
				Transitions:                  []Transition{{Days: ptr(30), StorageClass: "STANDARD_IA"}, {Days: ptr(90), StorageClass: "GLACIER"}},
				NoncurrentVersionTransitions: []NoncurrentVersionTransition{{NoncurrentDays: ptr(10), StorageClass: "GLACIER"}},
				Expiration:                   &Expiration{Days: ptr(365)},
				NoncurrentVersionExpiration:  &NoncurrentVersionExpiration{NoncurrentDays: ptr(20)}}},
		},
		{
			name: "no namespace and whitespace",
			xml: `<LifecycleConfiguration>
				<Rule> <ID> spaced </ID> <Filter> <Prefix>x</Prefix> </Filter> <Status>Enabled</Status>
				<Expiration> <Days> 7 </Days> </Expiration> </Rule>
				</LifecycleConfiguration>`,
			want: []Rule{{ID: " spaced ", Filter: &Filter{Prefix: ptr("x")}, Status: "Enabled", Expiration: &Expiration{Days: ptr(7)}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(tc.xml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !reflect.DeepEqual(c.Rules, tc.want) {
				t.Fatalf("rules mismatch\n got %#v\nwant %#v", c.Rules, tc.want)
			}
			// Round trip through Marshal.
			out, err := c.Marshal()
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			c2, err := Parse(out)
			if err != nil {
				t.Fatalf("re-Parse of %s: %v", out, err)
			}
			if !reflect.DeepEqual(c2.Rules, c.Rules) {
				t.Fatalf("round trip mismatch\n got %#v\nwant %#v", c2.Rules, c.Rules)
			}
		})
	}
}

func TestParseGeneratesIDs(t *testing.T) {
	c, err := Parse([]byte(doc(
		`<Filter/><Status>Enabled</Status><Expiration><Days>1</Days></Expiration>`,
		`<ID>given</ID><Filter/><Status>Enabled</Status><Expiration><Days>1</Days></Expiration>`,
		`<ID></ID><Filter/><Status>Enabled</Status><Expiration><Days>1</Days></Expiration>`,
	)))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, r := range c.Rules {
		if r.ID == "" || len(r.ID) > MaxIDLength || ids[r.ID] {
			t.Fatalf("bad generated ID %q in %v", r.ID, ids)
		}
		ids[r.ID] = true
	}
	if c.Rules[1].ID != "given" {
		t.Fatalf("explicit ID changed: %q", c.Rules[1].ID)
	}
}

func TestMarshalCanonical(t *testing.T) {
	c := &Configuration{Rules: []Rule{{
		ID:                             "r1",
		Filter:                         &Filter{And: &And{Prefix: ptr("a/"), Tags: []Tag{{"k", "v"}}}},
		Status:                         StatusEnabled,
		AbortIncompleteMultipartUpload: nil,
		NoncurrentVersionExpiration:    &NoncurrentVersionExpiration{NoncurrentDays: ptr(5)},
		Expiration:                     &Expiration{Date: &Date{time.Date(2031, 2, 3, 0, 0, 0, 0, time.UTC)}},
		Transitions:                    []Transition{{Date: &Date{time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)}, StorageClass: "GLACIER"}},
	}}}
	out, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule><ID>r1</ID>` +
		`<Filter><And><Prefix>a/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></And></Filter>` +
		`<Status>Enabled</Status>` +
		`<Transition><Date>2031-01-01T00:00:00.000Z</Date><StorageClass>GLACIER</StorageClass></Transition>` +
		`<Expiration><Date>2031-02-03T00:00:00.000Z</Date></Expiration>` +
		`<NoncurrentVersionExpiration><NoncurrentDays>5</NoncurrentDays></NoncurrentVersionExpiration>` +
		`</Rule></LifecycleConfiguration>`
	if string(out) != want {
		t.Fatalf("Marshal:\n got %s\nwant %s", out, want)
	}
	if _, err := Parse(out); err != nil {
		t.Fatalf("Parse(Marshal): %v", err)
	}
	// Special characters are escaped.
	c = &Configuration{Rules: []Rule{{ID: `<&">`, Prefix: ptr("a&b"), Status: StatusEnabled,
		Expiration: &Expiration{Days: ptr(1)}}}}
	out, err = c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	c2, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse(%s): %v", out, err)
	}
	if c2.Rules[0].ID != `<&">` || *c2.Rules[0].Prefix != "a&b" {
		t.Fatalf("escaping round trip: %+v", c2.Rules[0])
	}
}

func TestParseErrors(t *testing.T) {
	const okActions = `<Status>Enabled</Status><Expiration><Days>1</Days></Expiration>`
	many := make([]string, MaxRules+1)
	for i := range many {
		many[i] = fmt.Sprintf(`<ID>r%d</ID><Filter/>`, i) + okActions
	}
	maxOK := many[:MaxRules]

	tests := []struct {
		name string
		xml  string
		code string // "" means valid
		msg  string // substring of the message, optional
	}{
		{"empty body", "", "MalformedXML", ""},
		{"whitespace body", "  \n", "MalformedXML", ""},
		{"not xml", "hello", "MalformedXML", ""},
		{"truncated", `<LifecycleConfiguration><Rule>`, "MalformedXML", ""},
		{"wrong root", `<Lifecycle><Rule><Filter/>` + okActions + `</Rule></Lifecycle>`, "MalformedXML", ""},
		{"two roots", doc(`<Filter/>`+okActions) + doc(`<Filter/>`+okActions), "MalformedXML", ""},
		{"trailing text", doc(`<Filter/>`+okActions) + "junk", "MalformedXML", ""},
		{"text in container", `<LifecycleConfiguration>x<Rule><Filter/>` + okActions + `</Rule></LifecycleConfiguration>`, "MalformedXML", ""},
		{"doctype", `<!DOCTYPE x [<!ENTITY a "b">]>` + doc(`<Filter/>`+okActions), "MalformedXML", ""},
		{"no rules", doc(), "MalformedXML", ""},
		{"unknown rule element", doc(`<Filter/><Bogus/>` + okActions), "MalformedXML", ""},
		{"unknown filter element", doc(`<Filter><Suffix>x</Suffix></Filter>` + okActions), "MalformedXML", ""},
		{"element inside leaf", doc(`<ID><b/></ID><Filter/>` + okActions), "MalformedXML", ""},
		{"duplicate expiration", doc(`<Filter/>` + okActions + `<Expiration><Days>2</Days></Expiration>`), "MalformedXML", ""},
		{"duplicate status", doc(`<Filter/><Status>Enabled</Status>` + okActions), "MalformedXML", ""},
		{"too many rules", doc(many...), "InvalidRequest", "1000"},
		{"max rules ok", doc(maxOK...), "", ""},
		{"duplicate ids", doc(`<ID>x</ID><Filter/>`+okActions, `<ID>x</ID><Filter/>`+okActions), "InvalidArgument", "unique"},
		{"id too long", doc(`<ID>` + strings.Repeat("a", 256) + `</ID><Filter/>` + okActions), "InvalidArgument", "255"},
		{"id max length", doc(`<ID>` + strings.Repeat("é", 255) + `</ID><Filter/>` + okActions), "", ""},
		{"missing status", doc(`<Filter/><Expiration><Days>1</Days></Expiration>`), "MalformedXML", ""},
		{"bad status", doc(`<Filter/><Status>enabled</Status><Expiration><Days>1</Days></Expiration>`), "MalformedXML", ""},
		{"no filter and no prefix", doc(okActions), "MalformedXML", ""},
		{"filter and prefix", doc(`<Prefix>a</Prefix><Filter/>` + okActions), "MalformedXML", ""},
		{"filter prefix and tag", doc(`<Filter><Prefix>a</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></Filter>` + okActions), "MalformedXML", ""},
		{"filter prefix and size", doc(`<Filter><Prefix>a</Prefix><ObjectSizeLessThan>4</ObjectSizeLessThan></Filter>` + okActions), "MalformedXML", ""},
		{"filter tag and and", doc(`<Filter><Tag><Key>k</Key><Value>v</Value></Tag><And><Prefix>a</Prefix></And></Filter>` + okActions), "MalformedXML", ""},
		{"empty and", doc(`<Filter><And></And></Filter>` + okActions), "MalformedXML", ""},
		{"and with duplicate prefix", doc(`<Filter><And><Prefix>a</Prefix><Prefix>b</Prefix></And></Filter>` + okActions), "MalformedXML", ""},
		{"and duplicate tag keys", doc(`<Filter><And><Tag><Key>k</Key><Value>1</Value></Tag><Tag><Key>k</Key><Value>2</Value></Tag></And></Filter>` + okActions), "InvalidRequest", "Duplicate"},
		{"tag empty key", doc(`<Filter><Tag><Key></Key><Value>v</Value></Tag></Filter>` + okActions), "InvalidTag", ""},
		{"tag key too long", doc(`<Filter><Tag><Key>` + strings.Repeat("k", 129) + `</Key><Value>v</Value></Tag></Filter>` + okActions), "InvalidTag", ""},
		{"tag value too long", doc(`<Filter><Tag><Key>k</Key><Value>` + strings.Repeat("v", 257) + `</Value></Tag></Filter>` + okActions), "InvalidTag", ""},
		{"and tag key too long", doc(`<Filter><And><Prefix/><Tag><Key>` + strings.Repeat("k", 129) + `</Key><Value>v</Value></Tag></And></Filter>` + okActions), "InvalidTag", ""},
		{"size negative", doc(`<Filter><ObjectSizeGreaterThan>-1</ObjectSizeGreaterThan></Filter>` + okActions), "InvalidArgument", ""},
		{"size lt zero", doc(`<Filter><ObjectSizeLessThan>0</ObjectSizeLessThan></Filter>` + okActions), "InvalidArgument", ""},
		{"size gt >= lt", doc(`<Filter><And><ObjectSizeGreaterThan>10</ObjectSizeGreaterThan><ObjectSizeLessThan>10</ObjectSizeLessThan></And></Filter>` + okActions), "InvalidArgument", ""},
		{"size not a number", doc(`<Filter><ObjectSizeLessThan>big</ObjectSizeLessThan></Filter>` + okActions), "MalformedXML", ""},
		{"size overflow", doc(`<Filter><ObjectSizeLessThan>99999999999999999999</ObjectSizeLessThan></Filter>` + okActions), "MalformedXML", ""},
		{"no actions", doc(`<Filter/><Status>Enabled</Status>`), "InvalidRequest", "At least one action"},
		{"empty expiration", doc(`<Filter/><Status>Enabled</Status><Expiration></Expiration>`), "MalformedXML", ""},
		{"days zero", doc(`<Filter/><Status>Enabled</Status><Expiration><Days>0</Days></Expiration>`), "InvalidArgument", "positive"},
		{"days negative", doc(`<Filter/><Status>Enabled</Status><Expiration><Days>-3</Days></Expiration>`), "InvalidArgument", "positive"},
		{"days not a number", doc(`<Filter/><Status>Enabled</Status><Expiration><Days>one</Days></Expiration>`), "MalformedXML", ""},
		{"days huge", doc(`<Filter/><Status>Enabled</Status><Expiration><Days>3000000000</Days></Expiration>`), "InvalidArgument", ""},
		{"days and date", doc(`<Filter/><Status>Enabled</Status><Expiration><Days>1</Days><Date>2030-01-01T00:00:00Z</Date></Expiration>`), "MalformedXML", ""},
		{"date not midnight", doc(`<Filter/><Status>Enabled</Status><Expiration><Date>2030-01-01T10:00:00Z</Date></Expiration>`), "InvalidArgument", "midnight"},
		{"date midnight in other zone", doc(`<Filter/><Status>Enabled</Status><Expiration><Date>2030-01-01T00:00:00+01:00</Date></Expiration>`), "InvalidArgument", "midnight"},
		{"date fraction", doc(`<Filter/><Status>Enabled</Status><Expiration><Date>2030-01-01T00:00:00.5Z</Date></Expiration>`), "InvalidArgument", "midnight"},
		{"date garbage", doc(`<Filter/><Status>Enabled</Status><Expiration><Date>tomorrow</Date></Expiration>`), "MalformedXML", ""},
		{"date empty", doc(`<Filter/><Status>Enabled</Status><Expiration><Date></Date></Expiration>`), "MalformedXML", ""},
		{"eodm with days", doc(`<Filter/><Status>Enabled</Status><Expiration><Days>1</Days><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>`), "InvalidArgument", "ExpiredObjectDeleteMarker"},
		{"eodm with date", doc(`<Filter/><Status>Enabled</Status><Expiration><Date>2030-01-01</Date><ExpiredObjectDeleteMarker>false</ExpiredObjectDeleteMarker></Expiration>`), "InvalidArgument", "ExpiredObjectDeleteMarker"},
		{"eodm bad bool", doc(`<Filter/><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>1</ExpiredObjectDeleteMarker></Expiration>`), "MalformedXML", ""},
		{"eodm with tag filter", doc(`<Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>`), "InvalidRequest", "Tag"},
		{"eodm false alone ok", doc(`<Filter/><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>false</ExpiredObjectDeleteMarker></Expiration>`), "", ""},
		{"noncurrent days zero", doc(`<Filter/><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>0</NoncurrentDays></NoncurrentVersionExpiration>`), "InvalidArgument", "NoncurrentDays"},
		{"noncurrent days missing", doc(`<Filter/><Status>Enabled</Status><NoncurrentVersionExpiration><NewerNoncurrentVersions>2</NewerNoncurrentVersions></NoncurrentVersionExpiration>`), "InvalidArgument", "NoncurrentDays"},
		{"newer noncurrent zero", doc(`<Filter/><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>0</NewerNoncurrentVersions></NoncurrentVersionExpiration>`), "InvalidArgument", "NewerNoncurrentVersions"},
		{"newer noncurrent too big", doc(`<Filter/><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>101</NewerNoncurrentVersions></NoncurrentVersionExpiration>`), "InvalidArgument", "NewerNoncurrentVersions"},
		{"abort zero", doc(`<Filter/><Status>Enabled</Status><AbortIncompleteMultipartUpload><DaysAfterInitiation>0</DaysAfterInitiation></AbortIncompleteMultipartUpload>`), "InvalidArgument", "DaysAfterInitiation"},
		{"abort missing days", doc(`<Filter/><Status>Enabled</Status><AbortIncompleteMultipartUpload></AbortIncompleteMultipartUpload>`), "InvalidArgument", "DaysAfterInitiation"},
		{"abort with tags", doc(`<Filter><And><Prefix>a</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></And></Filter><Status>Enabled</Status><AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload>`), "InvalidRequest", "Tags"},
		{"transition bad class", doc(`<Filter/><Status>Enabled</Status><Transition><Days>1</Days><StorageClass>STANDARD</StorageClass></Transition>`), "MalformedXML", ""},
		{"transition days and date", doc(`<Filter/><Status>Enabled</Status><Transition><Days>1</Days><Date>2030-01-01</Date><StorageClass>GLACIER</StorageClass></Transition>`), "MalformedXML", ""},
		{"transition ia under 30", doc(`<Filter/><Status>Enabled</Status><Transition><Days>10</Days><StorageClass>STANDARD_IA</StorageClass></Transition>`), "InvalidArgument", "30"},
		{"transition duplicate class", doc(`<Filter/><Status>Enabled</Status><Transition><Days>10</Days><StorageClass>GLACIER</StorageClass></Transition><Transition><Days>20</Days><StorageClass>GLACIER</StorageClass></Transition>`), "InvalidRequest", "StorageClass"},
		{"mixed days and date", doc(`<Filter/><Status>Enabled</Status><Transition><Date>2030-01-01</Date><StorageClass>GLACIER</StorageClass></Transition><Expiration><Days>10</Days></Expiration>`), "InvalidRequest", "mixed"},
		{"expiration before transition", doc(`<Filter/><Status>Enabled</Status><Transition><Days>10</Days><StorageClass>GLACIER</StorageClass></Transition><Expiration><Days>10</Days></Expiration>`), "InvalidArgument", "greater"},
		{"expiration date before transition", doc(`<Filter/><Status>Enabled</Status><Transition><Date>2030-01-02</Date><StorageClass>GLACIER</StorageClass></Transition><Expiration><Date>2030-01-01</Date></Expiration>`), "InvalidArgument", "later"},
		{"nc transition missing days", doc(`<Filter/><Status>Enabled</Status><NoncurrentVersionTransition><StorageClass>GLACIER</StorageClass></NoncurrentVersionTransition>`), "MalformedXML", ""},
		{"nc expiration before nc transition", doc(`<Filter/><Status>Enabled</Status><NoncurrentVersionTransition><NoncurrentDays>10</NoncurrentDays><StorageClass>GLACIER</StorageClass></NoncurrentVersionTransition><NoncurrentVersionExpiration><NoncurrentDays>5</NoncurrentDays></NoncurrentVersionExpiration>`), "InvalidArgument", "greater"},
		{"oversized", doc(`<ID>` + strings.Repeat("a", MaxConfigSize) + `</ID>`), "MaxMessageLengthExceeded", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(tc.xml))
			if tc.code == "" {
				if err != nil {
					t.Fatalf("Parse: unexpected error %v", err)
				}
				if c == nil || len(c.Rules) == 0 {
					t.Fatal("Parse returned no rules")
				}
				return
			}
			if err == nil {
				t.Fatalf("Parse succeeded, want %s", tc.code)
			}
			var se *s3err.Error
			if !errors.As(err, &se) {
				t.Fatalf("error %T %v is not an *s3err.Error", err, err)
			}
			if se.Code != tc.code {
				t.Fatalf("code = %s (%s), want %s", se.Code, se.Message, tc.code)
			}
			if tc.msg != "" && !strings.Contains(se.Message, tc.msg) {
				t.Fatalf("message %q does not contain %q", se.Message, tc.msg)
			}
			if se.HTTPStatus != 400 {
				t.Fatalf("status = %d", se.HTTPStatus)
			}
		})
	}
}

func TestValidateDirect(t *testing.T) {
	// Validate is usable on programmatically built configurations.
	c := &Configuration{Rules: []Rule{{ID: "x", Filter: &Filter{}, Status: StatusEnabled, Expiration: &Expiration{Days: ptr(1)}}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Rules = append(c.Rules, c.Rules[0])
	if err := c.Validate(); !errors.Is(err, s3err.ErrInvalidArgument) {
		t.Fatalf("duplicate IDs: %v", err)
	}
	if err := (&Configuration{}).Validate(); !errors.Is(err, s3err.ErrMalformedXML) {
		t.Fatalf("empty: %v", err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(doc(`<ID>a</ID><Filter><And><Prefix>p</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></And></Filter><Status>Enabled</Status><Expiration><Days>3</Days></Expiration>`)))
	f.Add([]byte(doc(`<Prefix/><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>3</NoncurrentDays></NoncurrentVersionExpiration>`)))
	f.Add([]byte(`<LifecycleConfiguration><Rule></Rule></LifecycleConfiguration>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Parse(data)
		if err != nil {
			var se *s3err.Error
			if !errors.As(err, &se) {
				t.Fatalf("non-S3 error %T: %v", err, err)
			}
			return
		}
		out, err := c.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		c2, err := Parse(out)
		if err != nil {
			t.Fatalf("re-Parse(%s) of valid config: %v", out, err)
		}
		if !reflect.DeepEqual(c.Rules, c2.Rules) {
			t.Fatalf("round trip mismatch for %q", data)
		}
	})
}
