package s3

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// --- Website ---

func TestBucketWebsiteSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	_, err := c.GetBucketWebsite(bcCtx, &awss3.GetBucketWebsiteInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchWebsiteConfiguration")
	in := &types.WebsiteConfiguration{
		IndexDocument: &types.IndexDocument{Suffix: aws.String("index.html")},
		ErrorDocument: &types.ErrorDocument{Key: aws.String("errors/404.html")},
		RoutingRules: []types.RoutingRule{
			{Condition: &types.Condition{KeyPrefixEquals: aws.String("docs/")},
				Redirect: &types.Redirect{ReplaceKeyPrefixWith: aws.String("documents/"), HttpRedirectCode: aws.String("301")}},
			{Condition: &types.Condition{HttpErrorCodeReturnedEquals: aws.String("404")},
				Redirect: &types.Redirect{HostName: aws.String("example.com"), Protocol: types.ProtocolHttps}},
		},
	}
	if _, err := c.PutBucketWebsite(bcCtx, &awss3.PutBucketWebsiteInput{Bucket: aws.String("cfg"), WebsiteConfiguration: in}); err != nil {
		t.Fatal(err)
	}
	out, err := c.GetBucketWebsite(bcCtx, &awss3.GetBucketWebsiteInput{Bucket: aws.String("cfg")})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.IndexDocument.Suffix) != "index.html" || aws.ToString(out.ErrorDocument.Key) != "errors/404.html" ||
		len(out.RoutingRules) != 2 || aws.ToString(out.RoutingRules[0].Redirect.ReplaceKeyPrefixWith) != "documents/" ||
		aws.ToString(out.RoutingRules[0].Redirect.HttpRedirectCode) != "301" ||
		aws.ToString(out.RoutingRules[1].Condition.HttpErrorCodeReturnedEquals) != "404" ||
		out.RoutingRules[1].Redirect.Protocol != types.ProtocolHttps || out.RedirectAllRequestsTo != nil {
		t.Fatalf("website: %+v", out)
	}
	bcNS(t, e.signed(http.MethodGet, "/cfg?website", nil, nil), "WebsiteConfiguration")

	// Redirect-all replaces the configuration.
	if _, err := c.PutBucketWebsite(bcCtx, &awss3.PutBucketWebsiteInput{Bucket: aws.String("cfg"), WebsiteConfiguration: &types.WebsiteConfiguration{
		RedirectAllRequestsTo: &types.RedirectAllRequestsTo{HostName: aws.String("www.example.com"), Protocol: types.ProtocolHttp}}}); err != nil {
		t.Fatal(err)
	}
	out, err = c.GetBucketWebsite(bcCtx, &awss3.GetBucketWebsiteInput{Bucket: aws.String("cfg")})
	if err != nil || aws.ToString(out.RedirectAllRequestsTo.HostName) != "www.example.com" || out.IndexDocument != nil {
		t.Fatalf("redirect all: %v %+v", err, out)
	}
	if _, err := c.DeleteBucketWebsite(bcCtx, &awss3.DeleteBucketWebsiteInput{Bucket: aws.String("cfg")}); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetBucketWebsite(bcCtx, &awss3.GetBucketWebsiteInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchWebsiteConfiguration")
}

