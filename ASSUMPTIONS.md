# Assumptions made during autonomous, unsupervised work

Each entry: the question I would have asked, what I assumed instead, why,
and what to check. Written so each one can be confirmed or overridden
after the fact without having to re-derive the context.

---

## A1/A2: Protobuf file layout, checked-in generated code, wrapped types

**Question I'd have asked:** where should `.proto` files live, should
generated code be committed, and should generated Protobuf types be the
public API of `internal/block`/`internal/icbfs`, or wrapped?

**Assumed:**
- `.proto` sources live under `/proto/icbfs/v1/*.proto`.
- Generated `.pb.go` files are **checked into git**, in their own package
  `internal/pb`, imported by whatever package needs them. Rationale:
  `go build`/`go test` keep working for anyone without `protoc` installed
  locally — matches this project's existing low-dependency-friction
  pattern (e.g. `internal/block`'s current JSON approach needs nothing
  extra to build).
- Generated types are **not** exposed as the public API of `internal/block`
  or `internal/icbfs` — those packages keep their existing hand-written
  types (`block.Block`, `block.Entry`, `icbfs.Attr`, etc.) and convert
  to/from the generated types only inside `Encode`/`Decode`. Rationale:
  callers throughout the codebase (and anyone building on these packages
  later) shouldn't need to know the wire format changed; this also means
  if Protobuf is ever swapped for something else again, it's a one-file
  change, not a propagating one.

**Check this if:** a different contributor has a strong existing
convention for where generated Go protobuf code lives in their other
projects — this is a style choice, not a correctness one, and easy to
move later since nothing outside `internal/pb` and each package's
`Encode`/`Decode` would need to change.

---
