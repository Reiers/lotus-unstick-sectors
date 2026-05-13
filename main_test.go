package main

import (
	"bytes"
	"testing"
)

// buildFakeSectorInfo builds a minimal CBOR-encoded "SectorInfo-like" map
// with a few text-string keys and values, including "State". This is
// enough to exercise the locate/rewrite logic without depending on the
// real lotus cbor-gen output.
func buildFakeSectorInfo(state string) []byte {
	var buf bytes.Buffer
	// Map with 4 entries (Log, State, SectorNumber, LastErr).
	// Use major type 5 (map), additional info 24 → 1 extra byte for count.
	// Simpler: use ai = 4 → byte 0xa4 (map of 4).
	buf.WriteByte(0xa4)

	// "Log" -> empty array (so we exercise that we don't pick "State"
	// inside log entries by mistake).
	writeTstr(&buf, "Log")
	buf.WriteByte(0x80) // empty array

	// "State" -> state
	writeTstr(&buf, "State")
	writeTstr(&buf, state)

	// "SectorNumber" -> 36431
	writeTstr(&buf, "SectorNumber")
	// CBOR unsigned int, 2-byte: 0x19 0x8e 0x4f
	buf.WriteByte(0x19)
	buf.WriteByte(0x8e)
	buf.WriteByte(0x4f)

	// "LastErr" -> some bytes that include the *substring* "State" but
	// preceded by a different byte so our locator must skip it.
	writeTstr(&buf, "LastErr")
	writeTstr(&buf, "some State junk")
	return buf.Bytes()
}

func writeTstr(buf *bytes.Buffer, s string) {
	n := len(s)
	switch {
	case n < 24:
		buf.WriteByte(byte(3<<5) | byte(n))
	case n < 256:
		buf.WriteByte(byte(3<<5) | 24)
		buf.WriteByte(byte(n))
	default:
		panic("test helper: string too long")
	}
	buf.WriteString(s)
}

func TestReadStateField(t *testing.T) {
	rec := buildFakeSectorInfo("SubmitCommitAggregate")
	got, err := readStateField(rec)
	if err != nil {
		t.Fatalf("readStateField: %v", err)
	}
	if got != "SubmitCommitAggregate" {
		t.Fatalf("got %q, want %q", got, "SubmitCommitAggregate")
	}
}

func TestRewriteStateField_LongerToShorter(t *testing.T) {
	rec := buildFakeSectorInfo("SubmitCommitAggregate")
	out, err := rewriteStateField(rec, "SubmitCommitAggregate", "CommitFailed")
	if err != nil {
		t.Fatalf("rewriteStateField: %v", err)
	}
	got, err := readStateField(out)
	if err != nil {
		t.Fatalf("readStateField after rewrite: %v", err)
	}
	if got != "CommitFailed" {
		t.Fatalf("got %q, want %q", got, "CommitFailed")
	}
	// Size should shrink by exactly the difference (both lengths < 24, so
	// header is 1 byte each — only the content size changes).
	delta := len(rec) - len(out)
	want := len("SubmitCommitAggregate") - len("CommitFailed")
	if delta != want {
		t.Fatalf("size delta %d, want %d", delta, want)
	}
}

func TestRewriteStateField_ShorterToLonger(t *testing.T) {
	rec := buildFakeSectorInfo("Proving")
	out, err := rewriteStateField(rec, "Proving", "SubmitCommitAggregate")
	if err != nil {
		t.Fatalf("rewriteStateField: %v", err)
	}
	got, err := readStateField(out)
	if err != nil {
		t.Fatalf("readStateField after rewrite: %v", err)
	}
	if got != "SubmitCommitAggregate" {
		t.Fatalf("got %q, want %q", got, "SubmitCommitAggregate")
	}
}

func TestRewriteStateField_CrossesHeaderBoundary(t *testing.T) {
	// Going from a short string (<24, 1-byte header) to a long one
	// (>=24, 2-byte header) forces the header to grow. Verify that.
	rec := buildFakeSectorInfo("X")
	long := "ABCDEFGHIJKLMNOPQRSTUVWXY" // 25 chars, needs 2-byte header
	out, err := rewriteStateField(rec, "X", long)
	if err != nil {
		t.Fatalf("rewriteStateField: %v", err)
	}
	got, err := readStateField(out)
	if err != nil {
		t.Fatalf("readStateField after rewrite: %v", err)
	}
	if got != long {
		t.Fatalf("got %q, want %q", got, long)
	}
}

func TestRewriteStateField_RejectsMismatch(t *testing.T) {
	rec := buildFakeSectorInfo("Proving")
	_, err := rewriteStateField(rec, "SubmitCommitAggregate", "CommitFailed")
	if err == nil {
		t.Fatalf("expected error on state mismatch")
	}
}

func TestLocator_SkipsSubstringInOtherFields(t *testing.T) {
	// Our fake record has "some State junk" as a LastErr value. The
	// locator must not match that.
	rec := buildFakeSectorInfo("Proving")
	got, err := readStateField(rec)
	if err != nil {
		t.Fatalf("readStateField: %v", err)
	}
	if got != "Proving" {
		t.Fatalf("got %q, want Proving — locator picked the wrong occurrence", got)
	}
}

func TestLocator_RejectsNonMapTopLevel(t *testing.T) {
	rec := []byte{0x80} // empty array
	if _, err := readStateField(rec); err == nil {
		t.Fatalf("expected error for non-map top level")
	}
}
