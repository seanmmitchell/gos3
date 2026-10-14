package admin

import (
	"net/http"
	"strconv"
	"time"
)

// reauthorizeAfter is how long a request may have been in flight when its
// body has been read before the route's authorization is repeated (see
// revalidateAfterBody). It mirrors the S3 API's writeReauthorizeAfter.
const reauthorizeAfter = 5 * time.Second

// revalidateAfterBody is called once the request body has been read (by
// readBody, so every handler that acts on a body goes through it).
//
// Routes are authenticated and authorized before their body is read, and a
// body may take arbitrarily long to arrive (the read deadline is a rolling
// idle timeout). So that revoking access also stops admin requests that are
// already in flight (a slowly dripped PUT /policies or POST /users/{name}/keys
// must not land after its key was deleted):
//
//   - the caller's access key must still be active and belong to the same
//     enabled user: deleting or deactivating the key, or disabling or
//     deleting the user, fails the request with 403 AccessDenied before the
//     handler acts;
//   - when the request started more than reauthorizeAfter ago, the route's
//     own action (if it was authorized before the body was read) is
//     authorized again against the current policies, with the time
//     condition keys (aws:CurrentTime, aws:EpochTime) as of the request's
//     start, so a new Deny or a detached policy applies but a slow body is
//     not refused merely for having aged.
//
// Actions that handlers authorize after decoding the body (inHandler
// routes and extra requireAction checks) are evaluated afresh anyway.
func (a *api) revalidateAfterBody(rc *reqCtx) error {
	id := rc.id
	if id.AccessKey != "" && !id.IsAnonymous {
		_, cur, err := a.d.IAM.LookupCredentials(id.AccessKey)
		if err != nil || cur.User != id.User || cur.IsRoot != id.IsRoot {
			a.log.Info("admin: request refused: access key revoked while its body was read",
				"method", rc.r.Method, "path", rc.r.URL.Path, "user", id.User, "accessKey", id.AccessKey)
			return errorf(http.StatusForbidden, codeAccessDenied,
				"%s: the access key that signed this request was revoked while its body was being read", describeIdentity(id))
		}
	}
	if !rc.preAuthorized || a.now().Sub(rc.start) < reauthorizeAfter {
		return nil
	}
	start := rc.start.UTC()
	asOfStart := map[string][]string{
		"aws:CurrentTime": {start.Format(time.RFC3339)},
		"aws:EpochTime":   {strconv.FormatInt(start.Unix(), 10)},
	}
	return a.requireAction(rc, rc.action, rc.resource, asOfStart)
}
