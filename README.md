# lotus-unstick-sectors

A small recovery tool for **lotus-miner** when sectors get permanently stuck in the sealing pipeline and nothing you try will move them.

> ⚠️ **Important:** This tool is provided as-is. It has NOT been tested in production. It edits the lotus-miner database directly. **Always back up your datastore before running it.** Use at your own risk.

---

## What is this for?

If you've ever seen something like this:

- `lotus-miner info` shows sectors stuck in `SubmitCommitAggregate` (or `UpdateActivating`) for hours, days, or weeks.
- `lotus-miner sectors batching commit` says "ERROR: no sectors to publish".
- `lotus-miner sealing abort --sched <uuid>` says "No request with provided details found".
- `lotus-miner sectors update-state` does nothing — the sector stays in the same state after a miner restart.
- Miner logs spam `getSectorCollateral` lines every few seconds forever.

…then those sectors are wedged inside the lotus-miner sealing state machine in a way that the normal recovery commands can't fix. This tool is a manual escape hatch for that situation.

## What does it do, in plain language?

lotus-miner keeps a little database on disk that remembers what state each sector is in (`Proving`, `Sealing`, `CommitFailed`, etc.). Normally lotus-miner is the only thing that writes to that database.

This tool **stops lotus-miner**, opens that database, finds the stuck sectors, changes their state to a state lotus-miner knows how to handle (`CommitFailed`), then lets you start lotus-miner back up. When lotus-miner restarts, it sees the new state, runs its normal "this commit failed, let me check what's wrong" code, and either:

- finishes cleaning up the sector if the deal is truly expired, or
- pushes the sector through a different code path that bypasses the broken loop.

Either way, the sector finally moves.

## Will this damage my other sectors?

**No, this tool only touches the sectors you tell it to.**

By default it only changes sectors currently in `SubmitCommitAggregate`. If you want extra safety, you can pass a specific list of sector numbers with `--sectors 36431,36432,36433` and the tool will only touch exactly those sectors. Every other sector — your proving sectors, your sealing sectors, everything — is left completely alone.

The tool also never touches the chain or any on-chain state. It only changes how lotus-miner remembers its own progress on disk.

**That said:** the tool edits a database. Edits to databases can go wrong. **Always back up your datastore before running.** The tool has unit tests but no production deployment has been done with it yet.

## Will this work for me?

Honestly, we don't know for sure. The logic is based on reading the lotus source code carefully, and we believe it works, but we haven't run it against a real production lotus-miner with stuck sectors. The point of this tool is that without it, your stuck sectors stay stuck forever, so trying it (with a backup) is usually better than leaving them.

If it doesn't work for you, restore the backup of your datastore and you're back where you started.

---

## How to install

You need:

1. A Linux machine (the one running lotus-miner).
2. **Go 1.22 or newer** installed.

### Step 1 — Install Go (if you don't have it)

```bash
# On Ubuntu/Debian
sudo apt update
sudo apt install -y golang-go

# Check it's at least 1.22
go version
```

If `go version` shows something older than 1.22, get the newer version from https://go.dev/dl/ — pick the linux-amd64 tarball, follow the install instructions there.

### Step 2 — Get the tool

```bash
git clone https://github.com/Reiers/lotus-unstick-sectors.git
cd lotus-unstick-sectors
```

### Step 3 — Build it

```bash
go build -o lotus-unstick-sectors .
```

This produces a single file called `lotus-unstick-sectors` in the current directory. That's the whole tool. No installation, no system files modified.

### Step 4 — Run the tests (optional but recommended)

```bash
go test ./...
```

You should see `ok` at the end. If tests fail, do NOT use the tool — open a GitHub issue.

---

## How to use it (full safe procedure)

> ⚠️ Do every step. Do not skip the backup.

### Step 1 — Stop lotus-miner

```bash
sudo systemctl stop lotus-miner
```

If you don't run lotus-miner under systemd, stop it however you usually do. Make sure the lotus-miner process is fully gone:

```bash
ps -ef | grep lotus-miner | grep -v grep
```

Should show nothing.

### Step 2 — Back up your datastore

The datastore is inside your lotus-miner repo, usually at `~/.lotusminer`. If you use a different path, replace `~/.lotusminer` everywhere below.

```bash
cp -a ~/.lotusminer/datastore ~/.lotusminer/datastore.bak
```

This copies the entire datastore to a backup folder. If anything goes wrong, you can put it back with:

```bash
rm -rf ~/.lotusminer/datastore
mv ~/.lotusminer/datastore.bak ~/.lotusminer/datastore
```

