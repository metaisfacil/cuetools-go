package cuetools

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/mewkiz/flac"
)

// ARCRC32 accumulates AccurateRip-style CRC.
type ARCRC32 struct {
	low  uint64
	high uint64
	pos  uint64
}

// AddSample adds a sample to the ARCRC32 accumulator in AccurateRip style.
func (a *ARCRC32) AddSample(sample uint32) {
	a.pos++
	prod := uint64(sample) * a.pos
	a.low += uint64(uint32(prod))
	a.high += uint64(uint32(prod >> 32))
}

// Sum returns the AccurateRip-style CRC accumulated by the ARCRC32.
func (a *ARCRC32) Sum() uint32 {
	return uint32(a.low + a.high)
}

// TrackInfo is per-track decoded result.
const arOffsetRange = 5*588 - 1

func flattenTrackSamples(trackSamplesByTrack [][]uint32) []uint32 {
	var total int
	for _, t := range trackSamplesByTrack {
		total += len(t)
	}
	all := make([]uint32, 0, total)
	for _, t := range trackSamplesByTrack {
		all = append(all, t...)
	}
	return all
}

func trackCRC(allSamples []uint32, toc CDImageLayout, trackIdx, oi, prefixSectors, suffixSectors int) uint32 {
	if trackIdx < 0 || trackIdx >= len(toc.Tracks) {
		return 0
	}
	discBase := toc.Tracks[0].Start * 588
	trackStartSector := toc.Tracks[trackIdx].Start
	trackStartAbs := trackStartSector * 588
	var nextStartSector int
	if trackIdx+1 < len(toc.Tracks) {
		nextStartSector = toc.Tracks[trackIdx+1].Start
	}
	start := int(trackStartAbs - discBase)
	if trackIdx > 0 {
		start += oi
	} else {
		start += prefixSectors*588 + oi
	}
	if start < 0 {
		start = 0
	}
	var end int
	if trackIdx+1 < len(toc.Tracks) {
		end = int(nextStartSector*588-discBase) + oi
	} else {
		end = int((toc.Tracks[len(toc.Tracks)-1].Start+toc.Tracks[len(toc.Tracks)-1].Length)*588 - discBase)
		end -= suffixSectors*588 - oi
	}
	if end > len(allSamples) {
		end = len(allSamples)
	}
	if start > end {
		return 0
	}
	return computeCTDBCRC32(allSamples[start:end])
}

// trackWindowBase returns the sample index of the CTDB window start (at oi=0)
// and the window length in samples for the given track.
func trackWindowBase(toc CDImageLayout, trackIdx, prefixSectors, suffixSectors int) (a0, length int) {
	discBase := toc.Tracks[0].Start * 588
	trackStart := toc.Tracks[trackIdx].Start*588 - discBase
	if trackIdx == 0 {
		a0 = trackStart + prefixSectors*588
	} else {
		a0 = trackStart
	}
	var end int
	if trackIdx+1 < len(toc.Tracks) {
		end = toc.Tracks[trackIdx+1].Start*588 - discBase
	} else {
		last := toc.Tracks[len(toc.Tracks)-1]
		end = (last.Start+last.Length)*588 - discBase - suffixSectors*588
	}
	length = end - a0
	return
}

// collectNeededPositions returns the sorted list of allSamples positions at which
// raw CRC prefix values are needed for offset scanning across all tracks.
func collectNeededPositions(allSamples []uint32, toc CDImageLayout, suffixSectors int) []int {
	n := len(allSamples)
	posSet := make(map[int]struct{}, len(toc.Tracks)*4*arOffsetRange)
	for trackIdx := range toc.Tracks {
		a0, L := trackWindowBase(toc, trackIdx, 10, suffixSectors)
		loA := a0 - arOffsetRange
		hiA := a0 + arOffsetRange
		for p := loA; p <= hiA; p++ {
			if p >= 0 && p <= n {
				posSet[p] = struct{}{}
			}
			q := p + L
			if q >= 0 && q <= n {
				posSet[q] = struct{}{}
			}
		}
	}
	positions := make([]int, 0, len(posSet))
	for p := range posSet {
		positions = append(positions, p)
	}
	sort.Ints(positions)
	return positions
}

