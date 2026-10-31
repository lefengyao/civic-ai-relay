package upstream

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type oneByteReader struct{ source io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.source.Read(p)
}

func TestSSEReaderPreservesCRLFAndJoinsData(t *testing.T) {
	const comment = ": thinking\r\n\r\n"
	const event = "event: delta\r\ndata: first\r\ndata: second\r\n\r\n"
	reader := NewSSEReader(strings.NewReader(comment+event), 1024)
	frame, err := reader.Next()
	if err != nil || !frame.Comment || string(frame.Raw) != comment || len(frame.Data) != 0 {
		t.Fatalf("comment frame=%+v err=%v", frame, err)
	}
	frame, err = reader.Next()
	if err != nil || frame.Comment || frame.Event != "delta" || string(frame.Data) != "first\nsecond" || string(frame.Raw) != event {
		t.Fatalf("data frame=%+v err=%v", frame, err)
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after complete frames: %v, want EOF", err)
	}
}

func TestSSEReaderHandlesChunkBoundariesAndEmptyEvents(t *testing.T) {
	reader := NewSSEReader(oneByteReader{source: strings.NewReader("\n\nevent: ping\n\ndata:\n\ndata: ok\n\n")}, 1024)
	first, err := reader.Next()
	if err != nil || first.Event != "ping" || string(first.Raw) != "event: ping\n\n" || len(first.Data) != 0 {
		t.Fatalf("empty data frame=%+v err=%v", first, err)
	}
	second, err := reader.Next()
	if err != nil || string(second.Raw) != "data:\n\n" || len(second.Data) != 0 {
		t.Fatalf("chunked frame=%+v err=%v", second, err)
	}
	third, err := reader.Next()
	if err != nil || string(third.Raw) != "data: ok\n\n" || string(third.Data) != "ok" {
		t.Fatalf("second chunked frame=%+v err=%v", third, err)
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after complete frames: %v, want EOF", err)
	}
}

func TestSSEReaderRejectsUnterminatedAndOversizeFrames(t *testing.T) {
	reader := NewSSEReader(strings.NewReader("data: unfinished"), 1024)
	if _, err := reader.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unterminated event error=%v", err)
	}
	reader = NewSSEReader(strings.NewReader("data: "+strings.Repeat("x", 20)+"\n\n"), 16)
	if _, err := reader.Next(); !errors.Is(err, ErrSSEFrameTooLarge) {
		t.Fatalf("oversize event error=%v", err)
	}
}
