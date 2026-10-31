package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/requestmeta"
	"civic-ai-relay/internal/upstream"
)

const streamWriteTimeout = 30 * time.Second

type streamResultKind uint8

const (
	streamHeader streamResultKind = iota
	streamFrame
	streamReadError
	streamWriteError
	streamAdmissionError
	streamCanceled
)

type streamResult struct {
	kind     streamResultKind
	response *http.Response
	frame    upstream.SSEFrame
	err      error
}

type streamSession struct {
	w          http.ResponseWriter
	controller *http.ResponseController
	ctx        context.Context
	cancel     context.CancelFunc
	prepared   *relay.PreparedStream
	admission  func() error
	results    chan streamResult
	done       chan struct{}
	heartbeat  *time.Ticker
	check      *time.Ticker
}

func newStreamSession(w http.ResponseWriter, r *http.Request, prepared *relay.PreparedStream, admission func() error) (*streamSession, error) {
	ctx, cancel := context.WithCancel(r.Context())
	s := &streamSession{
		w: w, controller: http.NewResponseController(w), ctx: ctx, cancel: cancel,
		prepared: prepared, admission: admission, results: make(chan streamResult, 1), done: make(chan struct{}),
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := s.write([]byte(": relay-open\n\n")); err != nil {
		cancel()
		prepared.Cancel()
		return nil, err
	}
	if prepared.HeartbeatInterval > 0 {
		s.heartbeat = time.NewTicker(prepared.HeartbeatInterval)
	}
	if admission != nil {
		s.check = time.NewTicker(250 * time.Millisecond)
	}
	go s.produce()
	return s, nil
}

func (s *streamSession) produce() {
	defer close(s.done)
	defer close(s.results)
	response, err := s.prepared.Open()
	if err != nil {
		s.send(streamResult{kind: streamReadError, err: err})
		return
	}
	defer response.Body.Close()
	if !s.send(streamResult{kind: streamHeader, response: response}) {
		return
	}
	reader := upstream.NewSSEReader(response.Body, 1<<20)
	for {
		frame, err := reader.Next()
		if err != nil {
			s.send(streamResult{kind: streamReadError, err: err})
			return
		}
		if trace := requestmeta.FromContext(s.ctx); trace != nil {
			trace.MarkUpstream()
		}
		if !s.send(streamResult{kind: streamFrame, frame: frame}) {
			return
		}
		if bytes.Equal(bytes.TrimSpace(frame.Data), []byte("[DONE]")) {
			return
		}
	}
}

func (s *streamSession) send(result streamResult) bool {
	select {
	case s.results <- result:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// Next performs all downstream writes on the handler goroutine. The producer
// only opens and reads upstream, so heartbeats cannot interleave with JSON.
func (s *streamSession) Next() streamResult {
	var heartbeat, check <-chan time.Time
	if s.heartbeat != nil {
		heartbeat = s.heartbeat.C
	}
	if s.check != nil {
		check = s.check.C
	}
	for {
		select {
		case <-s.ctx.Done():
			return streamResult{kind: streamCanceled, err: s.ctx.Err()}
		case result, ok := <-s.results:
			if !ok {
				return streamResult{kind: streamReadError, err: io.EOF}
			}
			return result
		case <-heartbeat:
			if err := s.write([]byte(": relay-heartbeat\n\n")); err != nil {
				return streamResult{kind: streamWriteError, err: err}
			}
		case <-check:
			if err := s.admission(); err != nil {
				return streamResult{kind: streamAdmissionError, err: err}
			}
		}
	}
}

func (s *streamSession) write(data []byte) error {
	deadlineErr := s.controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	if deadlineErr != nil && !errors.Is(deadlineErr, http.ErrNotSupported) {
		return deadlineErr
	}
	n, err := s.w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = s.controller.Flush()
		if errors.Is(err, http.ErrNotSupported) {
			err = nil
		}
	}
	if deadlineErr == nil {
		if resetErr := s.controller.SetWriteDeadline(time.Time{}); err == nil && resetErr != nil && !errors.Is(resetErr, http.ErrNotSupported) {
			err = resetErr
		}
	}
	return err
}

func (s *streamSession) Close() {
	s.cancel()
	s.prepared.Cancel()
	if s.heartbeat != nil {
		s.heartbeat.Stop()
	}
	if s.check != nil {
		s.check.Stop()
	}
	<-s.done
}

func streamOpenFailure(err error, requestCtx context.Context) (status, code string, upstreamStatus int) {
	if errors.Is(requestCtx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "aborted", "client_canceled", 0
	}
	var upstreamErr *upstream.Error
	if errors.As(err, &upstreamErr) {
		return "failed", upstreamErr.Code, upstreamErr.Status
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "failed", "request_duration_exceeded", 0
	}
	return "failed", "upstream_connection_failed", 0
}

func writeChatStreamError(session *streamSession, code, requestID string) error {
	payload, _ := json.Marshal(map[string]any{"error": map[string]string{
		"type": "relay_error", "code": code, "message": "response stream interrupted", "request_id": requestID,
	}})
	return session.write(append(append([]byte("data: "), payload...), []byte("\n\n")...))
}
