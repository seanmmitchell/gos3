// Package e2e holds end-to-end tests that run complete gos3 servers
// in-process (internal/server, exactly as cmd/gos3 assembles them) on
// loopback listeners and drive them with real S3 clients: aws-sdk-go-v2
// (including feature/s3/manager) and minio-go/v7, plus raw signed HTTP
// requests and the admin API.
//
// TestMain starts two shared servers:
//
//   - an fs-backed server over plain HTTP in a temporary directory with the
//     virtual-host domain "s3.test" (clients map every host to the listener,
//     so no DNS is needed);
//   - a memory-backed server over HTTPS (self-signed certificate), which
//     makes the AWS SDK use its default aws-chunked
//     STREAMING-UNSIGNED-PAYLOAD-TRAILER uploads.
//
// Tests that need their own lifecycle (restart persistence) start private
// servers. Run with `go test -race ./test/e2e/` (or `make e2e`).
package e2e
