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

# icbfs-winfsp and icbfs-sync are windowed programs: no console window
# when they start at sign-in; from a prompt, they write to its console.
build-windows:
    GOOS=windows go build -o bin/ ./cmd/icbfs-cli
    GOOS=windows go build -ldflags -H=windowsgui -o bin/icbfs-winfsp.exe ./cmd/icbfs-winfsp
    GOOS=windows go build -ldflags -H=windowsgui -o bin/icbfs-sync.exe ./cmd/icbfs-sync

# The Windows package into dist/, signed, MSIs built on the Windows VM: HEAD's tag from a clean checkout, or this tree as X.Y.Z (ICBFS_CODESIGN=none: unsigned)
package *version:
    packaging/windows/package.sh {{version}}

# That package's install, checks, repair and uninstall on the Windows VM (packaging/windows/check-msi.ps1)
package-test *version:
    packaging/windows/package-test.sh {{version}}

# Every Go test (needs Docker, and /dev/fuse for the mount tests)
test *args:
    go test {{args}} ./...

# The Go tests without the code-generator check
test-short *args:
    go test -short {{args}} ./...

# The -timeout is explicit because Go cuts a test package at 10 minutes by default, and internal/icbfs takes 5 on a quiet machine.
# pkg/drive/icbfsdrive took 7 to 12 minutes alone and internal/icbfs 6 (2026-11-17), so scripts/split-gotest.sh runs their tests in processes split by name (six and two: in icbfsdrive the settle's tests are mostly TestSettle* and TestST*, a process each; the last of each takes every test the others do not), beside the rest, and fails unless each test go test -list names ran exactly once.
# The Go tests CI runs, with the race detector
test-ci:
    #!/usr/bin/env bash
    set -uo pipefail
    d=github.com/resurgentech/icbfs/pkg/drive/icbfsdrive i=github.com/resurgentech/icbfs/internal/icbfs
    go test -race -timeout 25m $(go list ./... | grep -vx -e "$d" -e "$i") & p1=$!
    scripts/split-gotest.sh ./pkg/drive/icbfsdrive '^Test[A-J]' '^Test[KL]' '^TestM' '^TestS[a-z]' '^TestS[A-Z]' & p2=$!
    scripts/split-gotest.sh ./internal/icbfs '^Test[A-N]' & p3=$!
    rc=0
    for p in $p1 $p2 $p3; do wait $p || rc=1; done
    exit $rc

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

# EraseAll at 100,000 entries against fakes of S3 and Azure, holding one page: alone, after test-ci, so its load does not slow the timing tests
test-erase-full:
    ICBFS_ERASE_FULL=1 go test -race -count=1 -run 'TestEraseAllHoldsOnePage' ./internal/objstore/s3store ./internal/objstore/azurestore

# What CI checks: while working and before a commit; under ten minutes on a lab runner (docs/testing.md, "Which gate when"). The long tests are in pre-merge.
ci: fmt-check tidy-check vet build test-ci

# ci, then every long test ci leaves out (ICBFS_PRE_MERGE=1, the 100,000-entry erase, the timing test alone, a dated write killed at every request and dated at the next start: about two minutes; T83-8's views compared with the base's recorded views: about a minute), then the docs/STATES.md rows the tests prove: once, before a merge that touches icbfs
pre-merge: ci test-erase-full test-settle test-settle-review
    ICBFS_PRE_MERGE=1 go test -race -count=1 -timeout 30m -run '^(TestTreeMemory|TestZZFanout)$' ./pkg/drive/icbfsdrive ./internal/icbfs
    ICBFS_PRE_MERGE=1 go test -race -count=1 -timeout 30m -run '^(TestBound16_BatchesInOrderSettleAsOneCall|TestModelTwoLevels|TestST27_AnotherWriterChangesOnlyTheRecordDuringTheReads)$' ./pkg/drive/icbfsdrive
    ICBFS_PRE_MERGE=1 go test -count=1 -timeout 30m -run '^TestT838_NoViewDiffers$' ./pkg/drive/icbfsdrive
    ICBFS_PRE_MERGE=1 go test -race -count=1 -run '^TestMountSmallWritesAreQuick$' ./internal/fuseserver
    ICBFS_PRE_MERGE=1 go test -count=1 -timeout 30m -run '^TestIT28_DatedWriteKilledIsDatedAtTheNextStart$' ./pkg/drive/icbfsdrive
    @just rows

