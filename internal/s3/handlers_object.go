package s3

// This file holds the object operations; it is owned by the s3-object work item.
// The bodies below are stubs created by s3-core so the package compiles;
// see doc.go (HANDLER GUIDE) for the helpers available to handlers.

import (
	"net/http"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// handlePutObject implements PutObject.
func (s *Server) handlePutObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleCopyObject implements CopyObject.
func (s *Server) handleCopyObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleGetObject implements GetObject.
func (s *Server) handleGetObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleHeadObject implements HeadObject.
func (s *Server) handleHeadObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleDeleteObject implements DeleteObject.
func (s *Server) handleDeleteObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleDeleteObjects implements DeleteObjects.
func (s *Server) handleDeleteObjects(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleGetObjectTagging implements GetObjectTagging.
func (s *Server) handleGetObjectTagging(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handlePutObjectTagging implements PutObjectTagging.
func (s *Server) handlePutObjectTagging(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleDeleteObjectTagging implements DeleteObjectTagging.
func (s *Server) handleDeleteObjectTagging(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleGetObjectAcl implements GetObjectAcl.
func (s *Server) handleGetObjectAcl(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handlePutObjectAcl implements PutObjectAcl.
func (s *Server) handlePutObjectAcl(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleGetObjectRetention implements GetObjectRetention.
func (s *Server) handleGetObjectRetention(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handlePutObjectRetention implements PutObjectRetention.
func (s *Server) handlePutObjectRetention(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleGetObjectLegalHold implements GetObjectLegalHold.
func (s *Server) handleGetObjectLegalHold(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handlePutObjectLegalHold implements PutObjectLegalHold.
func (s *Server) handlePutObjectLegalHold(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleGetObjectAttributes implements GetObjectAttributes.
func (s *Server) handleGetObjectAttributes(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handlePostObject implements PostObject.
func (s *Server) handlePostObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleRestoreObject implements RestoreObject.
func (s *Server) handleRestoreObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleSelectObjectContent implements SelectObjectContent.
func (s *Server) handleSelectObjectContent(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}
