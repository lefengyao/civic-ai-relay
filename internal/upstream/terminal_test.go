package upstream

import (
	"errors"
	"io"
	"testing"
)

func TestChatTerminal(t *testing.T) {
	readErr := errors.New("body read failed")
	for _, tc := range []struct {
		name      string
		done      bool
		readErr   error
		canceled  bool
		wantState string
		wantCode  string
	}{
		{"done", true, nil, false, "completed", ""},
		{"eof without done", false, nil, false, "failed", "upstream_stream_incomplete"},
		{"explicit eof without done", false, io.EOF, false, "failed", "upstream_stream_incomplete"},
		{"read error", false, readErr, false, "failed", "upstream_stream_read_failed"},
		{"read error after done", true, readErr, false, "failed", "upstream_stream_read_failed"},
		{"canceled before done", false, nil, true, "aborted", "client_canceled"},
		{"cancellation wins over read error", false, readErr, true, "aborted", "client_canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, code := ChatTerminal(tc.done, tc.readErr, tc.canceled)
			if state != tc.wantState || code != tc.wantCode {
				t.Fatalf("ChatTerminal() = (%q, %q), want (%q, %q)", state, code, tc.wantState, tc.wantCode)
			}
		})
	}
}

func TestResponsesBridgeTerminal(t *testing.T) {
	readErr := errors.New("body read failed")
	for _, tc := range []struct {
		name, finishReason string
		done               bool
		readErr            error
		canceled           bool
		wantState          string
		wantCode           string
		wantEvent          string
	}{
		{"done", "stop", true, nil, false, "completed", "", "response.completed"},
		{"length with done", "length", true, nil, false, "failed", "upstream_incomplete", "response.incomplete"},
		{"length before done", "length", false, nil, false, "failed", "upstream_stream_incomplete", "response.failed"},
		{"eof without done", "", false, nil, false, "failed", "upstream_stream_incomplete", "response.failed"},
		{"explicit eof without done", "", false, io.EOF, false, "failed", "upstream_stream_incomplete", "response.failed"},
		{"read error", "", false, readErr, false, "failed", "upstream_stream_read_failed", "response.failed"},
		{"canceled", "length", true, readErr, true, "aborted", "client_canceled", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, code, event := ResponsesBridgeTerminal(tc.done, tc.finishReason, tc.readErr, tc.canceled)
			if state != tc.wantState || code != tc.wantCode || event != tc.wantEvent {
				t.Fatalf("ResponsesBridgeTerminal() = (%q, %q, %q), want (%q, %q, %q)", state, code, event, tc.wantState, tc.wantCode, tc.wantEvent)
			}
		})
	}
}

func TestResponsesTerminal(t *testing.T) {
	readErr := errors.New("body read failed")
	for _, tc := range []struct {
		name, event string
		readErr     error
		canceled    bool
		wantState   string
		wantCode    string
	}{
		{"completed", "response.completed", nil, false, "completed", ""},
		{"failed", "response.failed", nil, false, "failed", "upstream_response_failed"},
		{"incomplete", "response.incomplete", nil, false, "failed", "upstream_incomplete"},
		{"unknown event", "response.output_text.delta", nil, false, "failed", "upstream_stream_incomplete"},
		{"read error", "response.completed", readErr, false, "failed", "upstream_stream_read_failed"},
		{"canceled", "response.completed", readErr, true, "aborted", "client_canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, code := ResponsesTerminal(tc.event, tc.readErr, tc.canceled)
			if state != tc.wantState || code != tc.wantCode {
				t.Fatalf("ResponsesTerminal() = (%q, %q), want (%q, %q)", state, code, tc.wantState, tc.wantCode)
			}
		})
	}
}
