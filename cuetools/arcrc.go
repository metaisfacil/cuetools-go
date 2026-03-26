package cuetools

// ComputeARTrackCRC32 computes the AccurateRip V2 CRC for a track's stereo-pair samples.
func ComputeARTrackCRC32(samples []uint32) uint32 {
	var low, high uint64
	for i, sample := range samples {
		p := uint64(i + 1)
		prod := uint64(sample) * p
		low += uint64(uint32(prod))
		high += uint64(uint32(prod >> 32))
	}
	return uint32(low + high)
}
