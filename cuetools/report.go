package cuetools

import (
	"encoding/json"
)

// TrackReport is a JSON-friendly track summary (omits raw samples).
type TrackReport struct {
	File          string `json:"file"`
	TrackNumber   int    `json:"track_number"`
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	BitsPerSample int    `json:"bits_per_sample"`
	FrameSamples  int    `json:"frame_samples"`
	Sectors       int    `json:"sectors"`
	ARCRCV1       uint32 `json:"accuraterip_v1_crc"`
	ARCRCV2       uint32 `json:"accuraterip_v2_crc"`
	TrackCRC32    uint32 `json:"track_crc32"`
	CRCWoNull     uint32 `json:"crc_wo_null"`
	Peak          int    `json:"peak"`
	IsSilent      bool   `json:"is_silent"`
}

// CTDBMatchReport is a JSON-friendly CTDB match row.
type CTDBMatchReport struct {
	Entry      DBEntry `json:"entry"`
	TrackMatch []bool  `json:"track_match"`
	AllMatched bool    `json:"all_matched"`
	ParityURL  string  `json:"parity_url"`
	ParitySize int64   `json:"parity_size"`
}

// VerificationReport is the library-friendly response structure for JSON output.
type VerificationReport struct {
	TOC           CDImageLayout     `json:"toc"`
	TOCID         string            `json:"toc_id"`
	Tracks        []TrackReport     `json:"tracks"`
	CTDBMatches   []CTDBMatchReport `json:"ctdb_matches"`
	MatchedEntry  *DBEntry          `json:"matched_entry,omitempty"`
	TotalEntries  int               `json:"total_entries"`
	DiscCRC32     uint32            `json:"disc_crc32"`
	DiscCRCWoNull uint32            `json:"disc_crc32_without_null"`
	DiscPeak      int               `json:"disc_peak"`
	ARFound       bool              `json:"accuraterip_found"`
	ARResults     []ARTrackResult   `json:"accuraterip_track_results"`
	AccurateRipID string            `json:"accuraterip_id"`
}

// BuildVerificationReport converts a raw VerificationResult to a report.
func BuildVerificationReport(result VerificationResult) VerificationReport {
	tracks := make([]TrackReport, 0, len(result.Tracks))
	for _, t := range result.Tracks {
		tracks = append(tracks, TrackReport{
			File:          t.File,
			TrackNumber:   t.TrackNumber,
			SampleRate:    t.SampleRate,
			Channels:      t.Channels,
			BitsPerSample: t.BitsPerSample,
			FrameSamples:  t.FrameSamples,
			Sectors:       t.Sectors,
			ARCRCV1:       t.ARCRCV1,
			ARCRCV2:       t.ARCRCV2,
			TrackCRC32:    t.TrackCRC32,
			CRCWoNull:     t.CRCWoNull,
			Peak:          t.Peak,
			IsSilent:      t.IsSilent,
		})
	}

	matches := make([]CTDBMatchReport, 0, len(result.CTDBMatches))
	for _, m := range result.CTDBMatches {
		matches = append(matches, CTDBMatchReport{
			Entry:      m.Entry,
			TrackMatch: m.TrackMatch,
			AllMatched: m.AllMatched,
			ParityURL:  m.ParityURL,
			ParitySize: m.ParitySize,
		})
	}

	return VerificationReport{
		TOC:           result.TOC,
		TOCID:         ComputeTOCID(result.TOC),
		Tracks:        tracks,
		CTDBMatches:   matches,
		MatchedEntry:  result.MatchedEntry,
		TotalEntries:  result.TotalEntries,
		DiscCRC32:     result.DiscCRC32,
		DiscCRCWoNull: result.DiscCRCWoNull,
		DiscPeak:      result.DiscPeak,
		ARFound:       result.ARFound,
		ARResults:     result.ARResults,
		AccurateRipID: ComputeAccurateRipID(result.TOC),
	}
}

// VerifyFLACFolderReport verifies a folder and returns the report object.
func VerifyFLACFolderReport(dir, ctdbServer string, progress ProgressFunc) (VerificationReport, error) {
	result, err := VerifyFLACFolder(dir, ctdbServer, progress)
	if err != nil {
		return VerificationReport{}, err
	}
	return BuildVerificationReport(result), nil
}

// VerifyFLACFolderJSON verifies a folder and returns indented JSON bytes.
func VerifyFLACFolderJSON(dir, ctdbServer string, progress ProgressFunc) ([]byte, error) {
	report, err := VerifyFLACFolderReport(dir, ctdbServer, progress)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(report, "", "  ")
}
