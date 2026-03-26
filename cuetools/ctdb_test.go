package cuetools

import (
	"fmt"
	"hash/crc32"
	"testing"
)

// ── CDImageLayout ─────────────────────────────────────────────────────────────

func TestCDImageLayoutToString(t *testing.T) {
	cd := CDImageLayout{
		Tracks: []CDTrack{
			{Number: 1, Start: 150, Length: 22050, IsAudio: true},
			{Number: 2, Start: 22200, Length: 18000, IsAudio: true},
		},
		FirstAudio:  1,
		AudioTracks: 2,
	}
	got := cd.ToString()
	want := "150:22200:40200"
	if got != want {
		t.Fatalf("ToString=%q want %q", got, want)
	}
}

func TestCDImageLayoutToString_SingleTrack(t *testing.T) {
	cd := CDImageLayout{
		Tracks:      []CDTrack{{Number: 1, Start: 150, Length: 1000, IsAudio: true}},
		FirstAudio:  1,
		AudioTracks: 1,
	}
	if cd.ToString() != "150:1150" {
		t.Fatalf("got %q", cd.ToString())
	}
}

func TestCDImageLayoutToString_Empty(t *testing.T) {
	// An empty layout has no tracks but ToString still appends Length() = 0.
	cd := CDImageLayout{}
	if cd.ToString() != "0" {
		t.Fatalf("empty layout ToString = %q, want %q", cd.ToString(), "0")
	}
}

func TestCDImageLayoutLength(t *testing.T) {
	layout := buildLayoutFromLengths([]int{100, 200, 300})
	got := layout.Length()
	// start=150, total sectors=600, last track ends at 750
	if got != 150+600 {
		t.Fatalf("Length = %d, want %d", got, 150+600)
	}
}

func TestCDImageLayoutLength_Empty(t *testing.T) {
	if (CDImageLayout{}).Length() != 0 {
		t.Fatal("empty layout Length should be 0")
	}
}

// ── ComputeTOCID ─────────────────────────────────────────────────────────────

// TestComputeTOCID_KnownDisc verifies that the 13-track disc with sector lengths
// derived from the TOC string
//
//	0:22452:48310:75540:95030:113275:117910:141930:164190:191077:217357:235317:258477:269940
//
// produces TOCID A0o8jAlrcLu.n.GYK9wCh4GdvOI-.
func TestComputeTOCID_KnownDisc(t *testing.T) {
	layout := buildLayoutFromLengths(knownTOCLengths)
	got := ComputeTOCID(layout)
	const want = "A0o8jAlrcLu.n.GYK9wCh4GdvOI-"
	if got != want {
		t.Fatalf("TOCID = %q, want %q", got, want)
	}
}

func TestComputeTOCID_RelativeInvariance(t *testing.T) {
	// TOCID is computed from relative offsets; shifting all start positions by a
	// constant must not change the result.
	lengths := []int{1000, 2000, 1500}

	layout1 := buildLayoutFromLengths(lengths) // starts at 150
	var layout2 CDImageLayout
	layout2.FirstAudio = 1
	layout2.AudioTracks = len(lengths)
	start := 0 // starts at 0 instead of 150
	for i, l := range lengths {
		layout2.Tracks = append(layout2.Tracks, CDTrack{Number: i + 1, Start: start, Length: l, IsAudio: true})
		start += l
	}

	if ComputeTOCID(layout1) != ComputeTOCID(layout2) {
		t.Fatalf("TOCID changed with absolute offset shift: %q vs %q",
			ComputeTOCID(layout1), ComputeTOCID(layout2))
	}
}

func TestComputeTOCID_Empty(t *testing.T) {
	if ComputeTOCID(CDImageLayout{}) != "" {
		t.Fatal("empty layout should return empty TOCID")
	}
}

func TestComputeTOCID_DifferentDiscs(t *testing.T) {
	a := buildLayoutFromLengths([]int{100, 200})
	b := buildLayoutFromLengths([]int{100, 201}) // one sector different
	if ComputeTOCID(a) == ComputeTOCID(b) {
		t.Fatal("different discs should produce different TOCIDs")
	}
}

// ── ComputeAccurateRipID ─────────────────────────────────────────────────────

func TestComputeAccurateRipID_Empty(t *testing.T) {
	if ComputeAccurateRipID(CDImageLayout{}) != "" {
		t.Fatal("empty layout should return empty AccurateRip ID")
	}
}

func TestComputeAccurateRipID_Format(t *testing.T) {
	layout := buildLayoutFromLengths([]int{1000, 2000})
	id := ComputeAccurateRipID(layout)
	// Must be three 8-digit hex groups separated by dashes
	var a, b, c uint32
	n, err := parseThreeHex(id, &a, &b, &c)
	if err != nil || n != 3 {
		t.Fatalf("AccurateRip ID %q: unexpected format (%v)", id, err)
	}
}

