#!/bin/bash
# Throwaway MinIO for one test run. Sourced, not run directly — use
# with-minio.sh (or up.sh/down.sh by hand).
#
# Each run gets its own container (unique name, random host port,
# anonymous data volume removed with the container), so concurrent runs
# don't collide and nothing survives a run. The Go unit tests don't use
# this: they start their own MinIO through testcontainers.

# MinIO no longer publishes minio/minio for free use; Chainguard's build is
# free, as its latest tag only (internal/testutil/minioimage says the same).
ICBFS_MINIO_IMAGE="${ICBFS_MINIO_IMAGE:-cgr.dev/chainguard/minio:latest}"
ICBFS_MINIO_ACCESS_KEY="${ICBFS_MINIO_ACCESS_KEY:-minioadmin}"
ICBFS_MINIO_SECRET_KEY="${ICBFS_MINIO_SECRET_KEY:-minioadmin}"
ICBFS_MINIO_BUCKET="${ICBFS_MINIO_BUCKET:-icbfs-test}"

# minio_host_addr is the address the test VMs reach this host on: the
# libvirt default bridge, unless ICBFS_MINIO_HOST_ADDR says otherwise.
minio_host_addr() {
  if [ -n "${ICBFS_MINIO_HOST_ADDR:-}" ]; then
    echo "$ICBFS_MINIO_HOST_ADDR"
    return
  fi
  ip -4 addr show virbr0 2>/dev/null | awk '/inet /{print $2}' | cut -d/ -f1
}

# minio_up starts a fresh MinIO with an empty, versioned bucket and
# exports ICBFS_MINIO_CONTAINER, ICBFS_MINIO_ENDPOINT (from this host),
# ICBFS_MINIO_VM_ENDPOINT (from the test VMs), ICBFS_MINIO_BUCKET and the
# access/secret keys.
minio_up() {
  local name="icbfs-test-minio-$$-$RANDOM"
  docker run -d --rm --name "$name" \
    -p 0.0.0.0::9000 \
    -e MINIO_ROOT_USER="$ICBFS_MINIO_ACCESS_KEY" \
    -e MINIO_ROOT_PASSWORD="$ICBFS_MINIO_SECRET_KEY" \
    "$ICBFS_MINIO_IMAGE" server /data >/dev/null || return 1
  export ICBFS_MINIO_CONTAINER="$name"

  local port
  port=$(docker port "$name" 9000/tcp | awk -F: '/0.0.0.0/{print $NF; exit}')
  if [ -z "$port" ]; then
    echo "minio: couldn't read the published port of $name" >&2
    return 1
  fi
  export ICBFS_MINIO_ENDPOINT="http://127.0.0.1:${port}"
  local host
  host=$(minio_host_addr)
  if [ -z "$host" ]; then
    echo "minio: no VM-facing host address (no virbr0? set ICBFS_MINIO_HOST_ADDR)" >&2
    return 1
  fi
  export ICBFS_MINIO_VM_ENDPOINT="http://${host}:${port}"
  export ICBFS_MINIO_BUCKET ICBFS_MINIO_ACCESS_KEY ICBFS_MINIO_SECRET_KEY

  local i
  for i in $(seq 1 60); do
    curl -fsS -o /dev/null "$ICBFS_MINIO_ENDPOINT/minio/health/ready" 2>/dev/null && break
    sleep 0.5
  done
  if ! curl -fsS -o /dev/null "$ICBFS_MINIO_ENDPOINT/minio/health/ready" 2>/dev/null; then
    echo "minio: $name never became ready" >&2
    return 1
  fi

  # icbfs requires a versioned bucket.
  local auth=(--aws-sigv4 "aws:amz:us-east-1:s3" -u "$ICBFS_MINIO_ACCESS_KEY:$ICBFS_MINIO_SECRET_KEY")
  curl -fsS "${auth[@]}" -X PUT "$ICBFS_MINIO_ENDPOINT/$ICBFS_MINIO_BUCKET" -o /dev/null || return 1
  curl -fsS "${auth[@]}" -X PUT "$ICBFS_MINIO_ENDPOINT/$ICBFS_MINIO_BUCKET?versioning" \
    -H 'Content-Type: application/xml' \
    --data '<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>' \
    -o /dev/null || return 1
  echo "minio: $name up — $ICBFS_MINIO_ENDPOINT (VMs: $ICBFS_MINIO_VM_ENDPOINT), bucket $ICBFS_MINIO_BUCKET" >&2
}

# minio_down removes the container started by minio_up (or the one named
# in $1), together with its data.
minio_down() {
  local name="${1:-${ICBFS_MINIO_CONTAINER:-}}"
  [ -n "$name" ] || return 0
  # rm -v, not stop: --rm cleans up asynchronously, and the run should
  # be gone, data volume included, by the time this returns.
  docker rm -f -v "$name" >/dev/null 2>&1 || true
  echo "minio: $name removed" >&2
}
