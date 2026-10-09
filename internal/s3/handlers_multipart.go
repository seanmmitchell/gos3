package s3

// This file holds the multipart upload operations; it is owned by the s3-multipart work item.
// The bodies below are stubs created by s3-core so the package compiles;
// see doc.go (HANDLER GUIDE) for the helpers available to handlers.

import (
	"net/http"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// handleCreateMultipartUpload implements CreateMultipartUpload.
func (s *Server) handleCreateMultipartUpload(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleUploadPart implements UploadPart.
func (s *Server) handleUploadPart(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleUploadPartCopy implements UploadPartCopy.
func (s *Server) handleUploadPartCopy(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleCompleteMultipartUpload implements CompleteMultipartUpload.
func (s *Server) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleAbortMultipartUpload implements AbortMultipartUpload.
func (s *Server) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleListParts implements ListParts.
func (s *Server) handleListParts(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleListMultipartUploads implements ListMultipartUploads.
func (s *Server) handleListMultipartUploads(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}
