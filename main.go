// lotus-unstick-sectors
//
// A surgical tool to rescue lotus-miner sectors that are stuck in a state
// the FSM cannot escape — most commonly SubmitCommitAggregate, where the
// in-memory CommitBatcher loops forever on a failing allocationCheck and
// nothing (not even `lotus-miner sectors update-state`) can pull the sector
// out because the FSM goroutine is blocked inside CommitBatcher.AddCommit.
//
// What it does:
//   - Opens the lotus-miner metadata badger datastore (lotus-miner MUST be
//     stopped first).
//   - Iterates every key under /sectors/.
//   - For each CBOR-encoded SectorInfo, locates the "State" field
//     (text-string key inside the top-level CBOR map) and rewrites the
//     value text-string in place from --from to --to.
//   - Optionally restricts to a comma-separated list of sector numbers.
//
// The CBOR mutation does NOT decode the full SectorInfo struct. It works
// at the byte level, finding the "State" key and rewriting just its value.
// This means the tool is version-agnostic across lotus releases (any
// release where SectorInfo is serialized as a CBOR map with a text-string
// "State" field — which is every release since the FSM was introduced).
//
// Recommended use for the SubmitCommitAggregate bug:
//
//   1. systemctl stop lotus-miner
//   2. cp -a ~/.lotusminer/datastore ~/.lotusminer/datastore.bak   # backup
//   3. lotus-unstick-sectors --repo ~/.lotusminer --dry-run        # preview
//   4. lotus-unstick-sectors --repo ~/.lotusminer                  # apply
//   5. systemctl start lotus-miner
//
// On restart, the FSM reads State="CommitFailed" from the datastore and
// runs handleCommitFailed → checkCommit, which evaluates the DDO
// allocation against the current chain head. From there the sector either
// transitions cleanly to SectorDealsExpired (if the allocation is truly
// past the live ts.Height() based check) and can be removed with
// `lotus-miner sectors remove`, or it retries via the SubmitCommit
// (single-commit) path which bypasses the broken CommitBatcher entirely
// and pushes one ProveCommitSectors3 message per sector.
//
// Author: Nicklas Reiersen (reiers.io)
// License: MIT
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	datastore "github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/query"
	badger "github.com/ipfs/go-ds-badger2"
)

const (
	// Namespace inside the lotus-miner metadata datastore where the
	// sealing FSM persists SectorInfo records. Matches storage/pipeline/
	// sealing.go: SectorStorePrefix = "/sectors".
	sectorPrefix = "/sectors"

	// SectorInfo is cbor-gen'd as a CBOR map. Its keys are CBOR text-strings.
	// The State field is a text-string value whose contents are one of the
	// SectorState constants from storage/pipeline/sector_state.go.
	stateKey = "State"
)