// computeRawCRCPrefixes builds a map of position → raw CRC (init=0) of allSamples[0:pos].
// positions must be sorted ascending.
func computeRawCRCPrefixes(allSamples []uint32, positions []int) map[int]uint32 {
	result := make(map[int]uint32, len(positions))
	if len(positions) == 0 {
		return result
	}
	pi := 0
	for pi < len(positions) && positions[pi] < 0 {
		pi++
	}
	if pi < len(positions) && positions[pi] == 0 {
		result[0] = 0
		pi++
	}
	t := crc32.IEEETable
	var raw uint32
	for i, s := range allSamples {
		raw = t[byte(raw)^byte(s)] ^ (raw >> 8)
		raw = t[byte(raw)^byte(s>>8)] ^ (raw >> 8)
		raw = t[byte(raw)^byte(s>>16)] ^ (raw >> 8)
		raw = t[byte(raw)^byte(s>>24)] ^ (raw >> 8)
		pos := i + 1
		for pi < len(positions) && positions[pi] == pos {
			result[pos] = raw
			pi++
		}
		if pi >= len(positions) {
			break
		}
	}
	return result
}

const gf2Dim = 32

func gf2MatrixTimes(mat *[gf2Dim]uint32, vec uint32) uint32 {
	var sum uint32
	for i := range gf2Dim {
		if vec&(1<<uint(i)) != 0 {
			sum ^= mat[i]
		}
	}
	return sum
}

func gf2MatrixSquare(sq, mat *[gf2Dim]uint32) {
	for i := range gf2Dim {
		sq[i] = gf2MatrixTimes(mat, mat[i])
	}
}

// buildAdvanceMatrix returns the GF2 matrix that advances a raw CRC state by nBytes zero bytes.
func buildAdvanceMatrix(nBytes int64) [gf2Dim]uint32 {
	var result [gf2Dim]uint32
	for i := range gf2Dim {
		result[i] = 1 << uint(i) // identity
	}
	if nBytes == 0 {
		return result
	}
	var odd [gf2Dim]uint32
	odd[0] = 0xEDB88320 // CRC-32/IEEE polynomial
	for i := range gf2Dim - 1 {
		odd[i+1] = 1 << uint(i)
	}
	var even [gf2Dim]uint32
	gf2MatrixSquare(&even, &odd) // 2-bit advance
	gf2MatrixSquare(&odd, &even) // 4-bit advance
	// Each outer loop squares: odd→even = 8-bit (1 byte), even→odd = 16-bit, …
	for n := nBytes; n > 0; {
		gf2MatrixSquare(&even, &odd)
		if n&1 != 0 {
			var tmp [gf2Dim]uint32
			for i := range gf2Dim {
				tmp[i] = gf2MatrixTimes(&even, result[i])
			}
			result = tmp
		}
		n >>= 1
		if n == 0 {
			break
		}
		gf2MatrixSquare(&odd, &even)
		if n&1 != 0 {
			var tmp [gf2Dim]uint32
			for i := range gf2Dim {
				tmp[i] = gf2MatrixTimes(&odd, result[i])
			}
			result = tmp
		}
		n >>= 1
	}
	return result
}

// findTrackCRC scans all sample-granularity offsets in [-arOffsetRange, +arOffsetRange]
// using precomputed raw CRC prefix values and GF2 matrix advance.
// Each offset check is O(32) instead of O(trackLen).
func findTrackCRC(allSamples []uint32, prefixCRCs map[int]uint32, toc CDImageLayout, trackIdx int, targetCRC uint32, suffixSectors int) (found bool, matchedOi int) {
	a0, L := trackWindowBase(toc, trackIdx, 10, suffixSectors)
	if L <= 0 {
		return false, 0
	}
	M := buildAdvanceMatrix(int64(L) * 4)
	n := len(allSamples)
	for oi := -arOffsetRange; oi <= arOffsetRange; oi++ {
		a := a0 + oi
		b := a + L
		if a < 0 || b > n {
			continue
		}
		pa, okA := prefixCRCs[a]
		pb, okB := prefixCRCs[b]
		if !okA || !okB {
			continue
		}
		computed := uint32(0xffffffff) ^ gf2MatrixTimes(&M, uint32(0xffffffff)^pa) ^ pb
		if computed == targetCRC {
			return true, oi
		}
	}
	return false, 0
}

