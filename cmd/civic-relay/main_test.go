package main

import "testing"

func TestNewServerUsesProvidedAddress(t *testing.T) {
	server := newServer("127.0.0.1:8000", nil)
	if server.Addr != "127.0.0.1:8000" {
		t.Fatalf("server address = %q, want %q", server.Addr, "127.0.0.1:8000")
	}
}
