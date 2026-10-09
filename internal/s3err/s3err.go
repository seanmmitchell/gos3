// Package s3err defines the S3 API error codes returned by gos3.
//
// Every S3-facing package (auth, s3, lifecycle, ...) returns *Error values from
// this package so the HTTP layer can render the canonical S3 XML error body:
//
//	<Error><Code>NoSuchKey</Code><Message>...</Message><Resource>...</Resource><RequestId>...</RequestId></Error>
//
// New codes may be appended to the table below; never change existing ones.
package s3err

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is an S3 API error.
type Error struct {
	Code       string
	Message    string
	HTTPStatus int
	// Extra holds optional additional XML elements rendered inside <Error>
	// (e.g. "BucketName", "Key", "ArgumentName", "ArgumentValue").
	Extra map[string]string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// WithMessage returns a copy of e with a custom message.
func (e *Error) WithMessage(format string, args ...any) *Error {
	c := e.clone()
	if len(args) > 0 {
		c.Message = fmt.Sprintf(format, args...)
	} else {
		c.Message = format
	}
	return c
}

// With returns a copy of e carrying an extra XML element (e.g. With("Key", key)).
func (e *Error) With(name, value string) *Error {
	c := e.clone()
	if c.Extra == nil {
		c.Extra = map[string]string{}
	}
	c.Extra[name] = value
	return c
}

// Is reports whether target is an *Error with the same Code, so
// errors.Is(err, s3err.ErrNoSuchKey) works on copies made by WithMessage/With.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

func (e *Error) clone() *Error {
	c := *e
	if e.Extra != nil {
		c.Extra = make(map[string]string, len(e.Extra))
		for k, v := range e.Extra {
			c.Extra[k] = v
		}
	}
	return &c
}

// As extracts an *Error from err. Non-S3 errors become InternalError.
func As(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return ErrInternalError
}

func def(code string, status int, msg string) *Error {
	return &Error{Code: code, Message: msg, HTTPStatus: status}
}

// Canonical S3 errors.
var (
	ErrAccessDenied                         = def("AccessDenied", http.StatusForbidden, "Access Denied")
	ErrAccountProblem                       = def("AccountProblem", http.StatusForbidden, "There is a problem with your account that prevents the operation from completing successfully.")
	ErrAuthorizationHeaderMalformed         = def("AuthorizationHeaderMalformed", http.StatusBadRequest, "The authorization header is malformed.")
	ErrAuthorizationQueryParametersError    = def("AuthorizationQueryParametersError", http.StatusBadRequest, "Error parsing the X-Amz-Credential parameter.")
	ErrBadDigest                            = def("BadDigest", http.StatusBadRequest, "The Content-MD5 or checksum value that you specified did not match what the server received.")
	ErrBucketAlreadyExists                  = def("BucketAlreadyExists", http.StatusConflict, "The requested bucket name is not available. The bucket namespace is shared by all users of the system. Please select a different name and try again.")
	ErrBucketAlreadyOwnedByYou              = def("BucketAlreadyOwnedByYou", http.StatusConflict, "Your previous request to create the named bucket succeeded and you already own it.")
	ErrBucketNotEmpty                       = def("BucketNotEmpty", http.StatusConflict, "The bucket you tried to delete is not empty.")
	ErrEntityTooLarge                       = def("EntityTooLarge", http.StatusBadRequest, "Your proposed upload exceeds the maximum allowed object size.")
	ErrEntityTooSmall                       = def("EntityTooSmall", http.StatusBadRequest, "Your proposed upload is smaller than the minimum allowed object size.")
	ErrExpiredToken                         = def("ExpiredToken", http.StatusBadRequest, "The provided token has expired.")
	ErrIllegalLocationConstraintException   = def("IllegalLocationConstraintException", http.StatusBadRequest, "The unspecified location constraint is incompatible for the region specific endpoint this request was sent to.")
	ErrIllegalVersioningConfiguration       = def("IllegalVersioningConfigurationException", http.StatusBadRequest, "The versioning configuration specified in the request is invalid.")
	ErrIncompleteBody                       = def("IncompleteBody", http.StatusBadRequest, "You did not provide the number of bytes specified by the Content-Length HTTP header.")
	ErrInternalError                        = def("InternalError", http.StatusInternalServerError, "We encountered an internal error. Please try again.")
	ErrInvalidAccessKeyID                   = def("InvalidAccessKeyId", http.StatusForbidden, "The AWS access key ID you provided does not exist in our records.")
	ErrInvalidArgument                      = def("InvalidArgument", http.StatusBadRequest, "Invalid Argument")
	ErrInvalidBucketName                    = def("InvalidBucketName", http.StatusBadRequest, "The specified bucket is not valid.")
	ErrInvalidBucketState                   = def("InvalidBucketState", http.StatusConflict, "The request is not valid with the current state of the bucket.")
	ErrInvalidDigest                        = def("InvalidDigest", http.StatusBadRequest, "The Content-MD5 or checksum value you specified is not valid.")
	ErrInvalidEncryptionAlgorithm           = def("InvalidEncryptionAlgorithmError", http.StatusBadRequest, "The encryption request that you specified is not valid.")
	ErrInvalidLocationConstraint            = def("InvalidLocationConstraint", http.StatusBadRequest, "The specified location constraint is not valid.")
	ErrInvalidObjectState                   = def("InvalidObjectState", http.StatusForbidden, "The operation is not valid for the current state of the object.")
	ErrInvalidPart                          = def("InvalidPart", http.StatusBadRequest, "One or more of the specified parts could not be found. The part might not have been uploaded, or the specified entity tag might not have matched the part's entity tag.")
	ErrInvalidPartOrder                     = def("InvalidPartOrder", http.StatusBadRequest, "The list of parts was not in ascending order. The parts list must be specified in order by part number.")
	ErrInvalidPolicyDocument                = def("InvalidPolicyDocument", http.StatusBadRequest, "The content of the form does not meet the conditions specified in the policy document.")
	ErrInvalidRange                         = def("InvalidRange", http.StatusRequestedRangeNotSatisfiable, "The requested range is not satisfiable")
	ErrInvalidRequest                       = def("InvalidRequest", http.StatusBadRequest, "Invalid Request")
	ErrInvalidStorageClass                  = def("InvalidStorageClass", http.StatusBadRequest, "The storage class you specified is not valid.")
	ErrInvalidTag                           = def("InvalidTag", http.StatusBadRequest, "The tag provided was not a valid tag.")
	ErrInvalidURI                           = def("InvalidURI", http.StatusBadRequest, "Couldn't parse the specified URI.")
	ErrInvalidRetentionPeriod               = def("InvalidRetentionPeriod", http.StatusBadRequest, "The retention period specified is not valid.")
	ErrKeyTooLong                           = def("KeyTooLongError", http.StatusBadRequest, "Your key is too long.")
	ErrMalformedACL                         = def("MalformedACLError", http.StatusBadRequest, "The XML you provided was not well-formed or did not validate against our published schema.")
	ErrMalformedPOSTRequest                 = def("MalformedPOSTRequest", http.StatusBadRequest, "The body of your POST request is not well-formed multipart/form-data.")
	ErrMalformedPolicy                      = def("MalformedPolicy", http.StatusBadRequest, "Policy has invalid resource.")
	ErrMalformedXML                         = def("MalformedXML", http.StatusBadRequest, "The XML you provided was not well-formed or did not validate against our published schema.")
	ErrMaxMessageLengthExceeded             = def("MaxMessageLengthExceeded", http.StatusBadRequest, "Your request was too big.")
	ErrMetadataTooLarge                     = def("MetadataTooLarge", http.StatusBadRequest, "Your metadata headers exceed the maximum allowed metadata size.")
	ErrMethodNotAllowed                     = def("MethodNotAllowed", http.StatusMethodNotAllowed, "The specified method is not allowed against this resource.")
	ErrMissingContentLength                 = def("MissingContentLength", http.StatusLengthRequired, "You must provide the Content-Length HTTP header.")
	ErrMissingRequestBody                   = def("MissingRequestBodyError", http.StatusBadRequest, "Request Body is empty.")
	ErrMissingSecurityHeader                = def("MissingSecurityHeader", http.StatusBadRequest, "Your request was missing a required header.")
	ErrNoSuchBucket                         = def("NoSuchBucket", http.StatusNotFound, "The specified bucket does not exist.")
	ErrNoSuchBucketPolicy                   = def("NoSuchBucketPolicy", http.StatusNotFound, "The bucket policy does not exist.")
	ErrNoSuchCORSConfiguration              = def("NoSuchCORSConfiguration", http.StatusNotFound, "The CORS configuration does not exist.")
	ErrNoSuchKey                            = def("NoSuchKey", http.StatusNotFound, "The specified key does not exist.")
	ErrNoSuchLifecycleConfiguration         = def("NoSuchLifecycleConfiguration", http.StatusNotFound, "The lifecycle configuration does not exist.")
	ErrNoSuchObjectLockConfiguration        = def("ObjectLockConfigurationNotFoundError", http.StatusNotFound, "Object Lock configuration does not exist for this bucket.")
	ErrNoSuchObjectLegalHold                = def("NoSuchObjectLockConfiguration", http.StatusNotFound, "The specified object does not have an ObjectLock configuration.")
	ErrNoSuchTagSet                         = def("NoSuchTagSet", http.StatusNotFound, "The TagSet does not exist.")
	ErrNoSuchUpload                         = def("NoSuchUpload", http.StatusNotFound, "The specified multipart upload does not exist. The upload ID might be invalid, or the multipart upload might have been aborted or completed.")
	ErrNoSuchVersion                        = def("NoSuchVersion", http.StatusNotFound, "The specified version does not exist.")
	ErrNoSuchWebsiteConfiguration           = def("NoSuchWebsiteConfiguration", http.StatusNotFound, "The specified bucket does not have a website configuration.")
	ErrNotImplemented                       = def("NotImplemented", http.StatusNotImplemented, "A header or query you provided implies functionality that is not implemented.")
	ErrNotModified                          = def("NotModified", http.StatusNotModified, "Not Modified")
	ErrObjectLockConfigurationNotFound      = ErrNoSuchObjectLockConfiguration
	ErrOwnershipControlsNotFound            = def("OwnershipControlsNotFoundError", http.StatusNotFound, "The bucket ownership controls were not found.")
	ErrPreconditionFailed                   = def("PreconditionFailed", http.StatusPreconditionFailed, "At least one of the pre-conditions you specified did not hold")
	ErrConditionalRequestConflict           = def("ConditionalRequestConflict", http.StatusConflict, "A conflicting conditional operation is currently in progress against this resource.")
	ErrRequestTimeTooSkewed                 = def("RequestTimeTooSkewed", http.StatusForbidden, "The difference between the request time and the server's time is too large.")
	ErrRequestTimeout                       = def("RequestTimeout", http.StatusBadRequest, "Your socket connection to the server was not read from or written to within the timeout period.")
	ErrServerSideEncryptionConfigNotFound   = def("ServerSideEncryptionConfigurationNotFoundError", http.StatusNotFound, "The server side encryption configuration was not found.")
	ErrSignatureDoesNotMatch                = def("SignatureDoesNotMatch", http.StatusForbidden, "The request signature we calculated does not match the signature you provided. Check your key and signing method.")
	ErrSlowDown                             = def("SlowDown", http.StatusServiceUnavailable, "Please reduce your request rate.")
	ErrTooManyBuckets                       = def("TooManyBuckets", http.StatusBadRequest, "You have attempted to create more buckets than allowed.")
	ErrUnexpectedContent                    = def("UnexpectedContent", http.StatusBadRequest, "This request does not support content.")
	ErrUnsupportedArgument                  = def("InvalidArgument", http.StatusBadRequest, "The argument is not supported.")
	ErrXAmzContentSHA256Mismatch            = def("XAmzContentSHA256Mismatch", http.StatusBadRequest, "The provided 'x-amz-content-sha256' header does not match what was computed.")
	ErrMissingFields                        = def("MissingFields", http.StatusBadRequest, "Missing fields in request.")
	ErrInvalidStorageClassTransition        = def("InvalidRequest", http.StatusBadRequest, "Storage class transitions are not supported.")
	ErrReplicationConfigurationNotFound     = def("ReplicationConfigurationNotFoundError", http.StatusNotFound, "The replication configuration was not found.")
	ErrNoSuchPublicAccessBlockConfiguration = def("NoSuchPublicAccessBlockConfiguration", http.StatusNotFound, "The public access block configuration was not found.")
)