func TestBucketWebsiteValidation(t *testing.T) {
	e := bcEnv(t)
	doc := func(inner string) []byte { return []byte("<WebsiteConfiguration>" + inner + "</WebsiteConfiguration>") }
	idx := "<IndexDocument><Suffix>index.html</Suffix></IndexDocument>"
	rules := func(rs ...string) string { return "<RoutingRules>" + strings.Join(rs, "") + "</RoutingRules>" }
	rr := func(cond, redirect string) string {
		s := "<RoutingRule>"
		if cond != "" {
			s += "<Condition>" + cond + "</Condition>"
		}
		if redirect != "-" {
			s += "<Redirect>" + redirect + "</Redirect>"
		}
		return s + "</RoutingRule>"
	}
	var fiftyOne []string
	for i := 0; i < 51; i++ {
		fiftyOne = append(fiftyOne, rr("", "<HostName>x</HostName>"))
	}
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		code   string
	}{
		{"index", doc(idx), 200, ""},
		{"empty", doc(""), 400, "InvalidArgument"},
		{"error only", doc("<ErrorDocument><Key>e.html</Key></ErrorDocument>"), 400, "InvalidArgument"},
		{"empty suffix", doc("<IndexDocument><Suffix></Suffix></IndexDocument>"), 400, "InvalidArgument"},
		{"slash suffix", doc("<IndexDocument><Suffix>a/index.html</Suffix></IndexDocument>"), 400, "InvalidArgument"},
		{"empty error key", doc(idx + "<ErrorDocument><Key></Key></ErrorDocument>"), 400, "InvalidArgument"},
		{"redirect all with index", doc(idx + "<RedirectAllRequestsTo><HostName>x</HostName></RedirectAllRequestsTo>"), 400, "InvalidRequest"},
		{"redirect all no host", doc("<RedirectAllRequestsTo><Protocol>https</Protocol></RedirectAllRequestsTo>"), 400, "InvalidArgument"},
		{"redirect all bad protocol", doc("<RedirectAllRequestsTo><HostName>x</HostName><Protocol>ftp</Protocol></RedirectAllRequestsTo>"), 400, "InvalidArgument"},
		{"rule ok", doc(idx + rules(rr("<KeyPrefixEquals>a/</KeyPrefixEquals>", "<ReplaceKeyWith>b.html</ReplaceKeyWith>"))), 200, ""},
		{"rule no redirect", doc(idx + rules(rr("<KeyPrefixEquals>a/</KeyPrefixEquals>", "-"))), 400, "MalformedXML"},
		{"rule empty redirect", doc(idx + rules(rr("", ""))), 400, "InvalidArgument"},
		{"rule empty condition", doc(idx + rules("<RoutingRule><Condition></Condition><Redirect><HostName>x</HostName></Redirect></RoutingRule>")), 400, "InvalidArgument"},
		{"rule both replace", doc(idx + rules(rr("", "<ReplaceKeyPrefixWith>a</ReplaceKeyPrefixWith><ReplaceKeyWith>b</ReplaceKeyWith>"))), 400, "InvalidArgument"},
		{"rule bad code", doc(idx + rules(rr("", "<HttpRedirectCode>200</HttpRedirectCode>"))), 400, "InvalidArgument"},
		{"rule bad error code", doc(idx + rules(rr("<HttpErrorCodeReturnedEquals>200</HttpErrorCodeReturnedEquals>", "<HostName>x</HostName>"))), 400, "InvalidArgument"},
		{"rule bad protocol", doc(idx + rules(rr("", "<Protocol>gopher</Protocol>"))), 400, "InvalidArgument"},
		{"no rules", doc(idx + rules()), 400, "MalformedXML"},
		{"51 rules", doc(idx + rules(fiftyOne...)), 400, "InvalidArgument"},
		{"50 rules", doc(idx + rules(fiftyOne[:50]...)), 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, "/cfg?website", tc.body, nil), tc.status, tc.code)
		})
	}
}

// --- Notification ---

func TestBucketNotificationSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	out, err := c.GetBucketNotificationConfiguration(bcCtx, &awss3.GetBucketNotificationConfigurationInput{Bucket: aws.String("cfg")})
	if err != nil || len(out.QueueConfigurations)+len(out.TopicConfigurations)+len(out.LambdaFunctionConfigurations) != 0 || out.EventBridgeConfiguration != nil {
		t.Fatalf("empty notification: %v %+v", err, out)
	}
	resp := e.signed(http.MethodGet, "/cfg?notification", nil, nil)
	bcExpect(t, resp, 200, "")
	bcNS(t, resp, "NotificationConfiguration")

	in := &types.NotificationConfiguration{
		QueueConfigurations: []types.QueueConfiguration{{Id: aws.String("q1"), QueueArn: aws.String("arn:aws:sqs:us-east-1:000000000000:q"),
			Events: []types.Event{"s3:ObjectCreated:*"},
			Filter: &types.NotificationConfigurationFilter{Key: &types.S3KeyFilter{FilterRules: []types.FilterRule{
				{Name: types.FilterRuleNamePrefix, Value: aws.String("images/")}, {Name: types.FilterRuleNameSuffix, Value: aws.String(".jpg")}}}}}},
		TopicConfigurations: []types.TopicConfiguration{{TopicArn: aws.String("arn:aws:sns:us-east-1:000000000000:t"),
			Events: []types.Event{"s3:ObjectRemoved:Delete", "s3:ObjectRemoved:DeleteMarkerCreated"}}},
		LambdaFunctionConfigurations: []types.LambdaFunctionConfiguration{{LambdaFunctionArn: aws.String("arn:aws:lambda:us-east-1:000000000000:function:f"),
			Events: []types.Event{"s3:ObjectTagging:*"}}},
		EventBridgeConfiguration: &types.EventBridgeConfiguration{},
	}
	if _, err := c.PutBucketNotificationConfiguration(bcCtx, &awss3.PutBucketNotificationConfigurationInput{Bucket: aws.String("cfg"), NotificationConfiguration: in}); err != nil {
		t.Fatal(err)
	}
	out, err = c.GetBucketNotificationConfiguration(bcCtx, &awss3.GetBucketNotificationConfigurationInput{Bucket: aws.String("cfg")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.QueueConfigurations) != 1 || aws.ToString(out.QueueConfigurations[0].QueueArn) != "arn:aws:sqs:us-east-1:000000000000:q" ||
		len(out.QueueConfigurations[0].Filter.Key.FilterRules) != 2 || aws.ToString(out.QueueConfigurations[0].Id) != "q1" ||
		len(out.TopicConfigurations) != 1 || len(out.TopicConfigurations[0].Events) != 2 ||
		len(out.LambdaFunctionConfigurations) != 1 || aws.ToString(out.LambdaFunctionConfigurations[0].LambdaFunctionArn) == "" ||
		out.EventBridgeConfiguration == nil {
		t.Fatalf("notification: %+v", out)
	}
	// An empty configuration removes it.
	if _, err := c.PutBucketNotificationConfiguration(bcCtx, &awss3.PutBucketNotificationConfigurationInput{Bucket: aws.String("cfg"),
		NotificationConfiguration: &types.NotificationConfiguration{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.backend.GetBucketConfig(bcCtx, "cfg", storage.ConfigNotification); err == nil {
		t.Fatal("empty notification configuration should be removed")
	}
}

func TestBucketNotificationValidation(t *testing.T) {
	e := bcEnv(t)
	doc := func(inner string) []byte {
		return []byte("<NotificationConfiguration>" + inner + "</NotificationConfiguration>")
	}
	queue := func(inner string) string { return "<QueueConfiguration>" + inner + "</QueueConfiguration>" }
	const arn = "<Queue>arn:aws:sqs:us-east-1:0:q</Queue>"
	filter := func(rules ...string) string {
		s := "<Filter><S3Key>"
		for i := 0; i+1 < len(rules); i += 2 {
			s += "<FilterRule><Name>" + rules[i] + "</Name><Value>" + rules[i+1] + "</Value></FilterRule>"
		}
		return s + "</S3Key></Filter>"
	}
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		code   string
	}{
		{"ok", doc(queue(arn + "<Event>s3:ObjectCreated:Put</Event>")), 200, ""},
		{"no events", doc(queue(arn)), 400, "InvalidArgument"},
		{"bad event", doc(queue(arn + "<Event>s3:ObjectEaten:*</Event>")), 400, "InvalidArgument"},
		{"no arn", doc(queue("<Event>s3:ObjectCreated:*</Event>")), 400, "InvalidArgument"},
		{"wrong arn kind", doc(queue("<Topic>arn:aws:sns:x</Topic><Event>s3:ObjectCreated:*</Event>")), 400, "InvalidArgument"},
		{"dup ids", doc(queue("<Id>a</Id>"+arn+"<Event>s3:ObjectCreated:*</Event>") + queue("<Id>a</Id>"+arn+"<Event>s3:ObjectCreated:*</Event>")), 400, "InvalidArgument"},
		{"filter ok", doc(queue(arn + "<Event>s3:ObjectCreated:*</Event>" + filter("Prefix", "a/", "suffix", ".txt"))), 200, ""},
		{"filter bad name", doc(queue(arn + "<Event>s3:ObjectCreated:*</Event>" + filter("infix", "a"))), 400, "InvalidArgument"},
		{"filter dup", doc(queue(arn + "<Event>s3:ObjectCreated:*</Event>" + filter("prefix", "a", "prefix", "b"))), 400, "InvalidArgument"},
		{"empty", doc(""), 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, "/cfg?notification", tc.body, nil), tc.status, tc.code)
		})
	}
}

// --- Logging ---

func TestBucketLoggingSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	out, err := c.GetBucketLogging(bcCtx, &awss3.GetBucketLoggingInput{Bucket: aws.String("cfg")})
	if err != nil || out.LoggingEnabled != nil {
		t.Fatalf("empty logging: %v %+v", err, out)
	}
	bcNS(t, e.signed(http.MethodGet, "/cfg?logging", nil, nil), "BucketLoggingStatus")

	put := func(le *types.LoggingEnabled) error {
		_, err := c.PutBucketLogging(bcCtx, &awss3.PutBucketLoggingInput{Bucket: aws.String("cfg"), BucketLoggingStatus: &types.BucketLoggingStatus{LoggingEnabled: le}})
		return err
	}
	bcSDKErr(t, put(&types.LoggingEnabled{TargetBucket: aws.String("logs"), TargetPrefix: aws.String("cfg/")}), "InvalidTargetBucketForLogging")
	e.createBucket("logs")
	if err := put(&types.LoggingEnabled{TargetBucket: aws.String("logs"), TargetPrefix: aws.String("cfg/"),
		TargetGrants:          []types.TargetGrant{{Grantee: &types.Grantee{Type: types.TypeGroup, URI: aws.String(groupAllUsers)}, Permission: types.BucketLogsPermissionRead}},
		TargetObjectKeyFormat: &types.TargetObjectKeyFormat{PartitionedPrefix: &types.PartitionedPrefix{PartitionDateSource: types.PartitionDateSourceEventTime}},
	}); err != nil {
		t.Fatal(err)
	}
	out, err = c.GetBucketLogging(bcCtx, &awss3.GetBucketLoggingInput{Bucket: aws.String("cfg")})
	if err != nil {
		t.Fatal(err)
	}
	le := out.LoggingEnabled
	if le == nil || aws.ToString(le.TargetBucket) != "logs" || aws.ToString(le.TargetPrefix) != "cfg/" || len(le.TargetGrants) != 1 ||
		aws.ToString(le.TargetGrants[0].Grantee.URI) != groupAllUsers || le.TargetGrants[0].Grantee.Type != types.TypeGroup ||
		le.TargetObjectKeyFormat == nil || le.TargetObjectKeyFormat.PartitionedPrefix.PartitionDateSource != types.PartitionDateSourceEventTime {
		t.Fatalf("logging: %+v", le)
	}
	// Simple prefix, then disable.
	if err := put(&types.LoggingEnabled{TargetBucket: aws.String("logs"), TargetPrefix: aws.String(""),
		TargetObjectKeyFormat: &types.TargetObjectKeyFormat{SimplePrefix: &types.SimplePrefix{}}}); err != nil {
		t.Fatal(err)
	}
	out, _ = c.GetBucketLogging(bcCtx, &awss3.GetBucketLoggingInput{Bucket: aws.String("cfg")})
	if out.LoggingEnabled == nil || out.LoggingEnabled.TargetObjectKeyFormat.SimplePrefix == nil {
		t.Fatalf("simple prefix: %+v", out.LoggingEnabled)
	}
	if err := put(nil); err != nil {
		t.Fatal(err)
	}
	out, _ = c.GetBucketLogging(bcCtx, &awss3.GetBucketLoggingInput{Bucket: aws.String("cfg")})
	if out.LoggingEnabled != nil {
		t.Fatalf("logging not disabled: %+v", out.LoggingEnabled)
	}
}

