package reviewprovider

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
)

// Compression is only a retained representation, never an authority cache.
// Small bodies avoid the CPU cost. The bounded writer abandons work that cannot
// save at least one eighth of the body. No compressor pool retains large buffers.
const conditionalCompressionThreshold = 32 << 10

var errConditionalRepresentation = errors.New("invalid conditional representation")

type conditionalWriter struct {
	bytes.Buffer
	limit int
}

func (w *conditionalWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.Len() {
		return 0, errConditionalRepresentation
	}
	return w.Buffer.Write(p)
}

func encodeConditionalBody(data []byte) ([]byte, bool) {
	if len(data) >= conditionalCompressionThreshold {
		w := &conditionalWriter{limit: len(data) * 7 / 8}
		z, _ := zlib.NewWriterLevel(w, zlib.BestSpeed)
		_, writeErr := z.Write(data)
		closeErr := z.Close()
		if writeErr == nil && closeErr == nil && w.Len() <= w.limit {
			return bytes.Clone(w.Bytes()), true
		}
	}
	return bytes.Clone(data), false
}

// Called only after a fresh matching 304, outside cacheMu. Both the allocation
// and expanded stream are bounded by the original REST body ceiling. Exact size,
// checksum, EOF and trailing-input checks refuse corrupt representations.
func (e conditionalEntry) decodedBody() ([]byte, error) {
	if e.expandedSize < 0 || e.expandedSize > githubMaxBody {
		return nil, errConditionalRepresentation
	}
	if !e.compressed {
		if len(e.body) != e.expandedSize {
			return nil, errConditionalRepresentation
		}
		return e.body, nil
	}
	input := bytes.NewReader(e.body)
	z, err := zlib.NewReader(input)
	if err != nil {
		return nil, errConditionalRepresentation
	}
	defer z.Close()
	body := make([]byte, e.expandedSize)
	if _, err := io.ReadFull(z, body); err != nil {
		return nil, errConditionalRepresentation
	}
	var extra [1]byte
	if n, err := z.Read(extra[:]); n != 0 || err != io.EOF || input.Len() != 0 {
		return nil, errConditionalRepresentation
	}
	return body, nil
}
