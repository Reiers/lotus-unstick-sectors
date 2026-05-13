# Technical notes

For developers and lotus contributors. The friendly user-facing docs are in [README.md](README.md).

## The bug

For DDO sectors (Direct Data Onboarding via `PieceActivationManifest` with `VerifiedAllocationKey`), `storage/pipeline/commit_batch.go::allocationCheck` contains:

```go
if precomitInfo.Info.Expiration < ts.Height()+alloc.TermMin {
    return error
}
```

- `precomitInfo.Info.Expiration` is fixed at precommit time.
- `ts.Height()` is the live chain head and grows every epoch.
- `alloc.TermMin` is fixed on the verifreg allocation.

Once the chain advances past `Expiration - TermMin`, the inequality flips from passing to failing, and it never recovers.

In `processBatchV2`, when every sector in the batch fails its check, the function returns `nil, nil` (silent return, no result delivered). The batcher's `run` loop never gets a `CommitBatchRes` to clean up `b.todo` / `b.waiting` / `b.cutoffs` from. The FSM goroutines waiting on `<-sent` are stuck forever, and the batcher reloop spams `getSectorCollateral` log lines every iteration.

## Why `lotus-miner sectors update-state` is a dead end

`SectorsUpdate` JSON-RPC → `Sealing.ForceSectorState` → `s.sectors.Send(id, SectorForceState{state})`.

`SectorForceState.applyGlobal(state *SectorInfo) bool` returns `true`, meaning it would override any normal planner gate.

But the FSM for that sector is using `hashicorp/go-statemachine`, which serializes events per-key. The current handler (`handleSubmitCommitAggregate`) is blocked at:

```go
res, err := m.commiter.AddCommit(ctx.Context(), sector, AggregateInput{...})
```

`AddCommit` blocks on `<-sent`, where `sent` is a buffered channel that only receives when `CommitBatcher.maybeStartBatch` delivers a `CommitBatchRes` for that sector. Since `processBatchV2` returns `nil, nil` when `infos` is empty, no `CommitBatchRes` is ever produced, and the channel is never written.

Force-state events queued via `SectorsUpdate` therefore sit in the per-key event queue indefinitely. They are never processed because the handler never returns.

On miner restart, the same thing happens: FSM reloads `SubmitCommitAggregate` from the datastore, calls `handleSubmitCommitAggregate`, blocks at `AddCommit`. The force-state event from the previous attempt is gone (events are not persisted; only state is).

## The fix

Two things should change in lotus:

1. **`allocationCheck` should not use live `ts.Height()`.** The check needs to be deterministic at precommit time, not flip from valid to invalid as time passes. Compare against `precomitInfo.Info.PreCommitEpoch` (or the sector's activation epoch).

2. **`CommitBatcher` needs eviction logic.** After N consecutive non-retryable failures (allocation gone, allocation expired, size mismatch, provider mismatch), the sector should be evicted from `b.todo` and a `SectorCommitFailed` event delivered through `b.waiting[sn]` so the FSM can advance.

Both are reasonable changes. Neither is invasive.

## Why this tool operates at the CBOR byte level

The persisted `SectorInfo` struct has ~40 fields, many of which are nested cbor-gen types (`SafeSectorPiece`, `storiface.PreCommit1Out`, `storiface.SectorLocation`, etc). A faithful round-trip decoder would require importing the entire lotus tree and pinning to a specific version. That makes the tool fragile.

Instead, the tool operates directly on the CBOR bytes. `SectorInfo` is serialized by cbor-gen as a CBOR major-type-5 map with text-string keys. The `"State"` key is a CBOR text-string of length 5, prefixed by the header byte `0x65` (major type 3, length 5). The full 6-byte sequence `{0x65, 'S', 't', 'a', 't', 'e'}` appears exactly once at the top level of the map.

The locator scans for that 6-byte sequence and validates that the next byte starts another text-string (major type 3). The value is then extracted (or rewritten) at the byte level.

This makes the tool:

- **Version-agnostic.** Works on any lotus release whose persistence format uses cbor-gen on a `SectorInfo` struct with a top-level `State` field. That's every release since the FSM was introduced.
- **Minimal-dependency.** Pulls in `go-datastore` and `go-ds-badger2` only.
- **Inspectable.** A single Go file, ~400 lines, plus tests.

## False-positive risk for the locator

The locator could in principle match `{0x65, 'S','t','a','t','e'}` somewhere inside another field's content. Two guards make this practically impossible:

1. After the candidate match, the next byte must start a CBOR text-string (major type 3). Most binary content in `SectorInfo` (proof bytes, PreCommit1Out, etc.) is encoded as byte-strings (major type 2), not text-strings. So a false match would require a coincidentally well-formed text-string header at exactly the right offset.

2. The decoded text-string content must be printable ASCII. `SectorState` values are always printable ASCII (`Proving`, `CommitFailed`, etc).

Combined, the probability of a false positive is negligible.

For paranoia, a future version could validate by re-running the locator on the rewritten bytes and confirming the offset and length match expectations.

## Datastore layout

- Repo path: `~/.lotusminer` (or wherever `LOTUS_MINER_PATH` points).
- Metadata DB: `~/.lotusminer/datastore/metadata` (badger v2).
- Namespace inside the DB: `/sectors` (defined as `SectorStorePrefix` in `storage/pipeline/sealing.go`).
- Key format: `/sectors/<decimal-sector-number>` (from `go-statestore::ToKey`).
- Value: CBOR-encoded `SectorInfo`.

## Tests

`main_test.go` exercises:

- Round-trip of `readStateField` → `rewriteStateField`.
- Header-size transitions (1-byte header → 2-byte header for length crossing 24).
- Substring-collision guard: a `LastErr` field containing the literal text `"some State junk"` does not confuse the locator.
- Rejection of mismatched current state.
- Rejection of non-map top-level CBOR.

Run with `go test ./...`.

## Open questions

- Does this tool need to worry about badger transaction batching? Currently each `ds.Put` is a single write. For typical "8 stuck sectors" scale this is fine. For thousands of stuck sectors it might be worth batching.
- Should the tool also clear `LastErr` / retry counters when it rewrites state? Probably yes for `--to CommitFailed`, so the FSM doesn't immediately bail thinking it's failed too many times. Not currently implemented; the rewrite is `State`-only.

## Contributing

Open issues on https://github.com/Reiers/lotus-unstick-sectors/issues. PRs welcome.
