package s3

// This file holds the bucket operations; it is owned by the s3-bucket work item.
// The bodies below are stubs created by s3-core so the package compiles;
// see doc.go (HANDLER GUIDE) for the helpers available to handlers.

import (
	"net/http"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// handleListBuckets implements ListBuckets.
func (s *Server) handleListBuckets(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleCreateBucket implements CreateBucket.
func (s *Server) handleCreateBucket(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleDeleteBucket implements DeleteBucket.
func (s *Server) handleDeleteBucket(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleHeadBucket implements HeadBucket.
func (s *Server) handleHeadBucket(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleGetBucketLocation implements GetBucketLocation.
func (s *Server) handleGetBucketLocation(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleListObjects implements ListObjects.
func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleListObjectsV2 implements ListObjectsV2.
func (s *Server) handleListObjectsV2(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}

// handleListObjectVersions implements ListObjectVersions.
func (s *Server) handleListObjectVersions(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented
}
