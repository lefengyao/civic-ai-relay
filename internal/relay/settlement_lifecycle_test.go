package relay_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/testutil"
	"civic-ai-relay/internal/upstream"
)

type settlementFakeClient struct {
	body string
	err  error
}

func (f *settlementFakeClient) ChatJSON(context.Context, map[string]any) ([]byte, error) {
	return []byte(f.body), f.err
}

func (f *settlementFakeClient) Stream(context.Context, map[string]any) (*http.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(f.body)),
	}, nil
}

func TestCanceledSettlementReleasesKey(t *testing.T) {
	f := testutil.NewRelayFixture(t, nil)
	req := relay.Request{Token: f.Key.Token, Model: "m", InputText: "hi", Payload: map[string]any{}}
	lease, _, err := f.Service.Begin(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lease.Close(ctx, relay.Outcome{Status: "aborted"}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.Store.RecentRequests(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["status"] != "aborted" {
		t.Fatalf("rows=%v", rows)
	}
	next, _, err := f.Service.Begin(context.Background(), req)
	if err != nil {
		t.Fatalf("key still blocked: %v", err)
	}
	if err := next.Close(context.Background(), relay.Outcome{Status: "aborted"}); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatedSettlementChargesOnce(t *testing.T) {
	f := testutil.NewRelayFixture(t, nil)
	lease, _, err := f.Service.Begin(context.Background(), relay.Request{Token: f.Key.Token, Model: "m", InputText: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	outcome := relay.Outcome{Status: "completed", InputTokens: 7, AmountMicroyuan: 7, HTTPStatus: http.StatusOK}
	if err := lease.Close(context.Background(), outcome); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(context.Background(), outcome); err != nil {
		t.Fatal(err)
	}
	rows, err := f.Store.RecentRequests(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["amount_microyuan"] != int64(7) {
		t.Fatalf("rows=%v", rows)
	}
}

func TestFailedUpstreamKeeps503(t *testing.T) {
	f := testutil.NewRelayFixture(t, &settlementFakeClient{err: &upstream.Error{Code: "upstream_unavailable", Status: http.StatusServiceUnavailable}})
	_, err := f.Service.Chat(context.Background(), relay.Request{Token: f.Key.Token, Model: "m", InputText: "hi", Payload: map[string]any{}})
	if err == nil {
		t.Fatal("expected upstream failure")
	}
	var upstreamErr *upstream.Error
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("error=%v, want upstream error", err)
	}
	rows, err := f.Store.RecentRequests(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["status"] != "failed" || rows[0]["upstream_status"] != int64(http.StatusServiceUnavailable) {
		t.Fatalf("rows=%v", rows)
	}
}

func TestUpstreamFailureKeeps429OrUnknownStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus any
		wantCode   string
	}{
		{name: "rate limited", err: &upstream.Error{Code: "upstream_rate_limited", Status: http.StatusTooManyRequests}, wantStatus: int64(http.StatusTooManyRequests), wantCode: "upstream_rate_limited"},
		{name: "no response headers", err: &upstream.Error{Code: "upstream_timeout"}, wantStatus: nil, wantCode: "upstream_timeout"},
		{name: "client canceled", err: context.Canceled, wantStatus: nil, wantCode: "client_canceled"},
		{name: "client canceled after headers", err: &upstream.Error{Code: "client_canceled", Status: http.StatusAccepted}, wantStatus: int64(http.StatusAccepted), wantCode: "client_canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testutil.NewRelayFixture(t, &settlementFakeClient{err: tc.err})
			_, err := f.Service.Chat(context.Background(), relay.Request{Token: f.Key.Token, Model: "m", InputText: "hi"})
			if err == nil {
				t.Fatal("expected upstream failure")
			}
			rows, err := f.Store.RecentRequests(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			wantOutcome := "failed"
			if tc.wantCode == "client_canceled" {
				wantOutcome = "aborted"
			}
			if len(rows) != 1 || rows[0]["status"] != wantOutcome || rows[0]["upstream_status"] != tc.wantStatus || rows[0]["error_code"] != tc.wantCode {
				t.Fatalf("rows=%v", rows)
			}
		})
	}
}