**Do not skip this step.**

### Step 3 — List the stuck sectors

```bash
./lotus-unstick-sectors --repo ~/.lotusminer --list
```

You should see something like:

```
Found 8 sector(s) in state "SubmitCommitAggregate":
  sector 36431  (4123 bytes)
  sector 36432  (4156 bytes)
  ...
```

If the list looks right (matches the sectors you actually want to unstick), continue. If it shows sectors you DON'T want touched, stop and use `--sectors` (next section) to target only the ones you want.

### Step 4 — Preview what will change

```bash
./lotus-unstick-sectors --repo ~/.lotusminer --dry-run
```

Shows what the tool would do without writing anything.

### Step 5 — Apply the change

```bash
./lotus-unstick-sectors --repo ~/.lotusminer
```

The tool will list the sectors again and ask you to type `YES` to proceed. Type `YES` and press enter.

### Step 6 — Start lotus-miner

```bash
sudo systemctl start lotus-miner
```

### Step 7 — Watch what happens

Give it a couple of minutes, then check the affected sectors:

```bash
lotus-miner sectors status 36431
```

You should see the sector has moved out of `SubmitCommitAggregate` to `CommitFailed`, then either onward to `DealsExpired` (terminal — clean it up with `lotus-miner sectors remove --really-do-it 36431`) or back through retry to a state that actually pushes the commit on chain.

If the sector is in `DealsExpired`, the on-chain allocation for that piece is actually past its valid window. The data the SP stored cannot be onboarded anymore. That's not a tool failure, that's a state-of-the-world fact — but at least the sector is no longer wedged eating CPU cycles, and you can finish cleanup.

---

## Targeting specific sectors

If you only want to touch certain sector numbers (recommended when you're not sure):

```bash
./lotus-unstick-sectors --repo ~/.lotusminer --sectors 36431,36432,36433 --dry-run
./lotus-unstick-sectors --repo ~/.lotusminer --sectors 36431,36432,36433
```

Only those sectors will be touched. Everything else in `SubmitCommitAggregate` (or any other state) is left alone.

## For the older `UpdateActivating` stuck-sector bug

There's a separate, older bug ([lotus#8415](https://github.com/filecoin-project/lotus/issues/8415)) where sectors get stuck in `UpdateActivating` after a chain prune or node outage. This tool can also help there:

```bash
./lotus-unstick-sectors --repo ~/.lotusminer --from UpdateActivating --to ReleaseSectorKey
```

(That's the terminal-friendly transition suggested by `arajasek` in that thread.)

---

## If something goes wrong

Restore the backup you made in step 2:

```bash
sudo systemctl stop lotus-miner
rm -rf ~/.lotusminer/datastore
mv ~/.lotusminer/datastore.bak ~/.lotusminer/datastore
sudo systemctl start lotus-miner
```

You're back to exactly the state before you ran the tool.

Open a GitHub issue at https://github.com/Reiers/lotus-unstick-sectors/issues with:

- Your lotus-miner version (`lotus-miner version`)
- The output of `./lotus-unstick-sectors --repo ~/.lotusminer --list`
- The error you got

---

## What this tool will NOT fix

- Sectors that are valid but slow. If your sector is sealing normally, just slowly, leave it alone.
- On-chain problems. If your miner has a wallet/funds issue, this tool can't help.
- Lost data. If the actual sealed sector files are gone from disk, no state-machine surgery brings them back.
- Anything in Curio. This is for legacy lotus-miner only.

## Why doesn't `lotus-miner sectors update-state` work for the `SubmitCommitAggregate` case?

The short answer: lotus-miner's `update-state` command sends an event to the in-memory state machine, but the state machine for that sector is **frozen** inside another function (`CommitBatcher.AddCommit`) waiting on a signal that never arrives. So the event sits in a queue forever and never gets applied. Restarting lotus-miner brings the sector back into the same frozen state because the on-disk record still says `SubmitCommitAggregate`.

The fix is to change the on-disk record while lotus-miner is stopped, so that when it starts up again, the sector enters a different state and a different (non-frozen) code path. That's all this tool does.

The deeper technical explanation is in [`TECHNICAL.md`](TECHNICAL.md) if you're a developer and curious.

---

## License

MIT. See `LICENSE`.

## Author

Nicklas Reiersen — [reiers.io](https://reiers.io)

If this tool helps you, a thank-you on the Filecoin Slack is appreciated. If it doesn't help you, open an issue.

This is community software. Not affiliated with Protocol Labs, FilOzone, or any organization.