// TrackInfo contains metadata and audio data for a single track.
type TrackInfo struct {
	File          string
	TrackNumber   int
	SampleRate    int
	Channels      int
	BitsPerSample int
	FrameSamples  int // samples per channel
	Sectors       int
	ARCRCV1       uint32 // AccurateRip V1 CRC (with edge exclusions)
	ARCRCV2       uint32 // AccurateRip V2 CRC (with edge exclusions)
	TrackCRC32    uint32 // standard CRC32 of audio samples
	CRCWoNull     uint32 // CRC32 skipping zero-valued 16-bit words (EAC style)
	Peak          int    // max absolute 16-bit sample value, range 0–32767
	Samples       []uint32
	IsSilent      bool
}

// VerificationResult holds CTDB and AccurateRip track results.
type VerificationResult struct {
	TOC           CDImageLayout
	Tracks        []TrackInfo
	CTDBMatches   []DBMatch
	MatchedEntry  *DBEntry
	Parity        []byte
	TotalEntries  int
	DiscCRC32     uint32 // standard CRC32 over all audio samples
	DiscCRCWoNull uint32 // CRC32 without null samples over all audio
	DiscPeak      int    // max peak across all tracks (0–32767)
	// AccurateRip
	ARFound   bool           // true when disc ID is in the AccurateRip database
	ARResults []ARTrackResult
}

// DBMatch indicates how a CTDB entry matches track CRCs.
type DBMatch struct {
	Entry      DBEntry
	TrackMatch []bool
	AllMatched bool
	ParityURL  string
	ParitySize int64
	Parity     []byte
}

var trackNumberRe = regexp.MustCompile(`(?i)^(?:track[_-]?|)(\d{1,3})`)

func parseTrackNumber(name string) (int, bool) {
	base := filepath.Base(name)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	m := trackNumberRe.FindStringSubmatch(base)
	if len(m) < 2 {
		return 0, false
	}
	var n int
	_, err := fmt.Sscanf(m[1], "%d", &n)
	if err != nil {
		return 0, false
	}
	return n, true
}

func toInt16(sample int32, bits int) int16 {
	if bits > 16 {
		sample >>= uint(bits - 16)
	} else if bits < 16 {
		sample <<= uint(16 - bits)
	}
	return int16(sample)
}

