package cuetools

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CDTrack is one entry in a TOC.
type CDTrack struct {
	Number  int  `json:"number"`
	Start   int  `json:"start"`  // sector (LBA)
	Length  int  `json:"length"` // sector count
	IsAudio bool `json:"is_audio"`
}

// CDImageLayout is a CUETools TOC layout.
type CDImageLayout struct {
	Tracks      []CDTrack `json:"tracks"`
	FirstAudio  int       `json:"first_audio"`
	AudioTracks int       `json:"audio_tracks"`
}

// Length returns total disc length in sectors.
func (cd CDImageLayout) Length() int {
	if len(cd.Tracks) == 0 {
		return 0
	}
	last := cd.Tracks[len(cd.Tracks)-1]
	return last.Start + last.Length
}

// ToString returns the CUETools TOC string representation.
func (cd CDImageLayout) ToString() string {
	parts := make([]string, 0, len(cd.Tracks)+1)
	for _, t := range cd.Tracks {
		prefix := ""
		if !t.IsAudio {
			prefix = "-"
		}
		parts = append(parts, fmt.Sprintf("%s%d", prefix, t.Start))
	}
	parts = append(parts, fmt.Sprintf("%d", cd.Length()))
	return strings.Join(parts, ":")
}

// CTDBResponse maps the CTDB XML response.
type CTDBResponse struct {
	XMLName   xml.Name    `xml:"ctdb"`
	Status    string      `xml:"status,attr"`
	UpdateURL string      `xml:"updateurl,attr"`
	Message   string      `xml:"message,attr"`
	Npar      int         `xml:"npar,attr"`
	Entries   []CTDBEntry `xml:"entry"`
	Metadata  []CTDBMeta  `xml:"metadata"`
}

// CTDBEntry models database entries.
type CTDBEntry struct {
	ID         int64  `xml:"id,attr"`
	CRC32      string `xml:"crc32,attr"`
	Confidence int    `xml:"confidence,attr"`
	Npar       int    `xml:"npar,attr"`
	Stride     int    `xml:"stride,attr"`
	HasParity  string `xml:"hasparity,attr"`
	Parity     string `xml:"parity,attr"`
	Syndrome   string `xml:"syndrome,attr"`
	TrackCRCs  string `xml:"trackcrcs,attr"`
	TOC        string `xml:"toc,attr"`
}

// CTDBMeta is minimal metadata structure.
type CTDBMeta struct {
	Source string `xml:"source,attr"`
	Artist string `xml:"artist,attr"`
	Title  string `xml:"title,attr"`
}

const ctdbServerURL = "http://db.cuetools.net"

// CTDBClient implements the CTDB network protocol.
type CTDBClient struct {
	BaseURL    string
	UserAgent  string
	HTTPClient *http.Client
}

