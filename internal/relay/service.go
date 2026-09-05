package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/store"
	"civic-ai-relay/internal/upstream"
)

type UpstreamClient interface {
	ChatJSON(context.Context, map[string]any) ([]byte, error)
	Stream(context.Context, map[string]any) (*http.Response, error)
}

type ClientFactory interface {
	ForProvider(context.Context, int64) (UpstreamClient, error)
}

type Request struct {
	Token               string
	Model               string
	Messages            []any
	Payload             map[string]any
	InputText           string
	MaxTokens           int64
	MaxCompletionTokens int64
	Stream              bool
}

type Outcome struct {
	Status          string
	InputTokens     int64
	OutputTokens    int64
	AmountMicroyuan int64
	HTTPStatus      int
}

type Service struct {
	store    *store.Store
	clients  ClientFactory
	settings func() config.Settings
	global   *Gate
	keys     *KeyGates
}

func (s *Service) Models(ctx context.Context, token string) ([]store.Model, error) {
	key, err := s.store.AuthenticateClientKey(ctx, token)
	if err != nil || !key.Enabled {
		return nil, ErrUnauthorized
	}
	return s.store.AuthorizedModels(ctx, token)
}

func NewService(repo *store.Store, clients ClientFactory, settings func() config.Settings) *Service {
	limit := 1
	if settings != nil && settings().GlobalConcurrencyLimit > 0 {
		limit = settings().GlobalConcurrencyLimit
	}
	return &Service{store: repo, clients: clients, settings: settings, global: NewGate(limit), keys: NewKeyGates()}
}

type Lease struct {
	service     *Service
	reservation store.RequestReservation
	keyID       int64
	token       string
	model       store.Model
	mu          sync.Mutex
	released    bool
}

func (l *Lease) Close(ctx context.Context, outcome Outcome) error {
	if l == nil || l.service == nil {
		return nil
	}
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	l.mu.Unlock()
	if outcome.Status == "" {
		outcome.Status = "completed"
	}
	err := l.service.store.SettleRequest(ctx, l.reservation.ID, outcome.InputTokens, outcome.OutputTokens, outcome.AmountMicroyuan, outcome.Status)
	l.service.keys.Release(l.keyID)
	l.service.global.Release()
	return err
}