func TestBucketLoggingValidation(t *testing.T) {
	e := bcEnv(t)
	e.createBucket("logs")
	doc := func(inner string) []byte {
		return []byte("<BucketLoggingStatus><LoggingEnabled>" + inner + "</LoggingEnabled></BucketLoggingStatus>")
	}
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		code   string
	}{
		{"ok", doc("<TargetBucket>logs</TargetBucket><TargetPrefix>p/</TargetPrefix>"), 200, ""},
		{"no target", doc("<TargetPrefix>p/</TargetPrefix>"), 400, "MalformedXML"},
		{"missing target", doc("<TargetBucket>nope</TargetBucket><TargetPrefix>p/</TargetPrefix>"), 400, "InvalidTargetBucketForLogging"},
		{"long prefix", doc("<TargetBucket>logs</TargetBucket><TargetPrefix>" + strings.Repeat("p", 1025) + "</TargetPrefix>"), 400, "InvalidArgument"},
		{"bad grant permission", doc("<TargetBucket>logs</TargetBucket><TargetPrefix/><TargetGrants><Grant><Grantee><URI>" + groupAllUsers + "</URI></Grantee><Permission>WRITE_ACP</Permission></Grant></TargetGrants>"), 400, "MalformedXML"},
		{"grant without grantee", doc("<TargetBucket>logs</TargetBucket><TargetPrefix/><TargetGrants><Grant><Permission>READ</Permission></Grant></TargetGrants>"), 400, "MalformedXML"},
		{"both key formats", doc("<TargetBucket>logs</TargetBucket><TargetPrefix/><TargetObjectKeyFormat><SimplePrefix/><PartitionedPrefix/></TargetObjectKeyFormat>"), 400, "MalformedXML"},
		{"bad date source", doc("<TargetBucket>logs</TargetBucket><TargetPrefix/><TargetObjectKeyFormat><PartitionedPrefix><PartitionDateSource>Now</PartitionDateSource></PartitionedPrefix></TargetObjectKeyFormat>"), 400, "MalformedXML"},
		{"disable", []byte("<BucketLoggingStatus/>"), 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, "/cfg?logging", tc.body, nil), tc.status, tc.code)
		})
	}
}

// --- Replication ---

func TestBucketReplicationSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	_, err := c.GetBucketReplication(bcCtx, &awss3.GetBucketReplicationInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "ReplicationConfigurationNotFoundError")
	in := &types.ReplicationConfiguration{Role: aws.String("arn:aws:iam::000000000000:role/replication"), Rules: []types.ReplicationRule{
		{ID: aws.String("all"), Priority: aws.Int32(1), Status: types.ReplicationRuleStatusEnabled,
			Filter:                  &types.ReplicationRuleFilter{Prefix: aws.String("")},
			DeleteMarkerReplication: &types.DeleteMarkerReplication{Status: types.DeleteMarkerReplicationStatusDisabled},
			Destination: &types.Destination{Bucket: aws.String("arn:aws:s3:::backup"), StorageClass: types.StorageClassStandardIa,
				ReplicationTime: &types.ReplicationTime{Status: types.ReplicationTimeStatusEnabled, Time: &types.ReplicationTimeValue{Minutes: aws.Int32(15)}},
				Metrics:         &types.Metrics{Status: types.MetricsStatusEnabled, EventThreshold: &types.ReplicationTimeValue{Minutes: aws.Int32(15)}}}},
		{ID: aws.String("tagged"), Priority: aws.Int32(2), Status: types.ReplicationRuleStatusDisabled,
			Filter: &types.ReplicationRuleFilter{And: &types.ReplicationRuleAndOperator{Prefix: aws.String("x/"),
				Tags: []types.Tag{{Key: aws.String("a"), Value: aws.String("1")}, {Key: aws.String("b"), Value: aws.String("2")}}}},
			DeleteMarkerReplication: &types.DeleteMarkerReplication{Status: types.DeleteMarkerReplicationStatusEnabled},
			Destination:             &types.Destination{Bucket: aws.String("arn:aws:s3:::backup")}},
	}}
	_, err = c.PutBucketReplication(bcCtx, &awss3.PutBucketReplicationInput{Bucket: aws.String("cfg"), ReplicationConfiguration: in})
	bcSDKErr(t, err, "InvalidRequest")
	bcVersioned(t, e, "cfg")
	if _, err := c.PutBucketReplication(bcCtx, &awss3.PutBucketReplicationInput{Bucket: aws.String("cfg"), ReplicationConfiguration: in}); err != nil {
		t.Fatal(err)
	}
	out, err := c.GetBucketReplication(bcCtx, &awss3.GetBucketReplicationInput{Bucket: aws.String("cfg")})
	if err != nil {
		t.Fatal(err)
	}
	rc := out.ReplicationConfiguration
	if aws.ToString(rc.Role) != "arn:aws:iam::000000000000:role/replication" || len(rc.Rules) != 2 {
		t.Fatalf("replication: %+v", rc)
	}
	r0, r1 := rc.Rules[0], rc.Rules[1]
	if aws.ToString(r0.ID) != "all" || aws.ToInt32(r0.Priority) != 1 || r0.Filter == nil || aws.ToString(r0.Filter.Prefix) != "" ||
		r0.Destination.StorageClass != types.StorageClassStandardIa || aws.ToInt32(r0.Destination.ReplicationTime.Time.Minutes) != 15 ||
		r0.DeleteMarkerReplication.Status != types.DeleteMarkerReplicationStatusDisabled {
		t.Fatalf("rule 0: %+v", r0)
	}
	if r1.Filter.And == nil || len(r1.Filter.And.Tags) != 2 || aws.ToString(r1.Filter.And.Prefix) != "x/" {
		t.Fatalf("rule 1: %+v", r1)
	}
	bcNS(t, e.signed(http.MethodGet, "/cfg?replication", nil, nil), "ReplicationConfiguration")
	// Versioning cannot be suspended while replication is configured.
	_, err = c.PutBucketVersioning(bcCtx, &awss3.PutBucketVersioningInput{Bucket: aws.String("cfg"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended}})
	bcSDKErr(t, err, "InvalidBucketState")
	if _, err := c.DeleteBucketReplication(bcCtx, &awss3.DeleteBucketReplicationInput{Bucket: aws.String("cfg")}); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetBucketReplication(bcCtx, &awss3.GetBucketReplicationInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "ReplicationConfigurationNotFoundError")
}

