package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"civic-ai-relay/internal/config"
)

func main() {
	configPath, err := config.DefaultPath(os.Getenv("CIVIC_RELAY_CONFIG_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	settings, bootstrapKey, created, err := config.Ensure(configPath)
	if err != nil {
		log.Fatal(err)
	}
	if created {
		// The credential itself is written only to the adjacent one-time file and
		// is intentionally never logged or included in process metadata.
		_ = bootstrapKey
		log.Printf("first start created bootstrap administrator key file: %s", filepath.Join(filepath.Dir(configPath), "bootstrap-admin-key.txt"))
	}
	server := newServer(net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port)), http.NotFoundHandler())
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: handler}
}
