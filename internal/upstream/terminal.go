package upstream

import (
	"errors"
	"io"
)

// ChatTerminal requires the protocol's [DONE] marker; EOF alone is incomplete.
func ChatTerminal(done bool, readErr error, canceled bool) (status, errorCode string) {
	if canceled {
		return "aborted", "client_canceled"
	}
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if readErr != nil {
		var upstreamErr *Error
		if errors.As(readErr, &upstreamErr) && upstreamErr.Code != "" {
			if upstreamErr.Code == "client_canceled" {
				return "aborted", upstreamErr.Code
			}
			return "failed", upstreamErr.Code
		}
		return "failed", "upstream_stream_read_failed"
	}
	if !done {
		return "failed", "upstream_stream_incomplete"
	}
	return "completed", ""
}

// ResponsesBridgeTerminal maps a Chat stream to its Responses stream outcome.
func ResponsesBridgeTerminal(done bool, finishReason string, readErr error, canceled bool) (status, errorCode, responseEvent string) {
	status, errorCode = ChatTerminal(done, readErr, canceled)
	if status == "aborted" {
		return status, errorCode, ""
	}
	if status == "failed" {
		return status, errorCode, "response.failed"
	}
	if finishReason == "length" {
		return "failed", "upstream_incomplete", "response.incomplete"
	}
	return "completed", "", "response.completed"
}

// ResponsesTerminal uses only the native Responses terminal event as success.
func ResponsesTerminal(event string, readErr error, canceled bool) (status, errorCode string) {
	if canceled {
		return "aborted", "client_canceled"
	}
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if readErr != nil {
		var upstreamErr *Error
		if errors.As(readErr, &upstreamErr) && upstreamErr.Code != "" {
			if upstreamErr.Code == "client_canceled" {
				return "aborted", upstreamErr.Code
			}
			return "failed", upstreamErr.Code
		}
		return "failed", "upstream_stream_read_failed"
	}
	switch event {
	case "response.completed":
		return "completed", ""
	case "response.failed":
		return "failed", "upstream_response_failed"
	case "response.incomplete":
		return "failed", "upstream_incomplete"
	default:
		return "failed", "upstream_stream_incomplete"
	}
}