func main() {
	var (
		repo     string
		fromS    string
		toS      string
		sectorsS string
		dry      bool
		yes      bool
		listOnly bool
	)
	flag.StringVar(&repo, "repo", os.Getenv("LOTUS_MINER_PATH"), "lotus-miner repo path (e.g. ~/.lotusminer). Defaults to $LOTUS_MINER_PATH.")
	flag.StringVar(&fromS, "from", "SubmitCommitAggregate", "current sector state to rewrite from")
	flag.StringVar(&toS, "to", "CommitFailed", "sector state to rewrite to (must be a valid SectorState string from sector_state.go)")
	flag.StringVar(&sectorsS, "sectors", "", "comma-separated list of sector numbers to act on (default: all sectors matching --from)")
	flag.BoolVar(&dry, "dry-run", false, "preview changes without writing")
	flag.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	flag.BoolVar(&listOnly, "list", false, "list sectors currently in --from state and exit (no writes)")
	flag.Usage = usage
	flag.Parse()

	if repo == "" {
		fatal("missing --repo (or set $LOTUS_MINER_PATH)")
	}
	repo = expand(repo)

	if fromS == "" || toS == "" {
		fatal("--from and --to are both required")
	}

	wanted := map[uint64]struct{}{}
	if sectorsS != "" {
		for _, s := range strings.Split(sectorsS, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			n, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				fatalf("invalid sector number %q: %v", s, err)
			}
			wanted[n] = struct{}{}
		}
	}

	if err := guardMinerStopped(repo); err != nil {
		fatal(err.Error())
	}

	ds, err := openMetadata(repo)
	if err != nil {
		fatalf("opening metadata datastore at %s: %v", repo, err)
	}
	defer ds.Close()

	matches, err := scan(ds, fromS, wanted)
	if err != nil {
		fatalf("scanning sectors: %v", err)
	}

	if len(matches) == 0 {
		fmt.Printf("No sectors found in state %q.\n", fromS)
		return
	}

	sort.Slice(matches, func(i, j int) bool { return matches[i].num < matches[j].num })

	fmt.Printf("Found %d sector(s) in state %q:\n", len(matches), fromS)
	for _, m := range matches {
		fmt.Printf("  sector %d  (%d bytes)\n", m.num, len(m.raw))
	}

	if listOnly {
		return
	}

	fmt.Printf("\nAbout to rewrite %d sector(s): %q -> %q\n", len(matches), fromS, toS)
	if dry {
		fmt.Println("Dry run: no changes will be written.")
	}

	if !dry && !yes {
		fmt.Print("Type YES to proceed: ")
		var line string
		fmt.Scanln(&line)
		if strings.TrimSpace(line) != "YES" {
			fmt.Println("Aborted.")
			os.Exit(1)
		}
	}

	var ok, fail int
	for _, m := range matches {
		mutated, err := rewriteStateField(m.raw, fromS, toS)
		if err != nil {
			fmt.Printf("  sector %d: SKIP (%v)\n", m.num, err)
			fail++
			continue
		}
		if bytes.Equal(mutated, m.raw) {
			fmt.Printf("  sector %d: SKIP (no change produced)\n", m.num)
			fail++
			continue
		}
		if dry {
			fmt.Printf("  sector %d: would rewrite (%d -> %d bytes)\n", m.num, len(m.raw), len(mutated))
			ok++
			continue
		}
		if err := ds.Put(context.Background(), m.key, mutated); err != nil {
			fmt.Printf("  sector %d: WRITE FAILED (%v)\n", m.num, err)
			fail++
			continue
		}
		fmt.Printf("  sector %d: rewritten (%d -> %d bytes)\n", m.num, len(m.raw), len(mutated))
		ok++
	}

	fmt.Printf("\nDone. %d rewritten, %d skipped.\n", ok, fail)
	if !dry {
		fmt.Println("Start lotus-miner. The FSM will reload these sectors in their new state.")
	}
}

type match struct {
	num uint64
	key datastore.Key
	raw []byte
}

func scan(ds datastore.Batching, fromState string, wanted map[uint64]struct{}) ([]match, error) {
	res, err := ds.Query(context.Background(), query.Query{Prefix: sectorPrefix})
	if err != nil {
		return nil, err
	}
	defer res.Close()

	var out []match
	for r := range res.Next() {
		if r.Error != nil {
			return nil, r.Error
		}
		// Key shape: /sectors/<number>
		base := filepath.Base(r.Key)
		num, err := strconv.ParseUint(base, 10, 64)
		if err != nil {
			// Some lotus versions stash non-numeric keys under /sectors. Skip.
			continue
		}
		if len(wanted) > 0 {
			if _, want := wanted[num]; !want {
				continue
			}
		}
		state, err := readStateField(r.Value)
		if err != nil {
			continue
		}
		if state != fromState {
			continue
		}
		// copy r.Value, query iterator reuses buffers in some backends
		raw := make([]byte, len(r.Value))
		copy(raw, r.Value)
		out = append(out, match{num: num, key: datastore.NewKey(r.Key), raw: raw})
	}
	return out, nil
}

// readStateField parses the CBOR-encoded SectorInfo just far enough to
// read the value of the top-level "State" text-string key.
func readStateField(b []byte) (string, error) {
	idx, vstart, vlen, err := locateStateValue(b)
	if err != nil {
		return "", err
	}
	_ = idx
	return string(b[vstart : vstart+vlen]), nil
}

