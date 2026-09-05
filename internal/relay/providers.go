package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"civic-ai-relay/internal/store"
	"civic-ai-relay/internal/upstream"
)

type Registry struct {
	store                      *store.Store
	mu                         sync.Mutex
	clients                    map[int64]*upstream.Client
	connect, read, write, pool time.Duration
}

func NewRegistry(repo *store.Store, connect, read, write, pool time.Duration) *Registry {
	return &Registry{store: repo, clients: make(map[int64]*upstream.Client), connect: connect, read: read, write: write, pool: pool}
}

func (r *Registry) forProvider(ctx context.Context, providerID int64) (*upstream.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if client := r.clients[providerID]; client != nil {
		return client, nil
	}
	provider, err := r.store.GetProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if !provider.Enabled {
		return nil, errors.New("provider is disabled")
	}
	key, err := r.store.ProviderAPIKey(ctx, providerID)
	if err != nil {
		return nil, err
	}
	client := upstream.New(provider.BaseURL, key, r.connect, r.read, r.write, r.pool)
	r.clients[providerID] = client
	return client, nil
}
func (r *Registry) EvictProvider(providerID int64) {
	r.mu.Lock()
	delete(r.clients, providerID)
	r.mu.Unlock()
}

func (r *Registry) ForProvider(ctx context.Context, providerID int64) (UpstreamClient, error) {
	return r.forProvider(ctx, providerID)
}
func (r *Registry) SyncProvider(ctx context.Context, providerID int64) ([]string, error) {
	client, err := r.forProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	data, err := client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}
	provider, err := r.store.GetProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	imported := make([]string, 0)
	for _, item := range payload.Data {
		if item.ID == "" {
			continue
		}
		name := provider.Name + "/" + item.ID
		if _, err := r.store.GetModelByPublicName(ctx, name); err == nil {
			continue
		}
		if err := r.store.CreateImportedModel(ctx, store.NewModel{ProviderID: providerID, PublicName: name, UpstreamName: item.ID, Enabled: false}); err != nil {
			return nil, err
		}
		imported = append(imported, name)
	}
	return imported, nil
}
