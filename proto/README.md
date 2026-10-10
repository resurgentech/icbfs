# Protobuf sources

`.proto` files here define the wire format for this project's own
object bodies (directory/root blocks, `.metadata`, and — once built —
`.lock` and the master block). Generated Go code lives in
`internal/pb/` and is **checked into git**: `go build`/`go test` never
need `protoc` installed, only regenerating after a schema change does.

## Regenerating after editing a `.proto` file

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest   # once
export PATH="$PATH:$(go env GOPATH)/bin"                          # once per shell

protoc --go_out=. --go_opt=module=github.com/resurgentech/icbfs \
  proto/icbfs/v1/<the-file-you-changed>.proto
```

Requires `protoc` itself (the compiler binary, not the Go plugin) to
already be installed — this project assumes it's present as a system
package (confirmed via `protoc --version` during development; install
via your OS package manager if it's missing, e.g. `apt install
protobuf-compiler` on Debian/Ubuntu).

Commit the resulting `internal/pb/*.pb.go` changes alongside your
`.proto` edit — they're generated, but they're also the thing everyone
else's `go build` actually depends on, not just a build artifact to
gitignore.