func loadFLACTrack(path string) (TrackInfo, error) {
	info := TrackInfo{File: path}
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()
	stream, err := flac.Parse(f)
	if err != nil {
		return info, err
	}

	if stream.Info == nil {
		return info, errors.New("missing stream info")
	}
	info.SampleRate = int(stream.Info.SampleRate)
	info.Channels = int(stream.Info.NChannels)
	info.BitsPerSample = int(stream.Info.BitsPerSample)
	if stream.Info.NSamples > 0 {
		info.Samples = make([]uint32, 0, stream.Info.NSamples)
	}

	t := crc32.IEEETable
	var crcRaw uint32 = 0xffffffff  // running state for TrackCRC32
	var crcNull uint32 = 0xffffffff // running state for CRCWoNull
	var peak int
	totalFrames := 0

	for {
		fr, err := stream.ParseNext()
		if err == io.EOF {
			break
		}
		if err != nil {
			return info, err
		}
		if len(fr.Subframes) == 0 {
			continue
		}
		block := len(fr.Subframes[0].Samples)
		for i := range block {
			left := fr.Subframes[0].Samples[i]
			var right int32
			if info.Channels >= 2 {
				right = fr.Subframes[1].Samples[i]
			} else {
				right = left
			}
			l16 := toInt16(left, info.BitsPerSample)
			r16 := toInt16(right, info.BitsPerSample)
			packed := uint32(uint16(l16)) | uint32(uint16(r16))<<16

			// TrackCRC32 inline (avoids a second pass)
			crcRaw = t[byte(crcRaw)^byte(packed)] ^ (crcRaw >> 8)
			crcRaw = t[byte(crcRaw)^byte(packed>>8)] ^ (crcRaw >> 8)
			crcRaw = t[byte(crcRaw)^byte(packed>>16)] ^ (crcRaw >> 8)
			crcRaw = t[byte(crcRaw)^byte(packed>>24)] ^ (crcRaw >> 8)

			// CRCWoNull inline (avoids a second pass)
			if lo := uint16(packed); lo != 0 {
				crcNull = t[byte(crcNull)^byte(lo)] ^ (crcNull >> 8)
				crcNull = t[byte(crcNull)^byte(lo>>8)] ^ (crcNull >> 8)
			}
			if hi := uint16(packed >> 16); hi != 0 {
				crcNull = t[byte(crcNull)^byte(hi)] ^ (crcNull >> 8)
				crcNull = t[byte(crcNull)^byte(hi>>8)] ^ (crcNull >> 8)
			}

			// Peak inline (avoids a second pass)
			if l := int(l16); l < 0 {
				if -l > peak {
					peak = -l
				}
			} else if l > peak {
				peak = l
			}
			if r := int(r16); r < 0 {
				if -r > peak {
					peak = -r
				}
			} else if r > peak {
				peak = r
			}

			info.Samples = append(info.Samples, packed)
		}
		totalFrames += block
	}

	info.FrameSamples = totalFrames
	if info.SampleRate <= 0 {
		return info, errors.New("invalid sample rate")
	}
	// CD sector = 1/75 second
	info.Sectors = int((int64(totalFrames)*75 + int64(info.SampleRate/2)) / int64(info.SampleRate))
	info.Sectors = max(info.Sectors, 1)
	info.TrackCRC32 = crcRaw ^ 0xffffffff
	info.CRCWoNull = crcNull ^ 0xffffffff
	info.Peak = peak
	return info, nil
}

// flacFileEntry is a discovered FLAC file with its parsed track number.
type flacFileEntry struct {
	path   string
	number int
}

// discoverFLACFiles scans dir for .flac files, parses track numbers, and returns
// them sorted by track number.
func discoverFLACFiles(dir string) ([]flacFileEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []flacFileEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".flac") {
			num, ok := parseTrackNumber(e.Name())
			if !ok {
				num = len(files) + 1
			}
			files = append(files, flacFileEntry{path: filepath.Join(dir, e.Name()), number: num})
		}
	}
	if len(files) == 0 {
		return nil, errors.New("no FLAC files found")
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].number != files[j].number {
			return files[i].number < files[j].number
		}
		return files[i].path < files[j].path
	})
	return files, nil
}

// quickBuildTOC reads only the StreamInfo metadata block from each FLAC file
// (no audio decoding) to compute sector counts and build the CDImageLayout.
// This is used to start the CTDB lookup before the full audio load completes.
func quickBuildTOC(files []flacFileEntry) (CDImageLayout, error) {
	layout := CDImageLayout{FirstAudio: 1, AudioTracks: len(files)}
	start := 150
	for idx, item := range files {
		f, err := os.Open(item.path)
		if err != nil {
			return CDImageLayout{}, fmt.Errorf("track %s: %w", item.path, err)
		}
		s, err := flac.Parse(f)
		f.Close()
		if err != nil {
			return CDImageLayout{}, fmt.Errorf("track %s: %w", item.path, err)
		}
		if s.Info == nil {
			return CDImageLayout{}, fmt.Errorf("missing stream info in %s", item.path)
		}
		nsamp := int64(s.Info.NSamples)
		sr := int64(s.Info.SampleRate)
		if sr <= 0 {
			return CDImageLayout{}, fmt.Errorf("invalid sample rate in %s", item.path)
		}
		sectors := int((nsamp*75 + sr/2) / sr)
		sectors = max(sectors, 1)
		layout.Tracks = append(layout.Tracks, CDTrack{Number: idx + 1, Start: start, Length: sectors, IsAudio: true})
		start += sectors
	}
	return layout, nil
}

