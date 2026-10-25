# Protobuf sources

`.proto` files here define the wire format of the objects icbfs encodes
itself: directory blocks (`block.proto`), `.metadata` (`metadata.proto`),
`.lock` (`lock.proto`), the master block (`master.proto`), and trash
entries, named versions and origin records (`records.proto`). Generated Go
code lives in `internal/pb/` and is **checked into git**: `go build` and
`go test` never need `protoc` installed, only regenerating after a schema
change does.

## Regenerating after editing a `.proto` file

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12   # once
export PATH="$PATH:$(go env GOPATH)/bin"                            # once per shell

protoc --go_out=. --go_opt=module=github.com/resurgentech/icbfs \
  spec/proto/icbfs/v1/<the-file-you-changed>.proto
```

This needs `protoc` itself, the compiler, from your OS's packages
(`apt install protobuf-compiler` on Debian and Ubuntu). `just generate`
runs it for every file.

Commit the regenerated `internal/pb/*.pb.go` with the `.proto` change:
they're generated, but everyone else's `go build` depends on them.
