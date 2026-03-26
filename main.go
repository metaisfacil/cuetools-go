package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/cuetools-go/cuetools"
)

func trackProgress(done, total int, message string) {
	const barWidth = 25
	if total == 0 {
		fmt.Fprintf(os.Stderr, "\r%-60s", message)
		return
	}
	filled := barWidth * done / total
	bar := strings.Repeat("=", filled)
	if filled < barWidth {
		bar += ">"
		bar += strings.Repeat(" ", barWidth-filled-1)
	}
	label := message
	if strings.ContainsAny(message, `/\`) {
		label = filepath.Base(message)
	}
	if len(label) > 35 {
		label = label[:32] + "..."
	}
	fmt.Fprintf(os.Stderr, "\r[%s] %d/%d  %s", bar, done, total, label)
	if done == total {
		fmt.Fprintln(os.Stderr)
	}
}

func peakPct(peak int) float64 {
	// Matches CUETools: (2*peak * 1000 / 65534) * 0.1
	return float64(peak*2*1000/65534) * 0.1
}

func ctdbStatus(matched, total int) string {
	switch {
	case total == 0:
		return "Unknown"
	case matched == total:
		return "Accurately ripped"
	case matched == 0:
		return "Inaccurate"
	default:
		return "Differs"
	}
}

func arStatus(r cuetools.ARTrackResult) string {
	if r.Accurate {
		return "Accurately ripped"
	}
	if r.Silent {
		return "Silent track"
	}
	return "No match"
}

func printLog(result cuetools.VerificationResult, showAccurateRip bool) {
	// ── Header ──────────────────────────────────────────────────────────────
	now := time.Now()
	fmt.Printf("[CUETools log; Date: %s; Version: 0.0.1]\n", now.Format("1/2/2006 3:04:05 PM"))

	// ── CTDB section ────────────────────────────────────────────────────────
	tocID := cuetools.ComputeTOCID(result.TOC)
	foundStr := "not found."
	if result.MatchedEntry != nil {
		foundStr = "found."
	}
	fmt.Printf("[CTDB TOCID: %s] %s\n", tocID, foundStr)
	fmt.Println("Track | CTDB Status")

	n := len(result.Tracks)
	total := result.TotalEntries
	for i := range n {
		matched := 0
		for _, m := range result.CTDBMatches {
			if i < len(m.TrackMatch) && m.TrackMatch[i] {
				matched += m.Entry.Confidence
			}
		}
		fmt.Printf("%3d   | (%d/%d) %s\n", i+1, matched, total, ctdbStatus(matched, total))
	}

	// ── AccurateRip section ──────────────────────────────────────────────────
	if showAccurateRip || result.ARFound || len(result.ARResults) > 0 {
		arID := cuetools.ComputeAccurateRipID(result.TOC)
		arFoundStr := "disk not present in database."
		if result.ARFound {
			arFoundStr = "found."
		}
		fmt.Printf("[AccurateRip ID: %s] %s\n", arID, arFoundStr)

		if result.ARFound && len(result.ARResults) > 0 {
			// Determine field width from max total.
			var maxTotal uint32
			for _, r := range result.ARResults {
				if r.Total > maxTotal {
					maxTotal = r.Total
				}
			}
			w := 1
			if maxTotal >= 100 {
				w = 3
			} else if maxTotal >= 10 {
				w = 2
			}
			fmt.Println("Track   [  CRC   |   V2   ] Status")
			for i, r := range result.ARResults {
				fmt.Printf(" %02d     [%08x|%08x] (%0*d+%0*d/%0*d) %s\n",
					i+1, r.V1, r.V2,
					w, r.ConfV1, w, r.ConfV2, w, r.Total,
					arStatus(r))
			}
		}
	}

	// ── Track CRC table ──────────────────────────────────────────────────────
	fmt.Println()
	fmt.Println("Track Peak [ CRC32  ] [W/O NULL]")
	fmt.Printf(" --   %4.1f [%08X] [%08X]\n",
		peakPct(result.DiscPeak), result.DiscCRC32, result.DiscCRCWoNull)
	for i, t := range result.Tracks {
		fmt.Printf(" %02d   %4.1f [%08X] [%08X]\n",
			i+1, peakPct(t.Peak), t.TrackCRC32, t.CRCWoNull)
	}
}

func main() {
	path := flag.String("path", "", "path to folder containing FLAC tracks")
	ar := flag.Bool("accuraterip", false, "show AccurateRip disc ID")
	flag.Parse()

	if *path == "" {
		fmt.Println("Usage: cuetools-go -path <flac-folder> [-accuraterip]")
		os.Exit(1)
	}

	result, err := cuetools.VerifyFLACFolder(*path, "http://db.cuetools.net", trackProgress)
	if err != nil {
		fmt.Printf("Verification error: %v\n", err)
		os.Exit(2)
	}

	printLog(result, *ar)

	if result.MatchedEntry == nil {
		os.Exit(3)
	}
	os.Exit(0)
}
