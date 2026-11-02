package s3

import (
	"strings"
	"testing"
)

const replicationRole = "arn:aws:iam::000000000000:role/replication"

func replicationDocument(rules ...string) []byte {
	return []byte("<ReplicationConfiguration><Role>" + replicationRole + "</Role>" + strings.Join(rules, "") + "</ReplicationConfiguration>")
}

func TestCompileExecutableReplicationRules(t *testing.T) {
	raw := replicationDocument(
		"<Rule><ID>older</ID><Priority>1</Priority><Status>Enabled</Status><Filter><And><Prefix>images/</Prefix><Tag><Key>class</Key><Value>public</Value></Tag></And></Filter><Destination><Bucket>arn:aws:s3:::backup</Bucket></Destination><DeleteMarkerReplication><Status>Disabled</Status></DeleteMarkerReplication><ExistingObjectReplication><Status>Enabled</Status></ExistingObjectReplication></Rule>",
		"<Rule><ID>newer</ID><Priority>2</Priority><Status>Enabled</Status><Filter><Prefix>images/</Prefix></Filter><Destination><Bucket>arn:aws:s3:::backup</Bucket></Destination><DeleteMarkerReplication><Status>Enabled</Status></DeleteMarkerReplication></Rule>",
	)
	role, rules, err := CompileReplicationRules(raw)
	if err != nil {
		t.Fatal(err)
	}
	if role != replicationRole || len(rules) != 2 || !rules[0].Backfill || !rules[1].DeleteMarkers {
		t.Fatalf("compiled role=%q rules=%+v", role, rules)
	}
	chosen := MatchingReplicationRules(rules, "images/cat.jpg", map[string]string{"class": "public"}, false)
	if len(chosen) != 1 || chosen[0].ID != "newer" {
		t.Fatalf("overlapping priority = %+v", chosen)
	}
	chosen = MatchingReplicationRules(rules, "images/cat.jpg", nil, true)
	if len(chosen) != 1 || chosen[0].ID != "newer" {
		t.Fatalf("delete marker rules = %+v", chosen)
	}
	if rules[0].Matches("images/cat.jpg", map[string]string{"class": "private"}, false) {
		t.Fatal("tag filter matched another value")
	}
	if rules[0].Matches("images/cat.jpg", nil, true) {
		t.Fatal("tag-filtered rule matched a delete marker")
	}
}

func TestUnsupportedReplicationRulesStayInactive(t *testing.T) {
	const destination = "<Destination><Bucket>arn:aws:s3:::backup</Bucket></Destination>"
	for _, tc := range []struct {
		name string
		rule string
	}{
		{"unknown field", "<Rule><Status>Enabled</Status><Foo/>" + destination + "</Rule>"},
		{"replication time", "<Rule><Status>Enabled</Status><Destination><Bucket>arn:aws:s3:::backup</Bucket><ReplicationTime><Status>Enabled</Status><Time><Minutes>15</Minutes></Time></ReplicationTime></Destination></Rule>"},
		{"storage class", "<Rule><Status>Enabled</Status><Destination><Bucket>arn:aws:s3:::backup</Bucket><StorageClass>STANDARD_IA</StorageClass></Destination></Rule>"},
		{"tagged delete marker", "<Rule><Status>Enabled</Status><Filter><Tag><Key>a</Key><Value>b</Value></Tag></Filter>" + destination + "<DeleteMarkerReplication><Status>Enabled</Status></DeleteMarkerReplication></Rule>"},
		{"unsupported source selection", "<Rule><Status>Enabled</Status><SourceSelectionCriteria><ReplicaModifications><Status>Enabled</Status></ReplicaModifications></SourceSelectionCriteria>" + destination + "</Rule>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := CompileReplicationRules(replicationDocument(tc.rule)); err == nil {
				t.Fatal("unsupported rule compiled as executable")
			}
		})
	}
	if _, _, err := CompileReplicationRules([]byte("<ReplicationConfiguration><Role>invalid</Role><Rule><Status>Enabled</Status>" + destination + "</Rule></ReplicationConfiguration>")); err == nil {
		t.Fatal("unbound role ARN compiled as executable")
	}
}
