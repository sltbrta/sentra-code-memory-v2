package projections

import (
	"encoding/binary"
	"fmt"
	"math"
)

// packFloat32LE encodes v as little-endian float32 bytes.
func packFloat32LE(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	out := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(x))
	}
	return out
}

// unpackFloat32LE decodes little-endian float32 bytes into a vector of length dim.
// When dim <= 0, dim is inferred as len(b)/4. Blob length must be dim*4.
func unpackFloat32LE(b []byte, dim int) ([]float32, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("projections: empty embedding blob")
	}
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("projections: embedding blob length %d not multiple of 4", len(b))
	}
	n := len(b) / 4
	if dim <= 0 {
		dim = n
	}
	if n != dim {
		return nil, fmt.Errorf("projections: embedding blob dim %d != declared %d", n, dim)
	}
	out := make([]float32, dim)
	for i := 0; i < dim; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}
