package testutil

import (
	"net/http"
	"net/http/httptest"
)

func Server(h http.Handler) *httptest.Server {
	return httptest.NewServer(h)
}