// loadAllTracks loads all tracks in parallel using goroutines.
func loadAllTracks(files []flacFileEntry, progress ProgressFunc) ([]TrackInfo, error) {
	tracks := make([]TrackInfo, len(files))
	errs := make([]error, len(files))

	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0

	for idx, item := range files {
		wg.Add(1)
		go func(idx int, item flacFileEntry) {
			defer wg.Done()
			info, err := loadFLACTrack(item.path)
			if err == nil {
				info.TrackNumber = item.number
				info.IsSilent = true
				for _, s := range info.Samples {
					if s != 0 {
						info.IsSilent = false
						break
					}
				}
			}
			tracks[idx] = info
			errs[idx] = err
			mu.Lock()
			done++
			if progress != nil {
				progress(done, len(files), item.path)
			}
			mu.Unlock()
		}(idx, item)
	}
	wg.Wait()

	for idx, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("track %s parse error: %w", files[idx].path, err)
		}
	}
	return tracks, nil
}

// ProgressFunc is called after each track is loaded. done is 1-based.
type ProgressFunc func(done, total int, filename string)

// BuildDiscFromFLAC scans directory and builds CDImageLayout and CTDB track CRCs.
// progress may be nil.
func BuildDiscFromFLAC(dir string, progress ProgressFunc) (CDImageLayout, []uint32, []TrackInfo, error) {
	files, err := discoverFLACFiles(dir)
	if err != nil {
		return CDImageLayout{}, nil, nil, err
	}
	tracks, err := loadAllTracks(files, progress)
	if err != nil {
		return CDImageLayout{}, nil, nil, err
	}

	layout := CDImageLayout{FirstAudio: 1, AudioTracks: len(files)}
	arCRCs := make([]uint32, len(tracks))
	start := 150
	for i, t := range tracks {
		if t.Channels < 1 || t.Channels > 2 {
			return CDImageLayout{}, nil, nil, fmt.Errorf("unsupported channels %d on %s", t.Channels, t.File)
		}
		layout.Tracks = append(layout.Tracks, CDTrack{Number: i + 1, Start: start, Length: t.Sectors, IsAudio: true})
		start += t.Sectors
		v1, v2 := ComputeARTrackCRCs(t.Samples, i == 0, i == len(tracks)-1)
		arCRCs[i] = v1
		tracks[i].ARCRCV1 = v1
		tracks[i].ARCRCV2 = v2
	}
	return layout, arCRCs, tracks, nil
}

