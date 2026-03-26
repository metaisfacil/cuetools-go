package accuraterip

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"time"
)

// arSkipStart is the number of samples excluded from the beginning of the first track.
// arSkipEnd is the number of samples excluded from the end of the last track.
// These match the AccurateRip reference implementation (CUETools.AccurateRip).
const (
	arSkipStart = 5*588 - 1 // 2939 samples
	arSkipEnd   = 5 * 588   // 2940 samples
)

// ARTrack is one track entry from the AccurateRip binary response.
type ARTrack struct {
	Confidence  uint8
	CRC         uint32
	Frame450CRC uint32
}

// ARDisk is one pressing record from the AccurateRip binary response.
type ARDisk struct {
	Count    uint8
	DiscID1  uint32
	DiscID2  uint32
	CDDBDisc uint32
	Tracks   []ARTrack
}

// ARResponse holds all pressing records returned by AccurateRip for a disc.
type ARResponse struct {
	Disks []ARDisk
}

// ARTrackResult holds the per-track AccurateRip verification result.
type ARTrackResult struct {
	V1       uint32 `json:"v1"`      // AccurateRip V1 CRC (lower-32 of weighted sum, with exclusions)
	V2       uint32 `json:"v2"`      // AccurateRip V2 CRC (lower+upper 32 of weighted sum)
	Total    uint32 `json:"total"`   // sum of all confidence counts for this track position
	ConfV1   uint32 `json:"conf_v1"` // confidence matched via V1
	ConfV2   uint32 `json:"conf_v2"` // confidence matched via V2
	Accurate bool   `json:"accurate"`
	Silent   bool   `json:"silent"`
}

// ComputeARTrackCRCs computes AccurateRip V1 and V2 CRCs for one track's packed
// stereo samples (each uint32 = lo16:hi16). isFirst/isLast control the exclusion
// zones at the disc edges (2939 samples from start, 2940 from end).
func ComputeARTrackCRCs(samples []uint32, isFirst, isLast bool) (v1, v2 uint32) {
	skipStart := 0
	if isFirst {
		skipStart = arSkipStart
	}
	skipEnd := 0
	if isLast {
		skipEnd = arSkipEnd
	}
	end := len(samples) - skipEnd
	if end <= skipStart {
		return 0, 0
	}
	var low, high uint32
	for k := skipStart; k < end; k++ {
		s := uint64(samples[k])
		pos := uint64(k + 1) // 1-based within-track position (matches C# reference)
		prod := s * pos
		low += uint32(prod)
		high += uint32(prod >> 32)
	}
	return low, low + high
}

// FetchARDatabase downloads and parses the AccurateRip binary database for layout.
// Returns nil response (no error) when the disc is not in the database (HTTP 404).
func FetchARDatabase(id1, id2, cddb uint32, nTracks int, client *http.Client) (*ARResponse, error) {
	url := fmt.Sprintf(
		"http://www.accuraterip.com/accuraterip/%x/%x/%x/dBAR-%03d-%08x-%08x-%08x.bin",
		id1&0xF, (id1>>4)&0xF, (id1>>8)&0xF,
		nTracks, id1, id2, cddb,
	)
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // disc not in database
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("accuraterip: HTTP %d", resp.StatusCode)
	}
	return parseARResponse(resp.Body)
}

func parseARResponse(r io.Reader) (*ARResponse, error) {
	var result ARResponse
	hdr := make([]byte, 13)
	trk := make([]byte, 9)
	for {
		if _, err := io.ReadFull(r, hdr); err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		} else if err != nil {
			return nil, err
		}
		disk := ARDisk{
			Count:    hdr[0],
			DiscID1:  binary.LittleEndian.Uint32(hdr[1:5]),
			DiscID2:  binary.LittleEndian.Uint32(hdr[5:9]),
			CDDBDisc: binary.LittleEndian.Uint32(hdr[9:13]),
		}
		for range disk.Count {
			if _, err := io.ReadFull(r, trk); err != nil {
				return nil, fmt.Errorf("accuraterip: truncated track record: %w", err)
			}
			disk.Tracks = append(disk.Tracks, ARTrack{
				Confidence:  trk[0],
				CRC:         binary.LittleEndian.Uint32(trk[1:5]),
				Frame450CRC: binary.LittleEndian.Uint32(trk[5:9]),
			})
		}
		result.Disks = append(result.Disks, disk)
	}
	return &result, nil
}

// VerifyAR matches per-track V1/V2 CRCs against an AccurateRip database response.
// firstAudio is the 1-based first audio track index in the disk.Tracks slice.
func VerifyAR(trackCRCs []ARTrackCRCPair, firstAudio int, response *ARResponse) []ARTrackResult {
	results := make([]ARTrackResult, len(trackCRCs))
	if response == nil {
		for i, p := range trackCRCs {
			results[i].V1 = p.V1
			results[i].V2 = p.V2
		}
		return results
	}
	for i, p := range trackCRCs {
		res := &results[i]
		res.V1 = p.V1
		res.V2 = p.V2
		trno := i + firstAudio - 1 // 0-indexed position in the disk.Tracks slice
		for _, disk := range response.Disks {
			if trno >= len(disk.Tracks) {
				continue
			}
			t := disk.Tracks[trno]
			res.Total += uint32(t.Confidence)
			if t.CRC != 0 && t.CRC == p.V1 {
				res.ConfV1 += uint32(t.Confidence)
			} else if t.CRC != 0 && t.CRC == p.V2 {
				res.ConfV2 += uint32(t.Confidence)
			}
		}
		res.Accurate = res.ConfV1+res.ConfV2 > 0
		res.Silent = p.V1 == 0 && res.Total == 0
	}
	return results
}

// ARTrackCRCPair holds the V1 and V2 AccurateRip CRCs for a single track.
type ARTrackCRCPair struct {
	V1 uint32
	V2 uint32
}
