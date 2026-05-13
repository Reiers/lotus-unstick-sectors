# lotus-unstick-sectors

A small recovery tool for lotus-miner sectors that get permanently wedged inside the sealing FSM.

If you've ever had sectors stuck in `SubmitCommitAggregate` that won't move no matter what you try — `sealing abort` says "No request found", `sectors update-state` does nothing, restarting the miner just brings the loop back, the log spams `getSectorCollateral` every few seconds forever — this is the tool for you.

## What it actually does

The lotus-miner sealing FSM persists each sector's state in a badger datastore at `~/.lotusminer/datastore/metadata` under keys of the form `/sectors/<sector-number>`. Each value is a CBOR-encoded `SectorInfo` struct whose first field is `State` (a text-string like `"SubmitCommitAggregate"`, `"Proving"`, `"CommitFailed"`, etc).

This tool opens the badger datastore directly, finds all sector records currently in a given state (default: `SubmitCommitAggregate`), and rewrites the `State` field in place to a target state (default: `CommitFailed`). It does NOT decode the full `SectorInfo` — it locates the `"State"` key inside the CBOR bytes and rewrites only that value. That makes the tool version-agnostic across lotus releases.

When you restart lotus-miner after running this, the FSM reads the new state and the normal handler for that state runs. For the `SubmitCommitAggregate → CommitFailed` case, `handleCommitFailed` calls `checkCommit` against the live chain. From there the sector either:

- Transitions cleanly to `DealsExpired` (if the DDO allocation is actually past its valid window), so you can finalize cleanup with `lotus-miner sectors remove`.
- Routes back through the retry path to `SubmitCommit` (single-commit), which pushes one `ProveCommitSectors3` message per sector and **bypasses the broken `CommitBatcher`** entirely.

Either outcome ends the infinite loop.

## Why `lotus-miner sectors update-state` doesn't work

When a sector is in `SubmitCommitAggregate`, the FSM handler is sitting inside `CommitBatcher.AddCommit`, which is a blocking call that waits on a channel. The batcher's run loop fails inside `allocationCheck` (it compares the sector's fixed on-chain `Expiration` to a live, growing `ts.Height() + alloc.TermMin`, which monotonically diverges as the chain advances), but on failure it returns `nil, nil` without delivering anything to the per-sector waiting channel. The sector remains in `b.todo`, the FSM goroutine never unblocks, and `SectorForceState` events sit in the event queue forever because the handler never returns.

Restarting the miner doesn't help: on startup the FSM reloads the persisted state from the datastore, sees `SubmitCommitAggregate`, calls `handleSubmitCommitAggregate`, blocks on `AddCommit` again. Same deadlock, every time.

The only way out is to **change the persisted state while the miner is stopped**, so that when the miner restarts the FSM never calls `handleSubmitCommitAggregate` for those sectors at all. That's what this tool does.

## Background — the underlying lotus bug

For DDO sectors (Direct Data Onboarding via `PieceActivationManifest` with `VerifiedAllocationKey`), `storage/pipeline/commit_batch.go::allocationCheck` contains:

```go
if precomitInfo.Info.Expiration < ts.Height()+alloc.TermMin {
    return error
}
```

`precomitInfo.Info.Expiration` is fixed at precommit time. `ts.Height()` is the live chain head and grows every epoch. `alloc.TermMin` is fixed on the verifreg allocation. Once the chain advances past `Expiration - TermMin`, the check flips from passing to failing, and it never recovers.

Then in `processBatchV2`, when every sector in the batch fails its check, the function returns `nil, nil` (silent return, no result delivered), and the batcher's `run` loop never gets a `CommitBatchRes` to clean up `b.todo` / `b.waiting` / `b.cutoffs` from. The FSM goroutines waiting on `<-sent` are stuck forever, and the batcher reloop spams `getSectorCollateral` log lines every iteration.

Two things are wrong here:

1. `allocationCheck` should compare against `precomitInfo.Info.PreCommitEpoch` (or the sector's activation epoch), not live `ts.Height()`. The check needs to be deterministic at precommit time, not flip from valid to invalid as time passes.
2. `b.todo` should evict sectors after N consecutive non-retryable failures and emit a `SectorCommitFailed` event so the FSM can advance.

A reasonable upstream fix touches both. Until that lands, this tool is the surgical escape hatch.

## Build

```bash
go build ./...
```

Produces a single static binary `lotus-unstick-sectors`.

## Use

**Always stop lotus-miner first. Always back up the datastore first.**

```bash
# 1. Stop the miner
systemctl stop lotus-miner

# 2. Back up the metadata datastore
cp -a ~/.lotusminer/datastore ~/.lotusminer/datastore.bak

# 3. See what would change
./lotus-unstick-sectors --repo ~/.lotusminer --list

# 4. Preview the rewrite
./lotus-unstick-sectors --repo ~/.lotusminer --dry-run

# 5. Apply
./lotus-unstick-sectors --repo ~/.lotusminer

# 6. Start the miner
systemctl start lotus-miner
```

After step 6, watch the miner logs. Sectors should move into `CommitFailed`, then either to `DealsExpired` (terminal — `lotus-miner sectors remove --really-do-it <sn>` to finalize) or back through retry into `SubmitCommit` and on-chain.

### Targeting specific sectors

```bash
./lotus-unstick-sectors --repo ~/.lotusminer --sectors 36431,36432,36433
```

### Different state transitions

The defaults (`--from SubmitCommitAggregate --to CommitFailed`) are tuned for the DDO/CommitBatcher deadlock. The tool will rewrite any state to any other state — use with care. Valid target states are the `SectorState` constants from `storage/pipeline/sector_state.go` (`CommitFailed`, `DealsExpired`, `Removed`, `FailedUnrecoverable`, etc).

For the older `UpdateActivating` deadlock (`#8415`), this tool can also unstick those:

```bash
./lotus-unstick-sectors --repo ~/.lotusminer --from UpdateActivating --to ReleaseSectorKey
```

(See the [#8415 thread](https://github.com/filecoin-project/lotus/issues/8415) and `arajasek`'s suggested terminal-friendly transition.)

## Safety

- **Always back up the datastore.** Badger is a key-value store; if you mess up a record you lose the sector's FSM state. The tool only mutates the `State` field byte-for-byte, but a backup costs you a few minutes and a few gigabytes.
- The tool refuses to run without `--repo`. It does NOT auto-detect lotus-miner paths.
- The tool will warn if `repo.lock` exists (i.e. lotus-miner is still running). Badger will then refuse to open the datastore anyway — but if you somehow force it, you can corrupt state. Don't.
- This tool does not touch on-chain state. It only changes how lotus-miner remembers its own FSM progress.

## License

MIT.

## Author

Nicklas Reiersen, [reiers.io](https://reiers.io).
