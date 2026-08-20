package multimodal

import (
	"encoding/binary"
	"testing"
)

// The WAV duration calculation multiplied two uint32 values and only then
// widened to uint64:
//
//	durationMs := uint64(dataSize) * 1000 / uint64(sampleRate*bytesPerSample)
//
// sampleRate comes straight from the payload and was only checked non-zero, so
// a crafted header whose product wraps to exactly zero divided by zero. That is
// reachable from Kernel.Admit on caller-supplied bytes, with no recover on the
// path and the kernel mutex held.

// wavHeader builds a minimal 44-byte RIFF/WAVE header with the given fields.
func wavHeader(sampleRate uint32, channels, bitsPerSample uint16, dataSize uint32) []byte {
	buf := make([]byte, 44)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 36+dataSize)
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], channels)
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	byteRate := sampleRate * uint32(channels) * uint32(bitsPerSample) / 8
	binary.LittleEndian.PutUint32(buf[28:32], byteRate)
	binary.LittleEndian.PutUint16(buf[32:34], channels*bitsPerSample/8)
	binary.LittleEndian.PutUint16(buf[34:36], bitsPerSample)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], dataSize)
	// The data chunk's bytes must actually be present: a declared size larger
	// than the payload is rejected as a partial payload long before the
	// duration arithmetic, which is how the first version of this test passed
	// without ever reaching the code it was written to exercise.
	return append(buf, make([]byte, dataSize)...)
}

func TestExtractWAVDoesNotDivideByZeroOnAWrappedSampleRate(t *testing.T) {
	// 2^30 * (16/8 * 2) == 2^32 == 0 in uint32.
	tests := []struct {
		name                    string
		sampleRate              uint32
		channels, bitsPerSample uint16
	}{
		{"wraps to zero", 1 << 30, 2, 16},
		{"wraps to zero via 8-bit", 1 << 31, 2, 8},
		{"max sample rate", ^uint32(0), 2, 16},
		{"large but valid", 192000, 2, 16},
		{"minimal", 8000, 1, 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := wavHeader(test.sampleRate, test.channels, test.bitsPerSample, 1024)
			// The contract is only "does not panic": a wrapped rate may well be
			// rejected as malformed, which is the right answer.
			_, _ = extract(kindWAV, payload, false, "rev-1")
		})
	}
}
