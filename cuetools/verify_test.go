package cuetools

import (
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	flacenc "github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// buildLayoutFromLengths builds a CDImageLayout with start=150 from sector lengths.
func buildLayoutFromLengths(trackLengths []int) CDImageLayout {
	layout := CDImageLayout{FirstAudio: 1, AudioTracks: len(trackLengths)}
	start := 150
	for i, l := range trackLengths {
		layout.Tracks = append(layout.Tracks, CDTrack{Number: i + 1, Start: start, Length: l, IsAudio: true})
		start += l
	}
	return layout
}

// writeFakeFLAC writes a minimal stereo 16-bit 44100 Hz FLAC with the given
// number of sectors (sectors*588 samples per channel). Each frame uses
// PredConstant encoding with a deterministic sample value derived from seed,
// making the audio cheap to encode while remaining non-silent and track-unique.
func writeFakeFLAC(t *testing.T, path string, sectors int, seed int64) {
	t.Helper()
	const (
		sampleRate    = 44100
		bitsPerSample = 16
		blockSize     = 588 // 588 = one CD sector; sectors*588 is always divisible so no partial last frame.
	)
	totalSamples := uint64(sectors * 588)

	si := &meta.StreamInfo{
		BlockSizeMin:  blockSize,
		BlockSizeMax:  blockSize,
		SampleRate:    sampleRate,
		NChannels:     2,
		BitsPerSample: bitsPerSample,
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := flacenc.NewEncoder(f, si)
	if err != nil {
		f.Close()
		t.Fatal(err)
	}

	// Derive non-zero constant sample values from seed so each track is unique.
	lVal := int32((seed & 0x7FFE) + 1) // positive, 1–32766
	rVal := -lVal                      // negative mirror keeps the track non-silent

	var written uint64
	for written < totalSamples {
		n := uint64(blockSize)
		if written+n > totalSamples {
			n = totalSamples - written
		}
		left := make([]int32, n)
		right := make([]int32, n)
		for i := range left {
			left[i] = lVal
			right[i] = rVal
		}
		fr := &frame.Frame{
			Header: frame.Header{
				HasFixedBlockSize: true, // keeps Num as a small frame counter, avoiding multi-byte UTF-8
				BlockSize:         uint16(n),
				SampleRate:        sampleRate,
				Channels:          frame.ChannelsLR,
				BitsPerSample:     bitsPerSample,
			},
			Subframes: []*frame.Subframe{
				{SubHeader: frame.SubHeader{Pred: frame.PredConstant}, Samples: left, NSamples: int(n)},
				{SubHeader: frame.SubHeader{Pred: frame.PredConstant}, Samples: right, NSamples: int(n)},
			},
		}
		if err := enc.WriteFrame(fr); err != nil {
			enc.Close()
			t.Fatal(err)
		}
		written += n
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
}

// makeFakeFLACDir creates a temp directory populated with one fake FLAC per
// track in trackLengths (named Track01.flac, Track02.flac, …).
func makeFakeFLACDir(t *testing.T, trackLengths []int) string {
	t.Helper()
	dir := t.TempDir()
	for i, sectors := range trackLengths {
		name := filepath.Join(dir, fmt.Sprintf("Track%02d.flac", i+1))
		writeFakeFLAC(t, name, sectors, int64(i+1)*0xDEAD)
	}
	return dir
}

// ── parseTrackNumber ──────────────────────────────────────────────────────────

func TestParseTrackNumber(t *testing.T) {
	cases := map[string]int{
		"01 - track.flac": 1,
		"track02.wav":     2,
		"3 song.flac":     3,
		"track-4.flac":    4,
	}
	for name, want := range cases {
		got, ok := parseTrackNumber(name)
		if !ok || got != want {
			t.Fatalf("parseTrackNumber(%q) = %d ok=%v, want %d", name, got, ok, want)
		}
	}
	_, ok := parseTrackNumber("notracknum.flac")
	if ok {
		t.Fatal("expected false for file with no track number")
	}
}

// ── toInt16 ───────────────────────────────────────────────────────────────────

func TestToInt16(t *testing.T) {
	if toInt16(32767, 16) != 32767 {
		t.Fatal("unexpected 16->16")
	}
	if toInt16(-32768, 16) != -32768 {
		t.Fatal("unexpected negative 16->16")
	}
	var c24 int32 = 32767 << 8
	if toInt16(c24, 24) != 32767 {
		t.Fatal("unexpected 24->16")
	}
	var x int32 = 327
	expected := int16(x << 8)
	if toInt16(327, 8) != expected {
		t.Fatal("unexpected 8->16")
	}
}

// ── ARCRC32 ───────────────────────────────────────────────────────────────────

func TestARCRC32_Empty(t *testing.T) {
	var a ARCRC32
	if a.Sum() != 0 {
		t.Fatalf("empty ARCRC32 sum = %08X, want 0", a.Sum())
	}
}

func TestARCRC32_KnownValues(t *testing.T) {
	// pos=1, sample=1 → prod=1, low+=1 → Sum=1
	var a ARCRC32
	a.AddSample(1)
	if a.Sum() != 1 {
		t.Fatalf("single sample(1) sum = %08X, want 1", a.Sum())
	}
	// pos=2, sample=2 → prod=4, low+=4 → Sum=5
	a.AddSample(2)
	if a.Sum() != 5 {
		t.Fatalf("two-sample sum = %08X, want 5", a.Sum())
	}
	// matches ComputeARTrackCRC32
	got := ComputeARTrackCRC32([]uint32{1, 2})
	if got != 5 {
		t.Fatalf("ComputeARTrackCRC32([1,2]) = %08X, want 5", got)
	}
}

func TestARCRC32_HighProduct(t *testing.T) {
	// sample=0xFFFFFFFF, pos=1 → prod=0xFFFFFFFF
	// uint32(prod)=0xFFFFFFFF, uint32(prod>>32)=0 → Sum=0xFFFFFFFF
	var a ARCRC32
	a.AddSample(0xFFFFFFFF)
	if a.Sum() != 0xFFFFFFFF {
		t.Fatalf("got %08X, want FFFFFFFF", a.Sum())
	}
	// pos=2, sample=0x80000001 → prod=0x80000001*2=0x100000002
	// uint32(prod)=0x00000002, uint32(prod>>32)=0x00000001 → low+=2, high+=1
	// Sum = (0xFFFFFFFF + 2) + 1 = 0x100000002 → uint32 = 0x00000002
	a.AddSample(0x80000001)
	if a.Sum() != 0x00000002 {
		t.Fatalf("overflow sum = %08X, want 00000002", a.Sum())
	}
}

// ── computeCTDBCRC32 ─────────────────────────────────────────────────────────

func TestComputeCTDBCRC32_MatchesStdlib(t *testing.T) {
	samples := []uint32{0x01020304, 0xDEADBEEF, 0x00000000, 0xFFFFFFFF}
	// The fast path must match encoding as little-endian bytes through stdlib
	h := crc32.NewIEEE()
	for _, s := range samples {
		h.Write([]byte{byte(s), byte(s >> 8), byte(s >> 16), byte(s >> 24)})
	}
	want := h.Sum32()
	got := computeCTDBCRC32(samples)
	if got != want {
		t.Fatalf("computeCTDBCRC32 = %08X, want %08X", got, want)
	}
}

func TestComputeCTDBCRC32_Empty(t *testing.T) {
	got := computeCTDBCRC32(nil)
	// CRC32 of empty input with init=0xFFFFFFFF and final XOR = 0xFFFFFFFF → 0
	if got != 0 {
		t.Fatalf("empty CRC = %08X, want 0", got)
	}
}

func TestComputeCTDBCRC32_SingleSample(t *testing.T) {
	s := uint32(0x01000000)
	got := computeCTDBCRC32([]uint32{s})
	want := crc32.ChecksumIEEE([]byte{byte(s), byte(s >> 8), byte(s >> 16), byte(s >> 24)})
	if got != want {
		t.Fatalf("single-sample CRC = %08X, want %08X", got, want)
	}
}

// ── computeCRCWoNull ─────────────────────────────────────────────────────────

func TestComputeCRCWoNull_AllZero(t *testing.T) {
	// All zero samples: no bytes processed → result is 0xFFFFFFFF ^ 0xFFFFFFFF = 0
	got := computeCRCWoNull([]uint32{0, 0, 0})
	if got != 0 {
		t.Fatalf("all-zero CRCWoNull = %08X, want 0", got)
	}
}

func TestComputeCRCWoNull_SkipsZeroWords(t *testing.T) {
	// sample with lo=0: only hi is processed; result equals CRC of just the hi word
	hi := uint16(0x1234)
	sample := uint32(hi) << 16
	got := computeCRCWoNull([]uint32{sample})

	h := crc32.NewIEEE()
	h.Write([]byte{byte(hi), byte(hi >> 8)})
	want := h.Sum32()
	if got != want {
		t.Fatalf("CRCWoNull(hi only) = %08X, want %08X", got, want)
	}
}

func TestComputeCRCWoNull_NonZeroMatchesCTDB(t *testing.T) {
	// When all 16-bit words are non-zero, CRCWoNull processes every word like a
	// standard CRC32 over the 16-bit words (little-endian).
	samples := []uint32{0x00010002, 0x00030004}
	got := computeCRCWoNull(samples)

	h := crc32.NewIEEE()
	for _, s := range samples {
		lo := uint16(s)
		hi := uint16(s >> 16)
		h.Write([]byte{byte(lo), byte(lo >> 8)})
		h.Write([]byte{byte(hi), byte(hi >> 8)})
	}
	want := h.Sum32()
	if got != want {
		t.Fatalf("CRCWoNull non-zero = %08X, want %08X", got, want)
	}
}

// ── computeRawCRCPrefixes ────────────────────────────────────────────────────

func TestComputeRawCRCPrefixes_Consistency(t *testing.T) {
	samples := []uint32{0xAABBCCDD, 0x11223344, 0x55667788}
	positions := []int{0, 1, 2, 3}
	prefixes := computeRawCRCPrefixes(samples, positions)

	// Prefix at position n should be the raw (init=0) CRC of samples[0:n].
	tab := crc32.IEEETable
	raw := uint32(0)
	for i, s := range samples {
		got, ok := prefixes[i]
		if !ok {
			t.Fatalf("missing prefix at %d", i)
		}
		if got != raw {
			t.Fatalf("prefix[%d] = %08X, want %08X", i, got, raw)
		}
		raw = tab[byte(raw)^byte(s)] ^ (raw >> 8)
		raw = tab[byte(raw)^byte(s>>8)] ^ (raw >> 8)
		raw = tab[byte(raw)^byte(s>>16)] ^ (raw >> 8)
		raw = tab[byte(raw)^byte(s>>24)] ^ (raw >> 8)
	}
	got, ok := prefixes[3]
	if !ok {
		t.Fatalf("missing prefix at 3")
	}
	if got != raw {
		t.Fatalf("prefix[3] = %08X, want %08X", got, raw)
	}
}

// ── GF2 matrix ───────────────────────────────────────────────────────────────

func TestBuildAdvanceMatrix_Zero(t *testing.T) {
	M := buildAdvanceMatrix(0)
	// Identity: gf2MatrixTimes(I, v) = v
	for _, v := range []uint32{0, 1, 0xDEADBEEF, 0xFFFFFFFF} {
		got := gf2MatrixTimes(&M, v)
		if got != v {
			t.Fatalf("identity matrix: got %08X for input %08X", got, v)
		}
	}
}

func TestBuildAdvanceMatrix_CombinesPrefix(t *testing.T) {
	// Verify: standardCRC(a++b) = 0xFFFFFFFF ^ gf2MatrixTimes(M_len_a, 0xFFFFFFFF^prefix[0]) ^ rawCRC(a++b)
	// Equivalently: the prefix-splitting formula used in findTrackCRC is correct.
	samples := []uint32{0x01020304, 0x05060708, 0x090A0B0C, 0x0D0E0F10}
	// Split at position 2
	splitAt := 2
	M := buildAdvanceMatrix(int64(splitAt) * 4)

	prefixes := computeRawCRCPrefixes(samples, []int{0, splitAt, len(samples)})
	pa := prefixes[0]
	pb := prefixes[splitAt]
	full := prefixes[len(samples)]

	// CRC of samples[0:splitAt] using the GF2 formula:
	got := uint32(0xffffffff) ^ gf2MatrixTimes(&M, uint32(0xffffffff)^pa) ^ pb
	want := computeCTDBCRC32(samples[:splitAt])
	if got != want {
		t.Fatalf("partial GF2 CRC = %08X, want %08X", got, want)
	}

	// CRC of the full slice:
	M2 := buildAdvanceMatrix(int64(len(samples)) * 4)
	got2 := uint32(0xffffffff) ^ gf2MatrixTimes(&M2, uint32(0xffffffff)^pa) ^ full
	want2 := computeCTDBCRC32(samples)
	if got2 != want2 {
		t.Fatalf("full GF2 CRC = %08X, want %08X", got2, want2)
	}
}

// ── ctdbSuffixSectors ────────────────────────────────────────────────────────

func TestCtdbSuffixSectors(t *testing.T) {
	// n/588 % 10 gives the residual; result = 10 + residual
	cases := []struct {
		n    int
		want int
	}{
		{0 * 588, 10},   // 0 % 10 = 0
		{5 * 588, 15},   // 5 % 10 = 5
		{10 * 588, 10},  // 10 % 10 = 0
		{13 * 588, 13},  // 13 % 10 = 3
		{100 * 588, 10}, // 100 % 10 = 0
		{123 * 588, 13}, // 123 % 10 = 3
	}
	for _, c := range cases {
		got := ctdbSuffixSectors(c.n)
		if got != c.want {
			t.Errorf("ctdbSuffixSectors(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}

// ── trackWindowBase ───────────────────────────────────────────────────────────

func TestTrackWindowBase(t *testing.T) {
	layout := buildLayoutFromLengths([]int{1000, 2000, 3000})
	// Track 0 (first): a0 = prefixSectors*588; length = track1.Start - a0
	a0, L := trackWindowBase(layout, 0, 10, 10)
	wantA0 := 10 * 588 // prefixSectors * 588 (relative to discBase=150)
	if a0 != wantA0 {
		t.Fatalf("track0 a0 = %d, want %d", a0, wantA0)
	}
	// end for track 0 = track1.Start - discBase = 1000 sectors * 588
	wantL := 1000*588 - wantA0
	if L != wantL {
		t.Fatalf("track0 length = %d, want %d", L, wantL)
	}

	// Track 1 (middle): a0 = start of track1 relative to discBase
	a0_1, L1 := trackWindowBase(layout, 1, 10, 10)
	wantA0_1 := 1000 * 588 // track1.Start - discBase
	if a0_1 != wantA0_1 {
		t.Fatalf("track1 a0 = %d, want %d", a0_1, wantA0_1)
	}
	wantL1 := 2000 * 588
	if L1 != wantL1 {
		t.Fatalf("track1 length = %d, want %d", L1, wantL1)
	}
}

// ── findTrackCRC ─────────────────────────────────────────────────────────────

func TestFindTrackCRC_ExactMatch(t *testing.T) {
	// Build a small 1-track disc and verify that findTrackCRC finds oi=0.
	const sectors = 100
	total := sectors * 588
	samples := make([]uint32, total)
	for i := range samples {
		samples[i] = uint32(i+1) * 0x00010001
	}
	layout := buildLayoutFromLengths([]int{sectors})
	suffix := ctdbSuffixSectors(total)

	target := trackCRC(samples, layout, 0, 0, 10, suffix)
	positions := collectNeededPositions(samples, layout, suffix)
	prefixCRCs := computeRawCRCPrefixes(samples, positions)

	found, oi := findTrackCRC(samples, prefixCRCs, layout, 0, target, suffix)
	if !found {
		t.Fatal("findTrackCRC: should find exact match")
	}
	if oi != 0 {
		t.Fatalf("findTrackCRC: expected oi=0, got %d", oi)
	}
}

func TestFindTrackCRC_OffsetMatch(t *testing.T) {
	// Build a 3-track disc; compute the CRC at a non-zero offset; verify findTrackCRC
	// reports the same offset.
	const prefixSectors = 10
	trackLengths := []int{300, 400, 200}
	total := 0
	for _, l := range trackLengths {
		total += l * 588
	}
	samples := make([]uint32, total)
	for i := range samples {
		// Non-trivial pattern so CRC is unlikely to match at the wrong offset
		samples[i] = uint32((i*7+13)&0xFFFF) | uint32((i*11+17)&0xFFFF)<<16
	}
	layout := buildLayoutFromLengths(trackLengths)
	suffix := ctdbSuffixSectors(total)

	const testOi = 300 // well within arOffsetRange (2939)
	for trackIdx := range trackLengths {
		target := trackCRC(samples, layout, trackIdx, testOi, prefixSectors, suffix)
		if target == 0 {
			continue // window out of bounds, skip
		}
		positions := collectNeededPositions(samples, layout, suffix)
		prefixCRCs := computeRawCRCPrefixes(samples, positions)

		found, matchedOi := findTrackCRC(samples, prefixCRCs, layout, trackIdx, target, suffix)
		if !found {
			t.Fatalf("track %d: findTrackCRC did not find target at oi=%d", trackIdx, testOi)
		}
		if matchedOi != testOi {
			// Multiple offsets can produce the same CRC; just check it's valid.
			verify := trackCRC(samples, layout, trackIdx, matchedOi, prefixSectors, suffix)
			if verify != target {
				t.Fatalf("track %d: mismatched oi=%d gives CRC %08X, not target %08X",
					trackIdx, matchedOi, verify, target)
			}
		}
	}
}

func TestFindTrackCRC_NoMatch(t *testing.T) {
	const sectors = 100
	total := sectors * 588
	samples := make([]uint32, total)
	for i := range samples {
		samples[i] = 0x00010001 // constant
	}
	layout := buildLayoutFromLengths([]int{sectors})
	suffix := ctdbSuffixSectors(total)
	positions := collectNeededPositions(samples, layout, suffix)
	prefixCRCs := computeRawCRCPrefixes(samples, positions)

	found, _ := findTrackCRC(samples, prefixCRCs, layout, 0, 0xDEADBEEF, suffix)
	if found {
		t.Fatal("findTrackCRC: should not find match for impossible CRC")
	}
}

// ── flattenTrackSamples ───────────────────────────────────────────────────────

func TestFlattenTrackSamples(t *testing.T) {
	a := []uint32{1, 2, 3}
	b := []uint32{4, 5}
	got := flattenTrackSamples([][]uint32{a, b})
	want := []uint32{1, 2, 3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

// ── loadFLACTrack with a real (in-memory) FLAC ───────────────────────────────

func TestLoadFLACTrack_CRCsConsistent(t *testing.T) {
	// Write a small fake FLAC, load it, and verify that all computed fields are
	// self-consistent (single-pass results match independent post-hoc computation).
	const sectors = 3
	dir := t.TempDir()
	path := filepath.Join(dir, "track01.flac")
	writeFakeFLAC(t, path, sectors, 0xCAFE)

	info, err := loadFLACTrack(path)
	if err != nil {
		t.Fatalf("loadFLACTrack: %v", err)
	}
	if len(info.Samples) != sectors*588 {
		t.Fatalf("sample count = %d, want %d", len(info.Samples), sectors*588)
	}

	// TrackCRC32 must match independent computation
	wantCRC := computeCTDBCRC32(info.Samples)
	if info.TrackCRC32 != wantCRC {
		t.Fatalf("TrackCRC32 = %08X, want %08X", info.TrackCRC32, wantCRC)
	}

	// CRCWoNull must match independent computation
	wantNull := computeCRCWoNull(info.Samples)
	if info.CRCWoNull != wantNull {
		t.Fatalf("CRCWoNull = %08X, want %08X", info.CRCWoNull, wantNull)
	}

	// AccurateRip CRC must match independent computation
	wantAR := ComputeARTrackCRC32(info.Samples)
	if info.CRC != wantAR {
		t.Fatalf("AccurateRip CRC = %08X, want %08X", info.CRC, wantAR)
	}

	// Peak must be correct
	var wantPeak int
	for _, s := range info.Samples {
		if l := int(int16(uint16(s))); l < 0 {
			if -l > wantPeak {
				wantPeak = -l
			}
		} else if l > wantPeak {
			wantPeak = l
		}
		if r := int(int16(uint16(s >> 16))); r < 0 {
			if -r > wantPeak {
				wantPeak = -r
			}
		} else if r > wantPeak {
			wantPeak = r
		}
	}
	if info.Peak != wantPeak {
		t.Fatalf("Peak = %d, want %d", info.Peak, wantPeak)
	}

	// Sectors must be correct
	if info.Sectors != sectors {
		t.Fatalf("Sectors = %d, want %d", info.Sectors, sectors)
	}
}

func TestLoadFLACTrack_IsSilent(t *testing.T) {
	// A track with seed 0 will have random data (not silent); verify IsSilent=false
	// via the BuildDiscFromFLAC path which sets IsSilent.
	const sectors = 2
	dir := makeFakeFLACDir(t, []int{sectors})
	_, _, tracks, err := BuildDiscFromFLAC(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tracks[0].IsSilent {
		t.Fatal("random audio should not be silent")
	}
}

// ── quickBuildTOC matches full load ─────────────────────────────────────────

func TestQuickBuildTOC_MatchesFullLoad(t *testing.T) {
	trackLengths := []int{50, 75, 30}
	dir := makeFakeFLACDir(t, trackLengths)

	files, err := discoverFLACFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	quick, err := quickBuildTOC(files)
	if err != nil {
		t.Fatal(err)
	}
	_, _, tracks, err := BuildDiscFromFLAC(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(quick.Tracks) != len(tracks) {
		t.Fatalf("track count: quick=%d full=%d", len(quick.Tracks), len(tracks))
	}
	for i := range tracks {
		if quick.Tracks[i].Length != tracks[i].Sectors {
			t.Errorf("track %d: quick sectors=%d, full sectors=%d",
				i+1, quick.Tracks[i].Length, tracks[i].Sectors)
		}
	}
}

// ── BuildDiscFromFLAC layout ─────────────────────────────────────────────────

func TestBuildDiscFromFLAC_Layout(t *testing.T) {
	trackLengths := []int{100, 200, 150}
	dir := makeFakeFLACDir(t, trackLengths)
	layout, crcs, tracks, err := BuildDiscFromFLAC(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(layout.Tracks) != 3 {
		t.Fatalf("track count = %d, want 3", len(layout.Tracks))
	}
	if layout.Tracks[0].Start != 150 {
		t.Fatalf("track 1 start = %d, want 150", layout.Tracks[0].Start)
	}
	for i, wantLen := range trackLengths {
		if layout.Tracks[i].Length != wantLen {
			t.Errorf("track %d length = %d, want %d", i+1, layout.Tracks[i].Length, wantLen)
		}
	}
	// Track starts must be contiguous
	for i := 1; i < len(layout.Tracks); i++ {
		wantStart := layout.Tracks[i-1].Start + layout.Tracks[i-1].Length
		if layout.Tracks[i].Start != wantStart {
			t.Errorf("track %d start = %d, want %d", i+1, layout.Tracks[i].Start, wantStart)
		}
	}
	if len(crcs) != len(tracks) {
		t.Fatalf("crcs len %d != tracks len %d", len(crcs), len(tracks))
	}
	for i, c := range crcs {
		if c != tracks[i].CRC {
			t.Errorf("crcs[%d] = %08X, tracks[%d].CRC = %08X", i, c, i, tracks[i].CRC)
		}
	}
}

func TestBuildDiscFromFLAC_Progress(t *testing.T) {
	dir := makeFakeFLACDir(t, []int{10, 20, 30})
	var calls []int
	_, _, _, err := BuildDiscFromFLAC(dir, func(done, total int, _ string) {
		calls = append(calls, done)
		if total != 3 {
			t.Errorf("progress total = %d, want 3", total)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("progress called %d times, want 3", len(calls))
	}
}

func TestBuildDiscFromFLAC_Empty(t *testing.T) {
	dir := t.TempDir()
	_, _, _, err := BuildDiscFromFLAC(dir, nil)
	if err == nil {
		t.Fatal("expected error for empty directory")
	}
	if !strings.Contains(err.Error(), "no FLAC files") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ── CTDB integration: fake audio must NOT match real disc ────────────────────

// knownTOCLengths are the per-track sector lengths for the disc with
// CTDB TOCID A0o8jAlrcLu.n.GYK9wCh4GdvOI- derived from:
//
//	0:22452:48310:75540:95030:113275:117910:141930:164190:191077:217357:235317:258477:269940
var knownTOCLengths = []int{
	22452, 25858, 27230, 19490, 18245, 4635,
	24020, 22260, 26887, 26280, 17960, 23160, 11463,
}

func TestFakeFLACsDoNotVerifyAgainstCTDB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in -short mode")
	}

	dir := makeFakeFLACDir(t, knownTOCLengths)

	result, err := VerifyFLACFolder(dir, "http://db.cuetools.net", nil)
	if err != nil {
		// Skip on network failure rather than failing the test
		t.Skipf("CTDB unreachable: %v", err)
	}

	if result.TotalEntries == 0 {
		t.Fatal("CTDB returned no entries for known disc — TOC mismatch or disc removed from database")
	}
	t.Logf("CTDB returned %d total entries for known TOC", result.TotalEntries)

	if result.MatchedEntry != nil {
		t.Fatal("fake audio should NOT match any CTDB entry, but got a match")
	}
	for i, m := range result.CTDBMatches {
		for j, ok := range m.TrackMatch {
			if ok {
				t.Errorf("entry[%d] track %d: fake audio unexpectedly matched", i, j+1)
			}
		}
	}
}
