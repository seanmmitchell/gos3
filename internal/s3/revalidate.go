package s3

// Re-validation of authorized writes right before they commit. Owned by
// the s3-object work item.

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// writeReauthorizeAfter is how long (nanoseconds) a write may have been in
// flight when it commits before its authorization decisions are repeated
// (see revalidateWrite). It is an atomic so tests can lower it.
var writeReauthorizeAfter = newAtomicInt64(int64(5 * time.Second))

// revalidateWrite is called by the handlers of requests with a body right
// before their write commits: PutObject, PostObject, UploadPart,
// CompleteMultipartUpload and the object subresource updates (ACL, tags,
// retention, legal hold).
//
// The caller was authenticated and authorized before its body was read,
// and a body may take arbitrarily long to arrive (the read deadline is a
// rolling idle timeout). So that revoking access also stops requests that
// are already in flight:
//
//   - the caller's access key must still be active and belong to the same
//     enabled user: deleting or deactivating the key, or disabling or
//     deleting the user, makes the commit fail with AccessDenied;
//   - when the request started more than writeReauthorizeAfter ago,
//     reauthorize (if not nil) repeats the request's authorization
//     decisions against the current policies, ACLs, ownership controls
//     and public access block, so a policy change or a newly enabled
//     BlockPublicAcls applies too. The repeated decisions evaluate the
//     condition keys derived from the clock as of the request's start
//     (see requestTimeConditions): the request is judged as it was made,
//     so a long upload is not refused because, say, its signature has
//     aged past an s3:signatureAge limit while the body was arriving, as
//     AWS would not refuse it either.
//
// Short requests skip the second step: their decision is at most a few
// seconds old, and repeating it would double the authorization work of
// every small upload.
func (s *Server) revalidateWrite(rc *reqContext, reauthorize func() error) error {
	if err := s.checkCredentialsCurrent(rc); err != nil {
		return err
	}
	if reauthorize == nil || s.now().Sub(rc.start) < time.Duration(writeReauthorizeAfter.Load()) {
		return nil
	}
	return reauthorize()
}

// checkCredentialsCurrent returns AccessDenied when the access key that
// authenticated the request no longer resolves to the same principal: it
// was deleted or deactivated, or its user was disabled or deleted (IAM
// lookups fail for all of these). Anonymous requests have nothing to
// check.
func (s *Server) checkCredentialsCurrent(rc *reqContext) error {
	id := rc.identity
	if id.IsAnonymous || id.AccessKey == "" || s.iam == nil {
		return nil
	}
	if _, cur, err := s.iam.LookupCredentials(id.AccessKey); err == nil && cur.User == id.User && cur.IsRoot == id.IsRoot {
		return nil
	}
	rc.authzReason = "access key revoked during the request"
	return s3err.ErrAccessDenied
}

// reauthorizeOp repeats the authorization of the request's own operation
// (authorizeOpWith with the same object attribute condition keys attrs,
// nil for the request headers) for revalidateWrite, with the condition
// keys derived from the clock evaluated as of the request's start. It
// builds the same authzTarget as authorizeOpWith.
func (s *Server) reauthorizeOp(ctx context.Context, r *http.Request, rc *reqContext, attrs map[string][]string) error {
	op := rc.op
	v := authzVersionID(op, rc)
	t := authzTarget{
		Action:         op.action(v),
		VersionID:      v,
		ObjectRead:     op.ObjectRead,
		NoBucketLookup: op.NoBucketLookup,
		ObjectAttrs:    attrs,
	}
	if attrs == nil && !(rc.identity.IsRoot && !rc.identity.IsAnonymous) {
		switch op.Name {
		case "UploadPart", "UploadPartCopy", "CompleteMultipartUpload":
			if up, err := s.backend.GetMultipartUpload(ctx, rc.bucket, rc.key, rc.query.Get("uploadId")); err == nil {
				t.ObjectAttrs = objectAttrConditions(up.Meta, up.Meta.ACL != "", up.Meta.LegalHold, len(up.Meta.Tags) > 0, rc.start)
			} else {
				t.ObjectAttrs = map[string][]string{}
			}
		}
	}
	switch op.Kind {
	case resBucket:
		t.Bucket = rc.bucket
	case resObject:
		t.Bucket, t.Key = rc.bucket, rc.key
	}
	t.Conditions = s.requestTimeConditions(r, rc, t.ObjectAttrs)
	if err := s.authorizeTarget(ctx, r, rc, t); err != nil {
		return err
	}
	return s.authorizeObjectRead(ctx, r, rc, t)
}

// requestTimeConditions returns the condition keys derived from the clock
// as they were when the request started (rc.start): aws:CurrentTime,
// aws:EpochTime, s3:signatureAge and, when the object written has a
// retain-until date, s3:object-lock-remaining-retention-days. Passed as
// authzTarget.Conditions they replace the values conditionContext computes
// from the current time. attrs are the object attribute condition keys the
// decision uses (nil: the request headers).
func (s *Server) requestTimeConditions(r *http.Request, rc *reqContext, attrs map[string][]string) map[string][]string {
	start := rc.start.UTC()
	c := map[string][]string{
		"aws:CurrentTime": {start.Format("2006-01-02T15:04:05Z")},
		"aws:EpochTime":   {strconv.FormatInt(start.Unix(), 10)},
	}
	if rc.auth != nil && !rc.auth.SignedAt.IsZero() {
		c["s3:signatureAge"] = []string{strconv.FormatInt(max(start.Sub(rc.auth.SignedAt).Milliseconds(), 0), 10)}
	}
	var until string
	if attrs != nil {
		if v := attrs["s3:object-lock-retain-until-date"]; len(v) > 0 {
			until = v[0]
		}
	} else {
		until = trimHeaderSpace(r.Header.Get("x-amz-object-lock-retain-until-date"))
	}
	if until != "" {
		if ut, ok := parseISOTime(until); ok {
			c["s3:object-lock-remaining-retention-days"] = []string{remainingRetentionDays(ut, start)}
		}
	}
	return c
}