// NewCTDBClient constructs a ready client.
func NewCTDBClient() *CTDBClient {
	return &CTDBClient{
		BaseURL:    ctdbServerURL,
		UserAgent:  "cuetools-go/0.1",
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Lookup requests /lookup2.php and parses XML.
func (c *CTDBClient) Lookup(toc CDImageLayout, ctdb, fuzzy bool, metadata string) (*CTDBResponse, error) {
	if metadata == "" {
		metadata = "none"
	}
	queryURL := fmt.Sprintf("%s/lookup2.php?version=3&ctdb=%d&metadata=%s&fuzzy=%d&toc=%s",
		c.BaseURL,
		boolToInt(ctdb),
		url.QueryEscape(metadata),
		boolToInt(fuzzy),
		url.QueryEscape(toc.ToString()),
	)
	res, err := c.HTTPClient.Get(queryURL)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ctdb lookup failed status %d", res.StatusCode)
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	var r CTDBResponse
	if err := xml.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ToDBEntry converts CTDBEntry -> DBEntry (application)
func (e CTDBEntry) ToDBEntry() (DBEntry, error) {
	db := DBEntry{
		ID:         e.ID,
		Confidence: e.Confidence,
		Npar:       e.Npar,
		Stride:     e.Stride * 2,
		HasParity:  strings.TrimSpace(e.HasParity) == "1" || strings.EqualFold(strings.TrimSpace(e.HasParity), "true"),
		TOC:        e.TOC,
	}
	var err error
	db.CRC32, err = parseHex32(e.CRC32)
	if err != nil {
		return db, err
	}
	if e.TrackCRCs != "" {
		for _, v := range strings.Fields(e.TrackCRCs) {
			t, err := parseHex32(v)
			if err != nil {
				return db, err
			}
			db.TrackCRCs = append(db.TrackCRCs, t)
		}
	}
	if strings.TrimSpace(e.HasParity) != "" {
		db.HasParity = true
		db.HasParityURL = strings.TrimSpace(e.HasParity)
	}
	// parity and syndrome data are not needed for lookup-only verification in this implementation.
	return db, nil
}

func parseHex32(inp string) (uint32, error) {
	if strings.TrimSpace(inp) == "" {
		return 0, nil
	}
	var val uint32
	_, err := fmt.Sscanf(inp, "%x", &val)
	return val, err
}

// ComputeTOCID returns the CTDB TOCID string for a disc layout.
// Algorithm: SHA1 of the hex-encoded relative track offsets padded to 100 entries,
// Base64-encoded with CUETools substitutions (+→. /→_ =→-).
func ComputeTOCID(layout CDImageLayout) string {
	if len(layout.Tracks) == 0 {
		return ""
	}
	first := layout.Tracks[0]
	last := layout.Tracks[len(layout.Tracks)-1]
	var sb strings.Builder
	for i := 1; i < len(layout.Tracks); i++ {
		fmt.Fprintf(&sb, "%08X", layout.Tracks[i].Start-first.Start)
	}
	fmt.Fprintf(&sb, "%08X", last.Start+last.Length-first.Start)
	sb.WriteString(strings.Repeat("0", (100-len(layout.Tracks))*8))
	h := sha1.Sum([]byte(sb.String()))
	b64 := base64.StdEncoding.EncodeToString(h[:])
	b64 = strings.ReplaceAll(b64, "+", ".")
	b64 = strings.ReplaceAll(b64, "/", "_")
	b64 = strings.ReplaceAll(b64, "=", "-")
	return b64
}

func digitSum(n int) int {
	s := 0
	for n > 0 {
		s += n % 10
		n /= 10
	}
	return s
}

// calcARDiscIDs returns the three uint32 components of the AccurateRip disc ID.
func calcARDiscIDs(layout CDImageLayout) (discID1, discID2, cddbID uint32) {
	if len(layout.Tracks) == 0 {
		return
	}
	base := layout.Tracks[0].Start
	last := layout.Tracks[len(layout.Tracks)-1]
	discLen := last.Start + last.Length - base

	var num uint32
	for _, t := range layout.Tracks {
		if !t.IsAudio {
			continue
		}
		rel := uint32(t.Start - base)
		discID1 += rel
		num++
		maxRel := rel
		if maxRel == 0 {
			maxRel = 1
		}
		discID2 += maxRel * num
	}
	discID1 += uint32(discLen)
	num++
	maxLen := uint32(discLen)
	if maxLen == 0 {
		maxLen = 1
	}
	discID2 += maxLen * num

	cddbSum := 0
	for _, t := range layout.Tracks {
		if t.IsAudio {
			cddbSum += digitSum(t.Start / 75)
		}
	}
	discLenSecs := (last.Start+last.Length)/75 - layout.Tracks[0].Start/75
	cddbID = uint32(cddbSum%255)<<24 | uint32(discLenSecs)<<8 | uint32(len(layout.Tracks))
	return
}

// ComputeAccurateRipID returns the AccurateRip disc identifier string (three hex values separated by dashes).
func ComputeAccurateRipID(layout CDImageLayout) string {
	if len(layout.Tracks) == 0 {
		return ""
	}
	id1, id2, cddb := calcARDiscIDs(layout)
	return fmt.Sprintf("%08x-%08x-%08x", id1, id2, cddb)
}

// DBEntry is a processed database result.
type DBEntry struct {
	ID           int64    `json:"id"`
	Confidence   int      `json:"confidence"`
	CRC32        uint32   `json:"crc32"`
	Npar         int      `json:"npar"`
	Stride       int      `json:"stride"`
	TrackCRCs    []uint32 `json:"track_crcs"`
	HasParity    bool     `json:"has_parity"`
	HasParityURL string   `json:"has_parity_url"`
	Syndrome     []byte   `json:"syndrome"`
	TOC          string   `json:"toc"`
}