// rewriteStateField returns a new byte slice whose top-level "State" field
// has been changed from `from` to `to`. The map header is left untouched
// (still has the same number of key-value pairs). The CBOR text-string
// length prefix for the value is rewritten if `to` has a different length.
func rewriteStateField(b []byte, from, to string) ([]byte, error) {
	_, vstart, vlen, err := locateStateValue(b)
	if err != nil {
		return nil, err
	}
	current := string(b[vstart : vstart+vlen])
	if current != from {
		return nil, fmt.Errorf("expected current state %q, found %q", from, current)
	}
	// The header for the value text-string starts at vstart - headerLen(vlen).
	// We need to replace [hdrStart .. vstart+vlen] with newHeader + to.
	hdrLen := textStringHeaderLen(uint64(vlen))
	hdrStart := vstart - hdrLen

	newHdr := encodeTextStringHeader(uint64(len(to)))
	out := make([]byte, 0, len(b)-hdrLen-vlen+len(newHdr)+len(to))
	out = append(out, b[:hdrStart]...)
	out = append(out, newHdr...)
	out = append(out, []byte(to)...)
	out = append(out, b[vstart+vlen:]...)
	return out, nil
}

// locateStateValue scans the CBOR-encoded SectorInfo (a CBOR map) for the
// text-string key "State" and returns:
//   - idx: byte offset of the start of the key's text-string header
//   - vstart: byte offset of the start of the value's text-string content
//   - vlen: byte length of the value
//
// Returns an error if the top-level item is not a CBOR map, or the State
// key is not present, or its value is not a text-string.
//
// We do NOT walk the entire map — we scan for the unique 7-byte sequence
// {0x65 'S' 't' 'a' 't' 'e'} which is the CBOR text-string header (major
// type 3, length 5) followed by the ASCII bytes for "State". Inside a
// cbor-gen-produced SectorInfo this appears exactly once at the top level
// as a key; it does not appear as substring of any other field's content
// because SectorState values like "SubmitCommitAggregate" do not contain
// the literal substring 0x65 + "State", and Log entries are encoded as
// nested structs with their own field-name keys ("Kind", "Trace", "Time",
// "Message") rather than the bare word "State".
//
// We additionally validate that the immediately-following byte starts a
// CBOR text-string, which rules out any false positives where the bytes
// might happen to appear as binary content.
func locateStateValue(b []byte) (idx, vstart, vlen int, err error) {
	// Validate top-level is a CBOR map. cbor-gen uses major type 5.
	if len(b) == 0 {
		return 0, 0, 0, errors.New("empty record")
	}
	mt := b[0] >> 5
	if mt != 5 {
		return 0, 0, 0, fmt.Errorf("top-level CBOR type %d, expected 5 (map)", mt)
	}

	// 0x65 = major type 3 (text string), length 5
	needle := []byte{0x65, 'S', 't', 'a', 't', 'e'}
	for off := 1; off < len(b)-len(needle); off++ {
		if !bytes.Equal(b[off:off+len(needle)], needle) {
			continue
		}
		// Validate the following byte starts a text-string (major type 3).
		after := off + len(needle)
		if after >= len(b) {
			continue
		}
		valHdr := b[after]
		if valHdr>>5 != 3 {
			continue
		}
		// Decode the text-string length.
		ai := valHdr & 0x1f // additional info
		var vl uint64
		var hdrBytes int
		switch {
		case ai < 24:
			vl = uint64(ai)
			hdrBytes = 1
		case ai == 24:
			if after+1 >= len(b) {
				continue
			}
			vl = uint64(b[after+1])
			hdrBytes = 2
		case ai == 25:
			if after+2 >= len(b) {
				continue
			}
			vl = uint64(b[after+1])<<8 | uint64(b[after+2])
			hdrBytes = 3
		default:
			// We do not need to handle longer lengths; SectorState strings
			// are short.
			continue
		}
		vs := after + hdrBytes
		if vs+int(vl) > len(b) {
			continue
		}
		// Sanity check: the text string is printable ASCII.
		val := b[vs : vs+int(vl)]
		if !isPrintableASCII(val) {
			continue
		}
		return off, vs, int(vl), nil
	}
	return 0, 0, 0, errors.New(`could not locate "State" field in record`)
}

