package s3client

import (
	"context"
	"encoding/xml"
	"net/http"
	"strings"
	"time"
)

// retainUntilLayout is the RetainUntilDate format sent to the service.
const retainUntilLayout = "2006-01-02T15:04:05.000Z"

// Retention is an object version's retention.
type Retention struct {
	Mode        string // "GOVERNANCE", "COMPLIANCE" or "" (none)
	RetainUntil time.Time
}

// retentionDoc is the <Retention> document.
type retentionDoc struct {
	XMLName         xml.Name `xml:"Retention"`
	XMLNS           string   `xml:"xmlns,attr,omitempty"`
	Mode            string   `xml:"Mode,omitempty"`
	RetainUntilDate string   `xml:"RetainUntilDate,omitempty"`
}

// legalHoldDoc is the <LegalHold> document.
type legalHoldDoc struct {
	XMLName xml.Name `xml:"LegalHold"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status"`
}

// noObjectLock reports the answers of services for a version without
// retention or legal hold (AWS: NoSuchObjectLockConfiguration).
func noObjectLock(err error) bool {
	return IsNotFound(err) && (IsCode(err, "NoSuchObjectLockConfiguration") || IsCode(err, "ObjectLockConfigurationNotFoundError"))
}

// formatRetainUntil formats t as a RetainUntilDate: UTC with millisecond
// precision, rounded up so the retention is never shorter than asked.
func formatRetainUntil(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond()%int(time.Millisecond) != 0 {
		t = t.Truncate(time.Millisecond).Add(time.Millisecond)
	}
	return t.Format(retainUntilLayout)
}

// GetObjectRetention returns a version's retention (versionID "" = the
// current one); the zero Retention when it has none.
func (c *Client) GetObjectRetention(ctx context.Context, bucket, key, versionID string) (Retention, error) {
	resp, err := c.do(ctx, request{
		op: "GetObjectRetention", method: http.MethodGet, bucket: bucket, key: key, hasKey: true,
		query: objectQuery("retention", versionID), retryTransport: true,
	})
	if noObjectLock(err) {
		return Retention{}, nil
	}
	if err != nil {
		return Retention{}, err
	}
	var out retentionDoc
	if err := decodeXML("GetObjectRetention", resp, &out); err != nil {
		return Retention{}, err
	}
	r := Retention{Mode: strings.TrimSpace(out.Mode), RetainUntil: parseTime(out.RetainUntilDate)}
	if r.Mode == "" {
		return Retention{}, nil
	}
	return r, nil
}

// PutObjectRetention sets (or, with the zero Retention, removes) a
// version's retention. RetainUntil is sent rounded up to the millisecond.
// bypassGovernance sends x-amz-bypass-governance-retention: true, which
// shortening or removing an active GOVERNANCE retention requires.
func (c *Client) PutObjectRetention(ctx context.Context, bucket, key, versionID string, r Retention, bypassGovernance bool) error {
	doc := retentionDoc{XMLNS: s3Namespace, Mode: r.Mode}
	if r.Mode != "" {
		doc.RetainUntilDate = formatRetainUntil(r.RetainUntil)
	}
	req, err := xmlRequest("PutObjectRetention", http.MethodPut, bucket, objectQuery("retention", versionID), doc)
	if err != nil {
		return err
	}
	req.key, req.hasKey, req.retryTransport = key, true, true
	if bypassGovernance {
		req.header.Set("X-Amz-Bypass-Governance-Retention", "true")
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// GetObjectLegalHold reports a version's legal hold (versionID "" = the
// current one); a version that never had one reports false.
func (c *Client) GetObjectLegalHold(ctx context.Context, bucket, key, versionID string) (bool, error) {
	resp, err := c.do(ctx, request{
		op: "GetObjectLegalHold", method: http.MethodGet, bucket: bucket, key: key, hasKey: true,
		query: objectQuery("legal-hold", versionID), retryTransport: true,
	})
	if noObjectLock(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var out legalHoldDoc
	if err := decodeXML("GetObjectLegalHold", resp, &out); err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(out.Status), "ON"), nil
}

// PutObjectLegalHold sets a version's legal hold.
func (c *Client) PutObjectLegalHold(ctx context.Context, bucket, key, versionID string, on bool) error {
	doc := legalHoldDoc{XMLNS: s3Namespace, Status: "OFF"}
	if on {
		doc.Status = "ON"
	}
	req, err := xmlRequest("PutObjectLegalHold", http.MethodPut, bucket, objectQuery("legal-hold", versionID), doc)
	if err != nil {
		return err
	}
	req.key, req.hasKey, req.retryTransport = key, true, true
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}
