package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/httpapi"
	"civic-ai-relay/internal/relay"
	"civic-ai-relay/internal/secret"
	"civic-ai-relay/internal/store"
)

type Application struct {
	Handler  http.Handler
	Store    *store.Store
	Registry *relay.Registry
	Server   *http.Server
}

func buildApplication(configPath string) (*Application, error) {
	settings, _, _, err := config.Ensure(configPath)
	if err != nil {
		return nil, err
	}
	box, err := secret.New(settings.EncryptionKey)
	if err != nil {
		return nil, err
	}
	database, err := store.Open(settings.DBPath, box)
	if err != nil {
		return nil, err
	}
	registry := relay.NewRegistry(database, settings.ConnectTimeout, settings.ReadTimeout, settings.WriteTimeout, settings.PoolTimeout)
	service := relay.NewService(database, registry, func() config.Settings { return settings })
	admin := httpapi.NewAdminHandler(database, service, settings, settings.AdminAPIKey)
	handler := httpapi.NewServer(service, settings.MaxBodyBytes, admin)
	return &Application{Handler: handler, Store: database, Registry: registry}, nil
}

func (a *Application) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if a.Server != nil {
		_ = a.Server.Shutdown(ctx)
	}
	if a.Store != nil {
		return a.Store.Close()
	}
	return nil
}

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
	app, err := buildApplication(configPath)
	if err != nil {
		log.Fatal(err)
	}
	server := newServer(net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port)), app.Handler)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: handler}
}