func TestBucketReplicationValidation(t *testing.T) {
	e := bcEnv(t)
	bcVersioned(t, e, "cfg")
	doc := func(rules ...string) []byte {
		return []byte("<ReplicationConfiguration><Role>arn:aws:iam::0:role/r</Role>" + strings.Join(rules, "") + "</ReplicationConfiguration>")
	}
	const dest = "<Destination><Bucket>arn:aws:s3:::backup</Bucket></Destination>"
	rule := func(inner string) string { return "<Rule>" + inner + "</Rule>" }
	ok := rule("<Status>Enabled</Status><Prefix></Prefix>" + dest)
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		code   string
	}{
		{"ok", doc(ok), 200, ""},
		{"no rules", doc(), 400, "MalformedXML"},
		{"bad status", doc(rule("<Status>On</Status>" + dest)), 400, "MalformedXML"},
		{"no destination", doc(rule("<Status>Enabled</Status>")), 400, "MalformedXML"},
		{"bad arn", doc(rule("<Status>Enabled</Status><Destination><Bucket>backup</Bucket></Destination>")), 400, "InvalidArgument"},
		{"bad bucket name", doc(rule("<Status>Enabled</Status><Destination><Bucket>arn:aws:s3:::Bad_Name</Bucket></Destination>")), 400, "InvalidArgument"},
		{"bad storage class", doc(rule("<Status>Enabled</Status><Destination><Bucket>arn:aws:s3:::backup</Bucket><StorageClass>CHEAP</StorageClass></Destination>")), 400, "InvalidStorageClass"},
		{"prefix and filter", doc(rule("<Status>Enabled</Status><Prefix/><Filter/>" + dest)), 400, "MalformedXML"},
		{"filter two kinds", doc(rule("<Status>Enabled</Status><Filter><Prefix/><Tag><Key>a</Key><Value>b</Value></Tag></Filter>" + dest)), 400, "MalformedXML"},
		{"filter bad tag", doc(rule("<Status>Enabled</Status><Filter><Tag><Key>aws:x</Key><Value>b</Value></Tag></Filter>" + dest)), 400, "InvalidTag"},
		{"dup ids", doc(rule("<ID>a</ID><Status>Enabled</Status>"+dest), rule("<ID>a</ID><Status>Enabled</Status>"+dest)), 400, "InvalidArgument"},
		{"dup priority", doc(rule("<Priority>1</Priority><Status>Enabled</Status><Filter/>"+dest), rule("<Priority>1</Priority><Status>Enabled</Status><Filter/>"+dest)), 400, "InvalidArgument"},
		{"bad delete marker status", doc(rule("<Status>Enabled</Status><Filter/><DeleteMarkerReplication><Status>Yes</Status></DeleteMarkerReplication>" + dest)), 400, "MalformedXML"},
		{"bad acl owner", doc(rule("<Status>Enabled</Status><Destination><Bucket>arn:aws:s3:::backup</Bucket><AccessControlTranslation><Owner>Me</Owner></AccessControlTranslation></Destination>")), 400, "MalformedXML"},
		{"long id", doc(rule("<ID>" + strings.Repeat("i", 256) + "</ID><Status>Enabled</Status>" + dest)), 400, "InvalidArgument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bcExpect(t, e.signed(http.MethodPut, "/cfg?replication", tc.body, nil), tc.status, tc.code)
		})
	}
}