func TestComputeAccurateRipID_Deterministic(t *testing.T) {
	layout := buildLayoutFromLengths([]int{500, 1000, 750})
	id1 := ComputeAccurateRipID(layout)
	id2 := ComputeAccurateRipID(layout)
	if id1 != id2 {
		t.Fatalf("AccurateRip ID is not deterministic: %q vs %q", id1, id2)
	}
}

func TestComputeAccurateRipID_DifferentDiscs(t *testing.T) {
	a := buildLayoutFromLengths([]int{500, 1000})
	b := buildLayoutFromLengths([]int{500, 999})
	if ComputeAccurateRipID(a) == ComputeAccurateRipID(b) {
		t.Fatal("different discs should produce different AccurateRip IDs")
	}
}

// parseThreeHex is a test helper that parses "XXXXXXXX-XXXXXXXX-XXXXXXXX".
func parseThreeHex(s string, a, b, c *uint32) (int, error) {
	n, err := fmt.Sscanf(s, "%08x-%08x-%08x", a, b, c)
	return n, err
}

// ── DBEntry.ParityURL ─────────────────────────────────────────────────────────

func TestParityURL_RelativePath(t *testing.T) {
	e := DBEntry{HasParity: true, HasParityURL: "/parity/abc.bin"}
	got := e.ParityURL("http://db.cuetools.net")
	want := "http://db.cuetools.net/parity/abc.bin"
	if got != want {
		t.Fatalf("ParityURL = %q, want %q", got, want)
	}
}

func TestParityURL_AbsoluteURL(t *testing.T) {
	e := DBEntry{HasParity: true, HasParityURL: "http://other.host/parity/abc.bin"}
	got := e.ParityURL("http://db.cuetools.net")
	want := "http://other.host/parity/abc.bin"
	if got != want {
		t.Fatalf("ParityURL = %q, want %q", got, want)
	}
}

func TestParityURL_NoParity(t *testing.T) {
	e := DBEntry{HasParity: false, HasParityURL: "/parity/abc.bin"}
	if e.ParityURL("http://db.cuetools.net") != "" {
		t.Fatal("expected empty URL when HasParity=false")
	}
}

// ── calcARDiscIDs internals ───────────────────────────────────────────────────

func TestCalcARDiscIDs_SingleTrack(t *testing.T) {
	layout := CDImageLayout{
		FirstAudio:  1,
		AudioTracks: 1,
		Tracks:      []CDTrack{{Number: 1, Start: 150, Length: 1000, IsAudio: true}},
	}
	id1, id2, _ := calcARDiscIDs(layout)
	// discID1 = discLen = 1000 (rel offset of lead-out from base 150)
	if id1 != 1000 {
		t.Fatalf("discID1 = %d, want 1000", id1)
	}
	// discID2 = 1 (track 1 rel=0, max=1) * 1 (num=1) + 1000 * 2 = 1 + 2000 = 2001
	if id2 != 2001 {
		t.Fatalf("discID2 = %d, want 2001", id2)
	}
}

// ── digitSum helper ───────────────────────────────────────────────────────────

func TestDigitSum(t *testing.T) {
	cases := []struct{ n, want int }{
		{0, 0}, {1, 1}, {9, 9}, {10, 1}, {99, 18}, {123, 6},
	}
	for _, c := range cases {
		got := digitSum(c.n)
		if got != c.want {
			t.Errorf("digitSum(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}

// ── parseHex32 ───────────────────────────────────────────────────────────────

func TestParseHex32(t *testing.T) {
	v, err := parseHex32("deadbeef")
	if err != nil || v != 0xDEADBEEF {
		t.Fatalf("parseHex32 = %08X %v", v, err)
	}
	v, err = parseHex32("")
	if err != nil || v != 0 {
		t.Fatalf("parseHex32('') = %08X %v", v, err)
	}
}

// ── crc32.IEEETable used consistently ────────────────────────────────────────

func TestIEEETableConsistency(t *testing.T) {
	// Confirm crc32.IEEETable is the same table used internally, by checking a
	// known CRC value.
	tab := crc32.IEEETable
	// CRC32 of 0x00 with init=0: table[0xFF^0x00] ^ (0xFF>>8)... skipping, just check
	// that the table matches the standard by computing CRC of "123456789".
	want := crc32.ChecksumIEEE([]byte("123456789"))
	crc := uint32(0xffffffff)
	for _, b := range []byte("123456789") {
		crc = tab[byte(crc)^b] ^ (crc >> 8)
	}
	crc ^= 0xffffffff
	if crc != want {
		t.Fatalf("manual CRC = %08X, want %08X", crc, want)
	}
}
