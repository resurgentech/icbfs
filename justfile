# icbfs tasks. `just` lists them.

linux_cmds := "./cmd/icbfs ./cmd/icbfs-cli"
windows_cmds := "./cmd/icbfs-winfsp ./cmd/icbfs-cli"
vm_ip := `virsh domifaddr icbfs-fuse-test 2>/dev/null | awk '/ipv4/{print $4}' | cut -d/ -f1`

[private]
default:
    @just --list

# Build the Linux and Windows commands into bin/
build: build-linux build-windows

build-linux:
    go build -o bin/ {{linux_cmds}}

build-windows:
    GOOS=windows go build -o bin/ {{windows_cmds}}
    GOOS=windows go build -ldflags -H=windowsgui -o bin/icbfs-sync.exe ./cmd/icbfs-sync

# Every Go test (needs Docker, and /dev/fuse for the mount tests)
test *args:
    go test {{args}} ./...

# The Go tests without the code-generator check
test-short *args:
    go test -short {{args}} ./...

# The Go tests CI runs, with the race detector
test-ci:
    go test -race ./...

# The Go tests that run on Windows (no Docker there)
test-windows:
    go test ./internal/acl ./internal/block ./internal/syncer ./internal/syncer/folderside ./pkg/storeflags ./pkg/tokencmd

# go vet, for Linux and for the Windows packages
vet:
    go vet ./...
    GOOS=windows go vet $(GOOS=windows go list ./... | grep -v -e /internal/fuseserver -e '/cmd/icbfs$')

# Reformat
fmt:
    gofmt -w cmd internal pkg

# Fail if anything isn't gofmt'ed
fmt-check:
    @test -z "$(gofmt -l cmd internal pkg)" || { gofmt -l cmd internal pkg; echo "run: just fmt"; exit 1; }

# Fail if go.mod or go.sum isn't tidy
tidy-check:
    go mod tidy -diff

# Regenerate the protobuf code
generate:
    PATH="$PATH:$(go env GOPATH)/bin" protoc --go_out=. --go_opt=module=github.com/resurgentech/icbfs spec/proto/icbfs/v1/*.proto

# What CI checks
ci: fmt-check tidy-check vet build test-ci

# Tag HEAD as VERSION (vX.Y.Z, new; tree clean, on origin/main, CI green) and build bin/; the tag's push is printed, not done
release VERSION:
    #!/usr/bin/env bash
    set -euo pipefail
    v="{{VERSION}}"
    [[ "$v" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "release: $v: want vX.Y.Z" >&2; exit 1; }
    git diff --quiet && git diff --cached --quiet || { echo "release: the tree has uncommitted changes" >&2; exit 1; }
    ! git rev-parse -q --verify "refs/tags/$v" >/dev/null || { echo "release: $v exists already" >&2; exit 1; }
    sha=$(git rev-parse HEAD)
    git fetch -q origin main
    git merge-base --is-ancestor "$sha" origin/main || { echo "release: HEAD isn't on origin/main; push it first" >&2; exit 1; }
    status=$(gh run list -b main -c "$sha" -L 1 --json status,conclusion -q '.[0] | "\(.status) \(.conclusion)"')
    [ "$status" = "completed success" ] || { echo "release: CI at ${sha:0:7} is '$status', want 'completed success'" >&2; exit 1; }
    git tag -a "$v" -m "$v"
    just build
    echo "tagged $v at ${sha:0:7}, built into bin/ (bin/icbfs -version says $(bin/icbfs -version))"
    echo "to publish it: git push origin $v"

# --- the VM suites (docs/testing.md) ---

# pjdfstest on the Linux VM
pjdfstest ip=vm_ip:
    test/linux/run-pjdfstest.sh {{ip}}

# The 6 GiB file test on the Linux VM (opt-in, ~10 min)
largefile-test ip=vm_ip:
    test/linux/run-largefile-test.sh {{ip}}

# mount -t fuse.icbfs through the mount helper, on the Linux VM
mount-helper-test ip=vm_ip:
    test/linux/run-mount-helper-test.sh {{ip}}

# xfstests on the Linux VM (default -g quick; e.g. just xfstests -g generic/auto)
xfstests *args:
    test/linux/run-xfstests.sh {{vm_ip}} {{args}}

# winfsp-tests on the Windows VM: icbfs-ci, icbfs-cs or cmemfs-ci
winfsp-tests target="icbfs-ci" *args:
    test/windows/run-winfsp-tests.sh {{target}} {{args}}

# icbfs-sync end to end on the Windows VM
sync-test *args:
    test/windows/run-sync-test.sh {{args}}

# The small VMs the multi-machine tests add to the Linux VM (default 2)
multi-vms n="2":
    test/multi/create-vms.sh {{n}}

# Several machines on one filesystem at once, the Linux VM and the multi VMs (e.g. just multi-test counter)
multi-test *args:
    test/multi/run-multi.sh {{args}}

# Every VM suite but the large-file one and xfstests
vm-tests: pjdfstest mount-helper-test multi-test (winfsp-tests "icbfs-ci") (winfsp-tests "icbfs-cs") sync-test