// --- Accelerate ---

func TestBucketAccelerateSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	out, err := c.GetBucketAccelerateConfiguration(bcCtx, &awss3.GetBucketAccelerateConfigurationInput{Bucket: aws.String("cfg")})
	if err != nil || out.Status != "" {
		t.Fatalf("unset accelerate: %v %+v", err, out)
	}
	resp := e.signed(http.MethodGet, "/cfg?accelerate", nil, nil)
	bcNS(t, resp, "AccelerateConfiguration")
	if bytes.Contains(resp.Data, []byte("Status")) {
		t.Fatalf("unset accelerate body: %s", resp.Data)
	}
	for _, st := range []types.BucketAccelerateStatus{types.BucketAccelerateStatusEnabled, types.BucketAccelerateStatusSuspended} {
		if _, err := c.PutBucketAccelerateConfiguration(bcCtx, &awss3.PutBucketAccelerateConfigurationInput{Bucket: aws.String("cfg"),
			AccelerateConfiguration: &types.AccelerateConfiguration{Status: st}}); err != nil {
			t.Fatal(err)
		}
		out, err := c.GetBucketAccelerateConfiguration(bcCtx, &awss3.GetBucketAccelerateConfigurationInput{Bucket: aws.String("cfg")})
		if err != nil || out.Status != st {
			t.Fatalf("%s: %v %+v", st, err, out)
		}
	}
	bcExpect(t, e.signed(http.MethodPut, "/cfg?accelerate", []byte("<AccelerateConfiguration><Status>On</Status></AccelerateConfiguration>"), nil), 400, "MalformedXML")
	bcExpect(t, e.signed(http.MethodPut, "/cfg?accelerate", []byte("<AccelerateConfiguration/>"), nil), 400, "MalformedXML")
	e.createBucket("with.dot")
	bcExpect(t, e.signed(http.MethodPut, "/with.dot?accelerate", []byte("<AccelerateConfiguration><Status>Enabled</Status></AccelerateConfiguration>"), nil), 400, "InvalidRequest")
}

// --- Request payment ---

func TestBucketRequestPaymentSDK(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	out, err := c.GetBucketRequestPayment(bcCtx, &awss3.GetBucketRequestPaymentInput{Bucket: aws.String("cfg")})
	if err != nil || out.Payer != types.PayerBucketOwner {
		t.Fatalf("payer: %v %+v", err, out)
	}
	bcNS(t, e.signed(http.MethodGet, "/cfg?requestPayment", nil, nil), "RequestPaymentConfiguration")
	if _, err := c.PutBucketRequestPayment(bcCtx, &awss3.PutBucketRequestPaymentInput{Bucket: aws.String("cfg"),
		RequestPaymentConfiguration: &types.RequestPaymentConfiguration{Payer: types.PayerBucketOwner}}); err != nil {
		t.Fatal(err)
	}
	_, err = c.PutBucketRequestPayment(bcCtx, &awss3.PutBucketRequestPaymentInput{Bucket: aws.String("cfg"),
		RequestPaymentConfiguration: &types.RequestPaymentConfiguration{Payer: types.PayerRequester}})
	bcSDKErr(t, err, "NotImplemented")
	bcExpect(t, e.signed(http.MethodPut, "/cfg?requestPayment", []byte("<RequestPaymentConfiguration><Payer>Me</Payer></RequestPaymentConfiguration>"), nil), 400, "MalformedXML")
	if out, _ := c.GetBucketRequestPayment(bcCtx, &awss3.GetBucketRequestPaymentInput{Bucket: aws.String("cfg")}); out.Payer != types.PayerBucketOwner {
		t.Fatalf("payer changed: %+v", out)
	}
}

// --- Stored documents ---

// TestBucketConfigCorruptStored checks that an invalid stored document is
// reported as InternalError (and logged), never echoed or crashed on.
func TestBucketConfigCorruptStored(t *testing.T) {
	e := bcEnv(t)
	for _, tc := range []struct{ sub, key string }{
		{"tagging", storage.ConfigTagging}, {"cors", storage.ConfigCORS}, {"encryption", storage.ConfigEncryption},
		{"ownershipControls", storage.ConfigOwnership}, {"publicAccessBlock", storage.ConfigPublicAccessBlock},
		{"website", storage.ConfigWebsite}, {"notification", storage.ConfigNotification}, {"logging", storage.ConfigLogging},
		{"replication", storage.ConfigReplication}, {"accelerate", storage.ConfigAccelerate}, {"lifecycle", storage.ConfigLifecycle},
	} {
		t.Run(tc.sub, func(t *testing.T) {
			e.putBucketConfig("cfg", tc.key, "<broken")
			bcExpect(t, e.signed(http.MethodGet, "/cfg?"+tc.sub, nil, nil), 500, "InternalError")
			_ = e.backend.DeleteBucketConfig(bcCtx, "cfg", tc.key)
		})
	}
	if !strings.Contains(e.logs.String(), "stored") {
		t.Fatal("corrupt documents were not logged")
	}
}