func (s *Service) Begin(ctx context.Context, req Request) (*Lease, store.Model, error) {
	if s == nil || s.store == nil || s.clients == nil || strings.TrimSpace(req.Model) == "" || strings.TrimSpace(req.Token) == "" {
		return nil, store.Model{}, ErrInvalidRequest
	}
	key, err := s.store.AuthenticateClientKey(ctx, req.Token)
	if err != nil || !key.Enabled {
		return nil, store.Model{}, ErrUnauthorized
	}
	models, err := s.store.AuthorizedModels(ctx, req.Token)
	if err != nil {
		return nil, store.Model{}, err
	}
	var model store.Model
	for _, candidate := range models {
		if candidate.PublicName == req.Model {
			model = candidate
			break
		}
	}
	if model.ID == 0 {
		return nil, store.Model{}, ErrModelNotAllowed
	}
	settings := config.Settings{GlobalConcurrencyLimit: 1, RPMLimit: 30, TokenLimit5H: 100000, TokenLimitDaily: 20000, MaxOutputTokens: 4096}
	if s.settings != nil {
		settings = s.settings()
	}
	if settings.GlobalConcurrencyLimit > 0 {
		s.global.SetLimit(settings.GlobalConcurrencyLimit)
	}
	if req.MaxTokens < 0 || req.MaxCompletionTokens < 0 {
		return nil, store.Model{}, ErrInvalidRequest
	}
	output := req.MaxTokens
	if req.MaxCompletionTokens > 0 {
		if output > 0 && output != req.MaxCompletionTokens {
			return nil, store.Model{}, ErrInvalidRequest
		}
		output = req.MaxCompletionTokens
	}
	if output <= 0 {
		output = int64(settings.MaxOutputTokens)
	}
	if output <= 0 || (settings.MaxOutputTokens > 0 && output > int64(settings.MaxOutputTokens)) {
		return nil, store.Model{}, ErrInvalidRequest
	}
	if !s.global.TryAcquire() {
		return nil, store.Model{}, ErrGlobalConcurrencyExceeded
	}
	if !s.keys.TryAcquire(key.ID, key.ConcurrencyLimit) {
		s.global.Release()
		return nil, store.Model{}, ErrKeyConcurrencyExceeded
	}
	stringFields := make([]string, 0, len(req.Messages))
	for _, message := range req.Messages {
		if encoded, marshalErr := json.Marshal(message); marshalErr == nil {
			stringFields = append(stringFields, string(encoded))
		}
	}
	reservation, err := s.store.ReserveRequest(ctx, store.ReserveInput{
		RequestID: requestID(), KeyID: key.ID, ModelID: model.ID, ProviderID: model.ProviderID,
		Stream: req.Stream, InputText: req.InputText, StringFields: stringFields,
		OutputTokenCeiling: output, MaxOutputTokens: output,
		InputPriceMicroyuan: valueOrZero(model.InputPriceMicroyuan), OutputPriceMicroyuan: valueOrZero(model.OutputPriceMicroyuan),
		RPMLimit: int64(settings.RPMLimit), TokenLimit5H: settings.TokenLimit5H, TokenLimitDaily: settings.TokenLimitDaily,
	})
	if err != nil {
		s.keys.Release(key.ID)
		s.global.Release()
		var quota *store.QuotaError
		if errors.As(err, &quota) && quota.Code == "concurrency_exceeded" {
			return nil, store.Model{}, ErrKeyConcurrencyExceeded
		}
		return nil, store.Model{}, err
	}
	return &Lease{service: s, reservation: reservation, keyID: key.ID, token: req.Token, model: model}, model, nil
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func (s *Service) Chat(ctx context.Context, req Request) ([]byte, error) {
	req.Stream = false
	lease, model, err := s.Begin(ctx, req)
	if err != nil {
		return nil, err
	}
	client, err := s.clients.ForProvider(ctx, model.ProviderID)
	if err != nil {
		_ = lease.Close(ctx, Outcome{Status: "failed"})
		return nil, err
	}
	payload := copyPayload(req.Payload)
	payload["model"] = model.UpstreamName
	payload["stream"] = false
	data, err := client.ChatJSON(ctx, payload)
	if err != nil {
		_ = lease.Close(ctx, Outcome{Status: "failed"})
		return nil, err
	}
	usage := struct {
		Usage upstream.Usage `json:"usage"`
	}{}
	if json.Unmarshal(data, &usage) != nil {
		_ = lease.Close(ctx, Outcome{Status: "failed"})
		return nil, &upstream.Error{Code: "upstream_response_invalid"}
	}
	amount := priceUsage(usage.Usage, model)
	if err := lease.Close(ctx, Outcome{Status: "completed", InputTokens: int64(usage.Usage.PromptTokens), OutputTokens: int64(usage.Usage.CompletionTokens), AmountMicroyuan: amount, HTTPStatus: 200}); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Service) Stream(ctx context.Context, req Request) (*http.Response, *Lease, store.Model, error) {
	req.Stream = true
	lease, model, err := s.Begin(ctx, req)
	if err != nil {
		return nil, nil, store.Model{}, err
	}
	client, err := s.clients.ForProvider(ctx, model.ProviderID)
	if err != nil {
		_ = lease.Close(ctx, Outcome{Status: "failed"})
		return nil, nil, store.Model{}, err
	}
	payload := copyPayload(req.Payload)
	payload["model"] = model.UpstreamName
	payload["stream"] = true
	response, err := client.Stream(ctx, payload)
	if err != nil {
		_ = lease.Close(ctx, Outcome{Status: "failed"})
		return nil, nil, store.Model{}, err
	}
	return response, lease, model, nil
}

func copyPayload(input map[string]any) map[string]any {
	output := make(map[string]any, len(input)+2)
	for key, value := range input {
		output[key] = value
	}
	return output
}

func requestID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "relay-" + time.Now().UTC().Format("20060102T150405.000000000Z07:00")
	}
	return "relay-" + hex.EncodeToString(raw)
}

func priceUsage(usage upstream.Usage, model store.Model) int64 {
	input, output := valueOrZero(model.InputPriceMicroyuan), valueOrZero(model.OutputPriceMicroyuan)
	return pricePart(int64(usage.PromptTokens), input) + pricePart(int64(usage.CompletionTokens), output)
}
func pricePart(tokens, price int64) int64 {
	if tokens <= 0 || price <= 0 {
		return 0
	}
	return (tokens*price + 999999) / 1000000
}

func (s *Service) ActiveGlobal() int {
	if s == nil {
		return 0
	}
	return s.global.Active()
}
func (s *Service) ActiveForKey(token string) int {
	if s == nil {
		return 0
	}
	key, err := s.store.AuthenticateClientKey(context.Background(), token)
	if err != nil {
		return 0
	}
	return s.keys.Active(key.ID)
}