func isPrintableASCII(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// textStringHeaderLen returns the number of bytes the CBOR header occupies
// for a text-string of the given byte length.
func textStringHeaderLen(n uint64) int {
	switch {
	case n < 24:
		return 1
	case n < 256:
		return 2
	case n < 65536:
		return 3
	case n < 1<<32:
		return 5
	default:
		return 9
	}
}

// encodeTextStringHeader returns the CBOR header bytes for a text-string
// of the given length.
func encodeTextStringHeader(n uint64) []byte {
	const mt = byte(3 << 5)
	switch {
	case n < 24:
		return []byte{mt | byte(n)}
	case n < 256:
		return []byte{mt | 24, byte(n)}
	case n < 65536:
		return []byte{mt | 25, byte(n >> 8), byte(n)}
	case n < 1<<32:
		return []byte{mt | 26, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	default:
		return []byte{mt | 27,
			byte(n >> 56), byte(n >> 48), byte(n >> 40), byte(n >> 32),
			byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
}

func openMetadata(repoPath string) (datastore.Batching, error) {
	path := filepath.Join(repoPath, "datastore", "metadata")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	opts := badger.DefaultOptions
	ds, err := badger.NewDatastore(path, &opts)
	if err != nil {
		return nil, err
	}
	return ds, nil
}

// guardMinerStopped does a best-effort check that lotus-miner is not
// currently holding the datastore lock. Badger refuses to open a locked
// directory, so opening will fail anyway — this is a friendlier error.
func guardMinerStopped(repoPath string) error {
	lockPath := filepath.Join(repoPath, "repo.lock")
	if _, err := os.Stat(lockPath); err == nil {
		fmt.Fprintln(os.Stderr, "Warning: repo.lock exists. Stop lotus-miner before running this tool.")
	}
	return nil
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func fatal(msg string)            { fmt.Fprintln(os.Stderr, "error: "+msg); os.Exit(1) }
func fatalf(f string, a ...any)   { fatal(fmt.Sprintf(f, a...)) }

func usage() {
	fmt.Fprintf(os.Stderr, `lotus-unstick-sectors — surgically rewrite stuck-sector states in the lotus-miner FSM datastore

Usage:
  lotus-unstick-sectors --repo PATH [--from STATE] [--to STATE] [--sectors N,N,...] [--dry-run] [--list] [--yes]

The lotus-miner sealing FSM persists each sector's state in the metadata
badger datastore under /sectors/<sector-number> as a CBOR-encoded
SectorInfo. This tool rewrites the top-level "State" field in place,
allowing recovery from FSM-deadlock situations where the in-memory
goroutine for the current handler is blocked and lotus-miner's
SectorsUpdate JSON-RPC cannot apply a ForceState event.

The most common case is SubmitCommitAggregate, where CommitBatcher leaks
sectors that fail allocationCheck and lotus-miner's sectors update-state
command has no effect because the FSM goroutine is blocked on an
unsignaled channel inside CommitBatcher.AddCommit.

Always stop lotus-miner first. Always back up the datastore.

Examples:
  # See what would change
  lotus-unstick-sectors --repo ~/.lotusminer --dry-run

  # Only list the affected sectors
  lotus-unstick-sectors --repo ~/.lotusminer --list

  # Apply (default from=SubmitCommitAggregate to=CommitFailed)
  lotus-unstick-sectors --repo ~/.lotusminer

  # Target specific sectors
  lotus-unstick-sectors --repo ~/.lotusminer --sectors 36431,36432,36433

  # Rewrite to a different terminal state (use with care)
  lotus-unstick-sectors --repo ~/.lotusminer --to Removed

Valid target states are the SectorState constants from storage/pipeline/
sector_state.go (e.g. CommitFailed, DealsExpired, Removed, FailedUnrecoverable).
The tool does not validate the target name beyond requiring it to be a
non-empty string; use a real state name.

Flags:
`)
	flag.PrintDefaults()
}