// TestBucketConfigRoundTripRaw checks that each GET returns exactly what a
// PUT stored (canonical, namespaced) for documents sent without a
// namespace.
func TestBucketConfigRoundTripRaw(t *testing.T) {
	e := bcEnv(t)
	bcVersioned(t, e, "cfg")
	e.createBucket("logs")
	for _, tc := range []struct{ sub, root, body, want string }{
		{"tagging", "Tagging", "<Tagging><TagSet><Tag><Key>k</Key><Value>v</Value></Tag></TagSet></Tagging>", "<Key>k</Key><Value>v</Value>"},
		{"cors", "CORSConfiguration", "<CORSConfiguration><CORSRule><AllowedMethod> GET </AllowedMethod><AllowedOrigin>*</AllowedOrigin></CORSRule></CORSConfiguration>", "<AllowedMethod>GET</AllowedMethod>"},
		{"encryption", "ServerSideEncryptionConfiguration", "<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>", "<SSEAlgorithm>AES256</SSEAlgorithm>"},
		{"website", "WebsiteConfiguration", "<WebsiteConfiguration><IndexDocument><Suffix>i.html</Suffix></IndexDocument></WebsiteConfiguration>", "<Suffix>i.html</Suffix>"},
		{"logging", "BucketLoggingStatus", "<BucketLoggingStatus><LoggingEnabled><TargetBucket>logs</TargetBucket><TargetPrefix></TargetPrefix></LoggingEnabled></BucketLoggingStatus>", "<TargetPrefix></TargetPrefix>"},
		{"accelerate", "AccelerateConfiguration", "<AccelerateConfiguration><Status>Enabled</Status></AccelerateConfiguration>", "<Status>Enabled</Status>"},
		{"object-lock", "ObjectLockConfiguration", "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>", "<ObjectLockEnabled>Enabled</ObjectLockEnabled>"},
		{"publicAccessBlock", "PublicAccessBlockConfiguration", "<PublicAccessBlockConfiguration><IgnorePublicAcls>true</IgnorePublicAcls></PublicAccessBlockConfiguration>", "<IgnorePublicAcls>true</IgnorePublicAcls>"},
	} {
		t.Run(tc.sub, func(t *testing.T) {
			resp := e.signed(http.MethodPut, "/cfg?"+tc.sub, []byte(tc.body), nil)
			if resp.StatusCode/100 != 2 {
				t.Fatalf("PUT: %d %s", resp.StatusCode, resp.Data)
			}
			resp = e.signed(http.MethodGet, "/cfg?"+tc.sub, nil, nil)
			bcExpect(t, resp, 200, "")
			bcNS(t, resp, tc.root)
			if !bytes.Contains(resp.Data, []byte(tc.want)) {
				t.Fatalf("GET body lacks %s: %s", tc.want, resp.Data)
			}
		})
	}
}

// TestBucketConfigDeletedWithBucket checks that configuration documents
// do not survive the bucket (a recreated bucket starts clean).
func TestBucketConfigDeletedWithBucket(t *testing.T) {
	e := bcEnv(t)
	c := e.client
	if _, err := c.PutBucketTagging(bcCtx, &awss3.PutBucketTaggingInput{Bucket: aws.String("cfg"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("k"), Value: aws.String("v")}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteBucket(bcCtx, &awss3.DeleteBucketInput{Bucket: aws.String("cfg")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateBucket(bcCtx, &awss3.CreateBucketInput{Bucket: aws.String("cfg")}); err != nil {
		t.Fatal(err)
	}
	_, err := c.GetBucketTagging(bcCtx, &awss3.GetBucketTaggingInput{Bucket: aws.String("cfg")})
	bcSDKErr(t, err, "NoSuchTagSet")
}