# The docs/STATES.md row ids this module's tests carry: in a test's name (TestLF4_..., TestPL2_PL3_...) or a // STATES: comment beside it
rows:
    @{ { grep -rhoE 'func Test([A-Z]{2,3}[0-9]+_)+' --include='*_test.go' cmd internal pkg || true; } | sed -E 's/^func Test//; s/_/\n/g'; { grep -rhE '//[[:space:]]*STATES:' --include='*_test.go' cmd internal pkg || true; } | { grep -oE '[A-Z]{2,3}-[0-9]+' || true; } | tr -d '-'; } | { grep -E '^[A-Z]{2,3}[0-9]+$' || true; } | sed -E 's/^([A-Z]+)([0-9]+)$/\1-\2/' | sort -uV | awk '{ r = r " " $0; k++ } END { print k+0 " docs/STATES.md row ids carried by this module'"'"'s tests:" r }'

# T83-1's settle tests run without -race (too long under it, and heap
# measured without it): the request and heap gates, the cut sweeps, the
# killed-write sweep, the round-4 review's model (two seeds at 4 entries a
# block, one at 128, with moves, renames, restores, child folders and the
# folders' own records)
test-settle:
    go test -count=1 -timeout 40m -run 'TestSettleRequestsBoundedByLaterStates|TestSettleRequestsBeyondOnePage|TestSettleCutAtEveryRequest' ./pkg/drive/icbfsdrive
    T83_ALWAYS=1 RV4_CUT=plain go test -count=1 -timeout 40m -run 'TestKilledWriteLeavesTheFolderSettleable|TestRV4Cut$' ./pkg/drive/icbfsdrive
    RV4_SEEDS=4229-4230 RV4_K=3 RV4_ENTRIES=4 RV4_OPS=120 go test -count=1 -timeout 30m -run 'TestRV4Model$' ./pkg/drive/icbfsdrive
    RV4_SEEDS=5201 RV4_K=3 RV4_ENTRIES=0 RV4_N0=300 RV4_OPS=150 go test -count=1 -timeout 40m -run 'TestRV4Model$' ./pkg/drive/icbfsdrive

# T83-1's slow settle checks, not in ci (about an hour; before a change to the settle merges): an immediate stop at every request of a settle (one folder, then a split one), and the mutations, each of which must turn its row's test red (test/settle-mutations/run.sh)
test-settle-review:
    ICBFS_PRE_MERGE=1 go test -count=1 -timeout 30m -run 'TestST9_ImmediateStopAtEveryRequest$' ./pkg/drive/icbfsdrive
    ICBFS_PRE_MERGE=1 FR_CASE=split go test -count=1 -timeout 50m -run 'TestST9_ImmediateStopAtEveryRequest$' ./pkg/drive/icbfsdrive
    test/settle-mutations/run.sh

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

# What a Windows mount does with names Windows can't hold, on the Windows VM (prints a table; asserts nothing yet)
names-test *args:
    test/windows/run-names-test.sh {{args}}

# The small VMs the multi-machine tests add to the Linux VM (default 2)
multi-vms n="2":
    test/multi/create-vms.sh {{n}}

# Several machines on one filesystem at once, the Linux VM and the multi VMs (e.g. just multi-test counter)
multi-test *args:
    test/multi/run-multi.sh {{args}}

# Every VM suite but the large-file one and xfstests
vm-tests: pjdfstest mount-helper-test multi-test (winfsp-tests "icbfs-ci") (winfsp-tests "icbfs-cs") sync-test