// VerifyFLACFolder processes all FLACs and checks CTDB entries.
// progress may be nil.
func VerifyFLACFolder(dir, ctdbServer string, progress ProgressFunc) (VerificationResult, error) {
	files, err := discoverFLACFiles(dir)
	if err != nil {
		return VerificationResult{}, err
	}

	// Quick scan: read only FLAC StreamInfo headers to build TOC without decoding audio.
	// This lets us launch the CTDB network lookup before the (much slower) audio load.
	layout, err := quickBuildTOC(files)
	if err != nil {
		return VerificationResult{}, err
	}

	client := NewCTDBClient()
	if ctdbServer != "" {
		client.BaseURL = ctdbServer
	}

	notify := func(done, total int, msg string) {
		if progress != nil {
			progress(done, total, msg)
		}
	}

	// Launch CTDB lookup concurrently while audio loads.
	type ctdbResult struct {
		resp *CTDBResponse
		err  error
	}
	ctdbCh := make(chan ctdbResult, 1)
	notify(0, 0, "Querying CTDB...")
	go func() {
		resp, err := client.Lookup(layout, true, true, "none")
		ctdbCh <- ctdbResult{resp, err}
	}()

	type arResult struct {
		resp *ARResponse
		err  error
	}
	arCh := make(chan arResult, 1)
	go func() {
		resp, err := FetchARDatabase(layout, client.HTTPClient)
		arCh <- arResult{resp, err}
	}()

	// Load all tracks in parallel.
	tracks, err := loadAllTracks(files, progress)
	if err != nil {
		return VerificationResult{}, err
	}

	// Rebuild layout from actual decoded sector counts (should match quickBuildTOC).
	layout = CDImageLayout{FirstAudio: 1, AudioTracks: len(files)}
	arPairs := make([]ARTrackCRCPair, len(tracks))
	start := 150
	for i, t := range tracks {
		if t.Channels < 1 || t.Channels > 2 {
			return VerificationResult{}, fmt.Errorf("unsupported channels %d on %s", t.Channels, t.File)
		}
		layout.Tracks = append(layout.Tracks, CDTrack{Number: i + 1, Start: start, Length: t.Sectors, IsAudio: true})
		start += t.Sectors
		v1, v2 := ComputeARTrackCRCs(t.Samples, i == 0, i == len(tracks)-1)
		arPairs[i] = ARTrackCRCPair{V1: v1, V2: v2}
		tracks[i].ARCRCV1 = v1
		tracks[i].ARCRCV2 = v2
	}

	// Collect CTDB result (may already be ready if audio loading took longer).
	ctdbRes := <-ctdbCh
	if ctdbRes.err != nil {
		return VerificationResult{}, fmt.Errorf("ctdb lookup failed: %w", ctdbRes.err)
	}
	resp := ctdbRes.resp

	// Collect AccurateRip result.
	arRes := <-arCh
	// Non-fatal: AR lookup failure is tolerated (network may be down).

	result := VerificationResult{TOC: layout, Tracks: tracks}
	if arRes.resp != nil {
		result.ARFound = len(arRes.resp.Disks) > 0
		result.ARResults = VerifyAR(arPairs, layout, arRes.resp)
	} else {
		result.ARResults = VerifyAR(arPairs, layout, nil)
	}

	result.TotalEntries = 0
	for _, entry := range resp.Entries {
		result.TotalEntries += entry.Confidence
	}

	trackSamplesByTrack := make([][]uint32, len(tracks))
	for i := range tracks {
		trackSamplesByTrack[i] = tracks[i].Samples
	}
	allSamples := flattenTrackSamples(trackSamplesByTrack)
	result.DiscCRC32 = computeCTDBCRC32(allSamples)
	result.DiscCRCWoNull = computeCRCWoNull(allSamples)
	for _, t := range tracks {
		if t.Peak > result.DiscPeak {
			result.DiscPeak = t.Peak
		}
	}

	suffixSectors := ctdbSuffixSectors(len(allSamples))
	localTrackCRCs := make([]uint32, len(tracks))
	for i := range tracks {
		localTrackCRCs[i] = trackCRC(allSamples, layout, i, 0, 10, suffixSectors)
	}

	positions := collectNeededPositions(allSamples, layout, suffixSectors)
	prefixCRCs := computeRawCRCPrefixes(allSamples, positions)

	verifyTotal := len(resp.Entries) * len(tracks)
	verifyDone := 0

	for _, entry := range resp.Entries {
		dbEntry, err := entry.ToDBEntry()
		if err != nil {
			continue
		}

		trackMatch := make([]bool, len(tracks))
		all := true
		if len(dbEntry.TrackCRCs) == len(tracks) {
			for i := range tracks {
				trackMatch[i] = false
				verifyDone++
				notify(verifyDone, verifyTotal, "Verifying tracks")
				if dbEntry.TrackCRCs[i] == localTrackCRCs[i] {
					trackMatch[i] = true
					continue
				}
				found, oi := findTrackCRC(allSamples, prefixCRCs, layout, i, dbEntry.TrackCRCs[i], suffixSectors)
				if found {
					trackMatch[i] = true
					_ = oi // keep for future debug/tracking
				} else {
					all = false
				}
			}
		} else {
			// Track count mismatch: cannot evaluate track CRCs directly.
			for i := range trackMatch {
				trackMatch[i] = false
			}
			all = false
		}

		match := DBMatch{Entry: dbEntry, TrackMatch: trackMatch, AllMatched: all}
		if dbEntry.HasParity {
			parityURL := dbEntry.ParityURL(client.BaseURL)
			match.ParityURL = parityURL
			data, err := client.FetchParity(dbEntry)
			if err == nil {
				match.ParitySize = int64(len(data))
				match.Parity = data
				if all {
					result.Parity = data
				}
			}
		}
		result.CTDBMatches = append(result.CTDBMatches, match)
		if all && result.MatchedEntry == nil {
			result.MatchedEntry = &match.Entry
		}
	}

	return result, nil
}

