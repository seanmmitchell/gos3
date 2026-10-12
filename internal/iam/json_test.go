package iam

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestStringOrSliceUnmarshal(t *testing.T) {
	tests := []struct {
		in      string
		want    StringOrSlice
		wantErr bool
	}{
		{`"s3:GetObject"`, StringOrSlice{"s3:GetObject"}, false},
		{`["a","b"]`, StringOrSlice{"a", "b"}, false},
		{`[]`, StringOrSlice{}, false},
		{`null`, nil, false},
		{`true`, StringOrSlice{"true"}, false},
		{`false`, StringOrSlice{"false"}, false},
		{`12.5`, StringOrSlice{"12.5"}, false},
		{`[1, "x", false]`, StringOrSlice{"1", "x", "false"}, false},
		{`{"a":1}`, nil, true},
		{`[["a"]]`, nil, true},
		{`[null]`, nil, true},
		{`[{"a":"b"}]`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var got StringOrSlice
			err := json.Unmarshal([]byte(tt.in), &got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestStringOrSliceMarshal(t *testing.T) {
	tests := []struct {
		in   StringOrSlice
		want string
	}{
		{StringOrSlice{"a"}, `"a"`},
		{StringOrSlice{"a", "b"}, `["a","b"]`},
		{StringOrSlice{}, `[]`},
		{nil, `[]`},
	}
	for _, tt := range tests {
		b, err := json.Marshal(tt.in)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tt.want {
			t.Errorf("Marshal(%#v) = %s, want %s", tt.in, b, tt.want)
		}
	}
}

func TestPrincipalJSON(t *testing.T) {
	tests := []struct {
		in       string
		want     Principal
		wantErr  bool
		wantJSON string
	}{
		{`"*"`, Principal{Any: true}, false, `"*"`},
		{`{"AWS":"*"}`, Principal{Any: true}, false, `"*"`},
		{`{"AWS":["alice","*"]}`, Principal{Any: true}, false, `"*"`},
		{`{"AWS":"alice"}`, Principal{AWS: StringOrSlice{"alice"}}, false, `{"AWS":"alice"}`},
		{`{"AWS":["alice","arn:aws:iam::123:user/bob"]}`, Principal{AWS: StringOrSlice{"alice", "arn:aws:iam::123:user/bob"}}, false, `{"AWS":["alice","arn:aws:iam::123:user/bob"]}`},
		{`{"CanonicalUser":"abc"}`, Principal{AWS: StringOrSlice{"abc"}}, false, `{"AWS":"abc"}`},
		{`{}`, Principal{}, false, `{}`},
		{`"alice"`, Principal{}, true, ""},
		{`{"Service":"s3.amazonaws.com"}`, Principal{other: map[string]StringOrSlice{"Service": {"s3.amazonaws.com"}}}, false, `{"Service":"s3.amazonaws.com"}`},
		{`{"federated":["cognito-identity.amazonaws.com"],"AWS":"alice"}`, Principal{AWS: StringOrSlice{"alice"}, other: map[string]StringOrSlice{"Federated": {"cognito-identity.amazonaws.com"}}}, false, `{"AWS":"alice","Federated":"cognito-identity.amazonaws.com"}`},
		{`{"Service":"x","AWS":"*"}`, Principal{Any: true}, false, `"*"`},
		{`{"Service":""}`, Principal{}, true, ""},
		{`{"Service":[]}`, Principal{}, true, ""},
		{`{"OIDC":"x"}`, Principal{}, true, ""},
		{`{"AWS":{"x":1}}`, Principal{}, true, ""},
		{`42`, Principal{}, true, ""},
		{`["*"]`, Principal{}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var p Principal
			err := json.Unmarshal([]byte(tt.in), &p)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(p, tt.want) {
				t.Fatalf("got %#v, want %#v", p, tt.want)
			}
			b, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.wantJSON {
				t.Fatalf("marshal = %s, want %s", b, tt.wantJSON)
			}
			// Pointer marshalling uses the same representation.
			b2, _ := json.Marshal(&p)
			if string(b2) != tt.wantJSON {
				t.Fatalf("pointer marshal = %s, want %s", b2, tt.wantJSON)
			}
		})
	}
}

func TestPolicyDocumentRoundTrip(t *testing.T) {
	src := `{
	  "Version": "2012-10-17",
	  "Id": "p1",
	  "Statement": [{
	    "Sid": "A",
	    "Effect": "Allow",
	    "Principal": {"AWS": ["alice", "bob"]},
	    "Action": ["s3:GetObject", "s3:ListBucket"],
	    "Resource": "arn:aws:s3:::b/*",
	    "Condition": {"IpAddress": {"aws:SourceIp": ["10.0.0.0/8", "192.168.1.1"]}, "Bool": {"aws:SecureTransport": true}}
	  }, {
	    "Effect": "Deny",
	    "NotPrincipal": "*",
	    "NotAction": "s3:Get*",
	    "NotResource": ["arn:aws:s3:::b"]
	  }]
	}`
	doc, err := ParseBucketPolicy([]byte(src), "b")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc2, err := ParseBucketPolicy(b, "b")
	if err != nil {
		t.Fatalf("re-parse %s: %v", b, err)
	}
	if !reflect.DeepEqual(doc, doc2) {
		t.Fatalf("round trip changed document:\n%#v\n%#v", doc, doc2)
	}
	if got := doc.Statement[0].Condition["Bool"]["aws:SecureTransport"]; !reflect.DeepEqual(got, StringOrSlice{"true"}) {
		t.Fatalf("bool condition = %#v", got)
	}

	// Lenient decoding (json.Unmarshal) also accepts a single statement object.
	var d PolicyDocument
	if err := json.Unmarshal([]byte(`{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"s3:*","Resource":"*"},"Extra":1}`), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Statement) != 1 || d.Statement[0].Action[0] != "s3:*" {
		t.Fatalf("single statement: %#v", d)
	}
	if err := json.Unmarshal([]byte(`{"Statement":"x"}`), &d); err == nil {
		t.Fatal("expected error for string Statement")
	}
	if err := json.Unmarshal([]byte(`{"Statement":null}`), &d); err != nil || d.Statement != nil {
		t.Fatalf("null statement: %v %#v", err, d)
	}
}

func TestCloneDocumentIsDeep(t *testing.T) {
	doc := PolicyDocument{Statement: []Statement{{
		Effect:    "Allow",
		Principal: &Principal{AWS: StringOrSlice{"a"}},
		Action:    StringOrSlice{"s3:*"},
		Resource:  StringOrSlice{"*"},
		Condition: map[string]map[string]StringOrSlice{"StringEquals": {"s3:prefix": {"x"}}},
	}}}
	c := cloneDocument(doc)
	c.Statement[0].Principal.AWS[0] = "z"
	c.Statement[0].Action[0] = "z"
	c.Statement[0].Condition["StringEquals"]["s3:prefix"][0] = "z"
	if doc.Statement[0].Principal.AWS[0] != "a" || doc.Statement[0].Action[0] != "s3:*" ||
		doc.Statement[0].Condition["StringEquals"]["s3:prefix"][0] != "x" {
		t.Fatal("clone shares memory with original")
	}
}
