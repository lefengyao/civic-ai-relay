package upstream

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

const defaultSSEFrameMaxBytes = 1 << 20

var ErrSSEFrameTooLarge = errors.New("upstream SSE frame too large")

type SSEFrame struct {
	Event   string
	Data    []byte
	Raw     []byte
	Comment bool
}

type SSEReader struct {
	reader   *bufio.Reader
	maxBytes int
}

func NewSSEReader(r io.Reader, maxBytes int) *SSEReader {
	if maxBytes <= 0 || maxBytes > defaultSSEFrameMaxBytes {
		maxBytes = defaultSSEFrameMaxBytes
	}
	return &SSEReader{reader: bufio.NewReader(r), maxBytes: maxBytes}
}

func (s *SSEReader) Next() (SSEFrame, error) {
	var frame SSEFrame
	var raw []byte
	var data bytes.Buffer
	var sawData, sawComment, sawField bool
	for {
		line, err := s.readLine(&raw)
		if err != nil {
			if errors.Is(err, io.EOF) && len(raw) > 0 {
				return SSEFrame{}, io.ErrUnexpectedEOF
			}
			return SSEFrame{}, err
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			if !sawData && !sawComment && !sawField {
				raw = nil
				frame = SSEFrame{}
				continue
			}
			frame.Raw = raw
			frame.Data = data.Bytes()
			frame.Comment = sawComment && !sawData && !sawField
			return frame, nil
		}
		if line[0] == ':' {
			sawComment = true
			continue
		}
		field, value, found := bytes.Cut(line, []byte{':'})
		if !found {
			value = nil
		}
		value = bytes.TrimPrefix(value, []byte{' '})
		sawField = true
		switch string(field) {
		case "data":
			if sawData {
				data.WriteByte('\n')
			}
			data.Write(value)
			sawData = true
		case "event":
			frame.Event = string(value)
		}
	}
}

func (s *SSEReader) readLine(raw *[]byte) ([]byte, error) {
	start := len(*raw)
	for {
		part, err := s.reader.ReadSlice('\n')
		if len(*raw)+len(part) > s.maxBytes {
			return nil, ErrSSEFrameTooLarge
		}
		*raw = append(*raw, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return (*raw)[start:], nil
	}
}