// computeCRCWoNull is the CUETools/EAC CRC32 that skips zero-valued 16-bit words.
// Equivalent to standard CRC32 (init=0xffffffff, finalXOR=0xffffffff) of non-zero 16-bit words.
func computeCRCWoNull(samples []uint32) uint32 {
	t := crc32.IEEETable
	crc := uint32(0xffffffff)
	for _, s := range samples {
		if lo := uint16(s); lo != 0 {
			crc = t[byte(crc)^byte(lo)] ^ (crc >> 8)
			crc = t[byte(crc)^byte(lo>>8)] ^ (crc >> 8)
		}
		if hi := uint16(s >> 16); hi != 0 {
			crc = t[byte(crc)^byte(hi)] ^ (crc >> 8)
			crc = t[byte(crc)^byte(hi>>8)] ^ (crc >> 8)
		}
	}
	return crc ^ 0xffffffff
}

// computeCTDBCRC32 computes the standard CRC32/IEEE of the packed stereo samples.
// Uses direct table lookups instead of the hash.Hash interface to avoid per-call overhead.
func computeCTDBCRC32(samples []uint32) uint32 {
	t := crc32.IEEETable
	crc := uint32(0xffffffff)
	for _, s := range samples {
		crc = t[byte(crc)^byte(s)] ^ (crc >> 8)
		crc = t[byte(crc)^byte(s>>8)] ^ (crc >> 8)
		crc = t[byte(crc)^byte(s>>16)] ^ (crc >> 8)
		crc = t[byte(crc)^byte(s>>24)] ^ (crc >> 8)
	}
	return crc ^ 0xffffffff
}

// ctdbSuffixSectors returns the suffix trim in sectors for a disc with n total stereo-pair samples.
// Formula: laststride = stride + ((n*2) % stride); suffixSectors = laststride/2/588
// where stride = 11760 channel-samples = 10 sectors × 588 × 2.
// Because n is always a multiple of 588, this simplifies to 10 + (n/588 % 10).
func ctdbSuffixSectors(n int) int {
	return 10 + (n/588)%10
}

// ParityURL returns the full URL to the parity file for the DBEntry, using the provided baseURL.
func (dbEntry *DBEntry) ParityURL(baseURL string) string {
	if !dbEntry.HasParity || strings.TrimSpace(dbEntry.HasParityURL) == "" {
		return ""
	}
	if strings.HasPrefix(dbEntry.HasParityURL, "/") {
		return strings.TrimRight(baseURL, "/") + dbEntry.HasParityURL
	}
	return dbEntry.HasParityURL
}

// FetchParity fetches parity file referenced by DBEntry.hasparity.
func (c *CTDBClient) FetchParity(entry DBEntry) ([]byte, error) {
	if !entry.HasParity {
		return nil, errors.New("entry has no parity")
	}
	urlString := entry.ParityURL(c.BaseURL)
	if urlString == "" {
		return nil, errors.New("invalid parity URL")
	}
	resp, err := c.HTTPClient.Get(urlString)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("parity URL status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return data, nil
}
