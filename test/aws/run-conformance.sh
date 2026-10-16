#!/bin/bash
# The object store's conformance tests (internal/objstore) against a real
# S3 bucket instead of a MinIO container: conditional writes and their
# 412, put-if-absent, multipart writes under If-Match, version listings,
# deleting versions, the server's clock.
#
#   test/aws/run-conformance.sh [go test flags]
#
# .env.aws (gitignored, in the repository root) names the bucket:
#   ICBFS_TEST_S3_BUCKET=...   a versioned bucket the tests may write to
#   ICBFS_TEST_S3_REGION=...   (default us-east-1)
# Credentials come from the AWS CLI's (aws configure export-credentials),
# so nothing secret is written down. Each test writes under a prefix of its
# own; the bucket's lifecycle rules should expire what's left.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
set -a
# shellcheck disable=SC1091
source "$REPO/.env.aws"
set +a
eval "$(aws configure export-credentials --format env)"
cd "$REPO"
exec go test -count=1 "$@" ./internal/objstore
