package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/example/cuetools-go/cuetools"
)

func TestPrintLog_AccurateRipNotFound(t *testing.T) {
	result := cuetools.VerificationResult{
		TOC:           cuetools.CDImageLayout{FirstAudio: 1, AudioTracks: 1, Tracks: []cuetools.CDTrack{{Number: 1, Start: 150, Length: 100, IsAudio: true}}},
		Tracks:        []cuetools.TrackInfo{{TrackNumber: 1, Peak: 100, TrackCRC32: 0x12345678, CRCWoNull: 0x87654321}},
		CTDBMatches:   nil,
		MatchedEntry:  nil,
		TotalEntries:  0,
		DiscCRC32:     0x11111111,
		DiscCRCWoNull: 0x22222222,
		DiscPeak:      100,
		ARFound:       false,
		ARResults:     []cuetools.ARTrackResult{{V1: 0xdeadbeef, V2: 0xcafebabe, Total: 0, ConfV1: 0, ConfV2: 0, Accurate: false, Silent: false}},
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	os.Stdout = w

	printLog(result, false)

	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	os.Stdout = oldStdout

	out := buf.String()
	if !strings.Contains(out, "disk not present in database") {
		t.Fatalf("expected not-found message, got:\n%s", out)
	}
	if strings.Contains(out, "Track   [  CRC   |   V2   ] Status") {
		t.Fatalf("expected no AccurateRip track table when AR not found, got:\n%s", out)
	}
}
