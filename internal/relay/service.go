package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"civic-ai-relay/internal/config"
	"civic-ai-relay/internal/logging"
	"civic-ai-relay/internal/requestmeta"
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
	Token string
	// AppID 是公开的应用标识（请求头 X-App-Id），用于「应用型凭据」——
	// 客户端里不存任何密钥的接入方式。与 Token 二选一，同给时以 Token 为准。
	AppID               string
	Model               string
	Messages            []any
	Payload             map[string]any
	InputText           string
	MaxTokens           int64
	MaxCompletionTokens int64
	Stream              bool
	Protocol            string
}

type Outcome struct {
	Status            string
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
	AmountMicroyuan   int64
	HTTPStatus        int
	ErrorCode         string
	FailureStage      string
	UsageSource       string
	Protocol          string
	IncompleteReason  string
	ClientRequestID   string
	DurationMS        *int64
	HeaderMS          *int64
	FirstEventMS      *int64
	LastUpstreamMS    *int64
}

type Service struct {
	store    *store.Store
	clients  ClientFactory
	settings func() config.Settings
	global   *Gate
	keys     *KeyGates
	active   ActiveReservations
	pending  Reconciler
	mu       sync.Mutex
	upstream map[int64]activeUpstream
	draining bool
}

// Ready reports whether the relay can serve traffic: the encrypted SQLite
// state must still answer queries. It backs the /readyz probe used by
// container orchestrators and reverse proxies.
func (s *Service) Ready(ctx context.Context) error {
	if s == nil || s.store == nil {
		return errors.New("relay service unavailable")
	}
	return s.store.Ping(ctx)
}

// authenticate 把「客户端如何证明自己」统一成一条记录：密钥（Bearer）或应用标识
// （X-App-Id）。
//
// 密钥优先，但**失败时回落到应用标识**：不少 OpenAI 兼容客户端会强制发送
// Authorization 头（哪怕里面填的是占位符），此时不该把同时给出的合法应用标识一起
// 废掉。这不降低安全性 —— 应用标识本来就是可以单独使用的凭据，能拿它调用的人
// 本来就能调用。
//
// 两种凭据都可能因「到期」失败，这对客户端是不同于「凭据不对」的信号，所以
// 单独返回 ErrTokenExpired。
func (s *Service) authenticate(ctx context.Context, token, appID string) (store.ClientKey, error) {
	token, appID = strings.TrimSpace(token), strings.TrimSpace(appID)
	if token != "" {
		key, err := s.store.AuthenticateClientKey(ctx, token)
		switch {
		case err == nil:
			if !key.Enabled {
				return key, ErrUnauthorized
			}
			return key, nil
		case errors.Is(err, store.ErrClientKeyExpired):
			return key, ErrTokenExpired
		case appID == "":
			return store.ClientKey{}, ErrUnauthorized
		}
		// 密钥无效但还有应用标识：继续往下试，不把两者一起判死
	}
	key, err := s.store.AuthenticateClientApp(ctx, appID)
	if errors.Is(err, store.ErrClientKeyExpired) {
		return key, ErrTokenExpired
	}
	if err != nil {
		return store.ClientKey{}, ErrUnauthorized
	}
	if !key.Enabled {
		return key, ErrUnauthorized
	}
	return key, nil
}

func (s *Service) Models(ctx context.Context, token, appID string) ([]store.Model, error) {
	key, err := s.authenticate(ctx, token, appID)
	if err != nil {
		return nil, err
	}
	// 认证已确认启用，这里按 Key ID 取授权模型（与公共 /v1/models、管理台接入信息同一份口径）
	return s.store.AuthorizedModelsForKey(ctx, key.ID)
}

// PreviewProviderModels fetches the upstream model catalog for a provider via
// the configured client factory, without importing anything.
func (s *Service) PreviewProviderModels(ctx context.Context, providerID int64) ([]string, error) {
	if s == nil || s.clients == nil {
		return nil, errors.New("relay service unavailable")
	}
	previewer, ok := s.clients.(interface {
		PreviewModels(context.Context, int64) ([]string, error)
	})
	if !ok {
		return nil, errors.New("upstream client does not support model preview")
	}
	return previewer.PreviewModels(ctx, providerID)
}

func NewService(repo *store.Store, clients ClientFactory, settings func() config.Settings) *Service {
	limit := 1
	if settings != nil && settings().GlobalConcurrencyLimit > 0 {
		limit = settings().GlobalConcurrencyLimit
	}
	return &Service{store: repo, clients: clients, settings: settings, global: NewGate(limit), keys: NewKeyGates(), upstream: make(map[int64]activeUpstream)}
}

type Lease struct {
	service         *Service
	reservation     store.RequestReservation
	keyID           int64
	token           string
	clientRequestID string
	protocol        string
	streamed        bool
	model           store.Model
	quote           PriceSnapshot
	mu              sync.Mutex
	outcomeFrozen   bool
	settled         bool
	frozenOutcome   Outcome
	releaseOnce     sync.Once
}

func (l *Lease) Close(ctx context.Context, outcome Outcome) error {
	if l == nil || l.service == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.settled {
		return nil
	}
	if !l.outcomeFrozen {
		if outcome.Status == "" {
			outcome.Status = "completed"
		}
		l.frozenOutcome = outcome
		l.outcomeFrozen = true
	}
	outcome = l.frozenOutcome
	if outcome.ClientRequestID == "" {
		outcome.ClientRequestID = l.clientRequestID
	}
	if trace := requestmeta.FromContext(ctx); trace != nil {
		if outcome.ClientRequestID == "" {
			outcome.ClientRequestID = trace.ClientID
		}
		snapshot := trace.Snapshot(time.Now())
		if outcome.DurationMS == nil {
			outcome.DurationMS = snapshot.DurationMS
		}
		if outcome.HeaderMS == nil {
			outcome.HeaderMS = snapshot.HeaderMS
		}
		if outcome.FirstEventMS == nil {
			outcome.FirstEventMS = snapshot.FirstEventMS
		}
		if outcome.LastUpstreamMS == nil {
			outcome.LastUpstreamMS = snapshot.LastUpstreamMS
		}
	}
	// Keep the enriched outcome (client correlation and elapsed-time snapshot)
	// in the frozen copy so settlement retries use exactly the same values even
	// when their retry context has been canceled or no longer carries Trace.
	l.frozenOutcome = outcome
	cleanup, cancel := settlementContext(ctx)
	defer cancel()
	settlement := store.Settlement{
		Status: outcome.Status, InputTokens: outcome.InputTokens, OutputTokens: outcome.OutputTokens,
		CachedInputTokens: outcome.CachedInputTokens, AmountMicroyuan: outcome.AmountMicroyuan,
		UpstreamStatus: outcome.HTTPStatus, ErrorCode: outcome.ErrorCode,
		FailureStage: outcome.FailureStage, UsageSource: outcome.UsageSource, Protocol: outcome.Protocol,
		IncompleteReason: outcome.IncompleteReason,
		ClientRequestID:  outcome.ClientRequestID,
		DurationMS:       outcome.DurationMS, HeaderMS: outcome.HeaderMS,
		FirstEventMS: outcome.FirstEventMS, LastUpstreamMS: outcome.LastUpstreamMS,
	}
	err := settleWithRetry(cleanup, func(attemptCtx context.Context) error {
		return l.service.store.FinishReservation(attemptCtx, l.reservation.ID, settlement)
	})
	if err != nil {
		l.service.pending.Add(l)
		l.service.active.Remove(l.reservation.ID)
		l.service.forgetUpstream(l.reservation.ID)
		logging.Errorf("request_settlement_failed request_id=%s status=%s error_code=%s failure_stage=%s upstream_status=%d", l.reservation.RequestID, outcome.Status, outcome.ErrorCode, outcome.FailureStage, outcome.HTTPStatus)
		return err
	}
	logging.Infof("request_complete request_id=%s protocol=%s status=%s streamed=%t model_id=%d provider_id=%d upstream_status=%d usage_source=%s input_tokens=%d output_tokens=%d cached_input_tokens=%d duration_ms=%s upstream_header_ms=%s post_header_ms=%s first_content_ms=%s last_upstream_ms=%s", l.reservation.RequestID, outcome.Protocol, outcome.Status, l.streamed, l.model.ID, l.model.ProviderID, outcome.HTTPStatus, outcome.UsageSource, outcome.InputTokens, outcome.OutputTokens, outcome.CachedInputTokens, formatMillis(outcome.DurationMS), formatMillis(outcome.HeaderMS), postHeaderMillis(outcome.DurationMS, outcome.HeaderMS), formatMillis(outcome.FirstEventMS), formatMillis(outcome.LastUpstreamMS))
	l.settled = true
	l.service.pending.Remove(l.reservation.ID)
	l.service.active.Remove(l.reservation.ID)
	l.service.forgetUpstream(l.reservation.ID)
	l.releaseOnce.Do(func() {
		l.service.keys.Release(l.keyID)
		l.service.global.Release()
	})
	return nil
}

func (l *Lease) RequestID() string {
	if l == nil {
		return ""
	}
	return l.reservation.RequestID
}

func (l *Lease) Protocol() string {
	if l == nil || l.protocol == "" {
		return "chat"
	}
	return l.protocol
}

// LeaseInputTokens returns the input token estimate captured when the
// reservation was created. It is used only when a streaming upstream omits
// prompt usage from its terminal event.
func (l *Lease) LeaseInputTokens() int64 {
	if l == nil {
		return 0
	}
	return l.reservation.InputTokens
}

func (s *Service) Begin(ctx context.Context, req Request) (*Lease, store.Model, error) {
	if s != nil && s.isDraining() {
		return nil, store.Model{}, ErrSettlementUnavailable
	}
	// 两种凭据二选一：密钥（Bearer crk_…）或公开的应用标识（X-App-Id）
	if s == nil || s.store == nil || s.clients == nil || strings.TrimSpace(req.Model) == "" ||
		(strings.TrimSpace(req.Token) == "" && strings.TrimSpace(req.AppID) == "") {
		return nil, store.Model{}, ErrInvalidRequest
	}
	key, err := s.authenticate(ctx, req.Token, req.AppID)
	if err != nil {
		return nil, store.Model{}, err
	}
	models, err := s.store.AuthorizedModelsForKey(ctx, key.ID)
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
	// 兜底配置只在未注入 settings 时生效（测试用）。额度项留 0 = 不限，
	// 与出厂默认一致：限额是运维显式选择，不该由兜底值造成意外拒绝。
	settings := config.Settings{GlobalConcurrencyLimit: 1, RPMLimit: 30, MaxOutputTokens: 4096}
	if s.settings != nil {
		settings = s.settings()
	}
	if settings.GlobalConcurrencyLimit > 0 {
		s.global.SetLimit(settings.GlobalConcurrencyLimit)
	}
	if req.MaxTokens < 0 || req.MaxCompletionTokens < 0 {
		return nil, store.Model{}, fmt.Errorf("max_tokens must not be negative: %w", ErrInvalidRequest)
	}
	output := req.MaxTokens
	if req.MaxCompletionTokens > 0 {
		if output > 0 && output != req.MaxCompletionTokens {
			return nil, store.Model{}, fmt.Errorf("max_tokens (%d) conflicts with max_completion_tokens (%d): %w", output, req.MaxCompletionTokens, ErrInvalidRequest)
		}
		output = req.MaxCompletionTokens
	}
	if output <= 0 {
		output = int64(settings.MaxOutputTokens)
	}
	if output <= 0 {
		return nil, store.Model{}, fmt.Errorf("no output token budget: client sent none and server MAX_OUTPUT_TOKENS is unset: %w", ErrInvalidRequest)
	}
	// 客户端（尤其是第三方 UI）常硬编码较大的默认输出上限，超过服务器
	// 限制时收敛到上限继续服务，而不是整体拒绝请求。
	if settings.MaxOutputTokens > 0 && output > int64(settings.MaxOutputTokens) {
		output = int64(settings.MaxOutputTokens)
	}
	if !s.global.TryAcquire() {
		if len(s.pending.IDs()) > 0 || s.isDraining() {
			return nil, store.Model{}, ErrSettlementUnavailable
		}
		return nil, store.Model{}, ErrGlobalConcurrencyExceeded
	}
	if s.isDraining() {
		s.global.Release()
		return nil, store.Model{}, ErrSettlementUnavailable
	}
	if !s.keys.TryAcquire(key.ID, key.ConcurrencyLimit) {
		s.global.Release()
		return nil, store.Model{}, ErrKeyConcurrencyExceeded
	}
	stringFields := make([]string, 0, len(req.Messages)+2)
	for _, message := range req.Messages {
		if encoded, marshalErr := json.Marshal(message); marshalErr == nil {
			stringFields = append(stringFields, string(encoded))
		}
	}
	// tools / functions / response_format 是 messages 之外独立的长文本，会被真实
	// 分词器计入 prompt。旧实现漏算它们，带工具定义的编码类客户端会被显著低估。
	for _, key := range []string{"tools", "functions", "response_format"} {
		if value, ok := req.Payload[key]; ok && value != nil {
			if encoded, marshalErr := json.Marshal(value); marshalErr == nil {
				stringFields = append(stringFields, string(encoded))
			}
		}
	}
	ledgerRequestID := requestID()
	clientRequestID := ""
	if trace := requestmeta.FromContext(ctx); trace != nil {
		ledgerRequestID = trace.ID
		clientRequestID = trace.ClientID
	}
	// 窗口额度不在这里传：判定在事务内从密钥所属分组读取（v18 起），
	// 配置里只剩全局的 RPM 与本文件其他全局项。
	reservation, err := s.store.ReserveRequest(ctx, store.ReserveInput{
		RequestID: ledgerRequestID, KeyID: key.ID, ModelID: model.ID, ProviderID: model.ProviderID,
		Stream: req.Stream, InputText: req.InputText, StringFields: stringFields,
		OutputTokenCeiling: output, MaxOutputTokens: output,
		InputPriceMicroyuan: valueOrZero(model.InputPriceMicroyuan), OutputPriceMicroyuan: valueOrZero(model.OutputPriceMicroyuan),
		Limits: store.QuotaLimits{RPMLimit: int64(settings.RPMLimit)},
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
	quote := priceSnapshot(reservation)
	effective := quote.effective(model)
	s.active.Register(reservation.ID)
	protocol := strings.TrimSpace(req.Protocol)
	if protocol == "" {
		protocol = "chat"
	}
	return &Lease{service: s, reservation: reservation, keyID: key.ID, token: req.Token, model: effective, quote: quote, clientRequestID: clientRequestID, protocol: protocol, streamed: req.Stream}, effective, nil
}

// applyRate scales a per-million-token price by a group multiplier expressed in
// thousandths (1000 = 1.0x). A nil price stays nil (unpriced).
func applyRate(price *int64, rateMilli int64) *int64 {
	if price == nil || rateMilli == 1000 {
		return price
	}
	if rateMilli <= 0 {
		rateMilli = 1000
	}
	if *price > math.MaxInt64/rateMilli {
		n := int64(math.MaxInt64)
		return &n
	}
	n := *price * rateMilli / 1000
	return &n
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func formatMillis(value *int64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatInt(*value, 10)
}

func postHeaderMillis(duration, header *int64) string {
	if duration == nil || header == nil || *duration < *header {
		return "-"
	}
	return strconv.FormatInt(*duration-*header, 10)
}

func (s *Service) Chat(ctx context.Context, req Request) ([]byte, error) {
	req.Stream = false
	lease, model, err := s.Begin(ctx, req)
	if err != nil {
		return nil, err
	}
	client, err := s.clients.ForProvider(ctx, model.ProviderID)
	if err != nil {
		return nil, lease.closeAfterFailure(ctx, err)
	}
	payload := copyPayload(req.Payload)
	payload["model"] = model.UpstreamName
	payload["stream"] = false
	// 非流式同样要有界：上游客户端已不再设总超时，这里套上同一个上限，
	// 避免「响应头回来了但 body 永远发不完」把连接和并发槽一直占着。
	ctx, cancel := s.requestContext(ctx)
	defer cancel()
	s.setUpstream(lease.reservation.ID, cancel, nil)
	data, err := client.ChatJSON(ctx, payload)
	if trace := requestmeta.FromContext(ctx); trace != nil && len(data) > 0 {
		trace.MarkUpstream()
	}
	if err != nil {
		return nil, lease.closeAfterFailure(ctx, err)
	}
	usage := struct {
		Usage upstream.Usage `json:"usage"`
	}{}
	if json.Unmarshal(data, &usage) != nil {
		return nil, lease.closeAfterFailure(ctx, &upstream.Error{Code: "upstream_response_invalid", Status: http.StatusOK})
	}
	amount := PriceUsage(usage.Usage, model)
	if err := lease.Close(ctx, Outcome{Status: "completed", InputTokens: int64(usage.Usage.PromptTokens), OutputTokens: int64(usage.Usage.CompletionTokens), CachedInputTokens: int64(usage.Usage.PromptTokensDetails.CachedTokens), AmountMicroyuan: amount, HTTPStatus: http.StatusOK, UsageSource: "upstream", Protocol: lease.protocol}); err != nil {
		return nil, errors.Join(ErrSettlementUnavailable, err)
	}
	return data, nil
}

func (s *Service) Stream(ctx context.Context, req Request) (*http.Response, *Lease, store.Model, error) {
	prepared, err := s.PrepareStream(ctx, req)
	if err != nil {
		return nil, nil, store.Model{}, err
	}
	response, err := prepared.Open()
	if err != nil {
		prepared.Cancel()
		return nil, nil, store.Model{}, prepared.Lease.closeAfterFailure(ctx, err)
	}
	return response, prepared.Lease, prepared.Model, nil
}

type PreparedStream struct {
	Lease             *Lease
	Model             store.Model
	Open              func() (*http.Response, error)
	Cancel            context.CancelFunc
	HeartbeatInterval time.Duration
}

// PrepareStream reserves once, then leaves the upstream request to Open so
// the HTTP handler can flush a comment before waiting for response headers.
func (s *Service) PrepareStream(ctx context.Context, req Request) (*PreparedStream, error) {
	req.Stream = true
	lease, model, err := s.Begin(ctx, req)
	if err != nil {
		return nil, err
	}
	payload := copyPayload(req.Payload)
	payload["model"] = model.UpstreamName
	payload["stream"] = true
	var heartbeat, maxDuration time.Duration
	if s.settings != nil {
		settings := s.settings()
		heartbeat = settings.SSEHeartbeatInterval
		maxDuration = settings.MaxStreamDuration
	}
	var upstreamCtx context.Context
	var cancel context.CancelFunc
	if maxDuration > 0 {
		upstreamCtx, cancel = context.WithTimeout(ctx, maxDuration)
	} else {
		upstreamCtx, cancel = context.WithCancel(ctx)
	}
	s.setUpstream(lease.reservation.ID, cancel, nil)
	var bodyMu sync.Mutex
	var body io.ReadCloser
	var openMu sync.Mutex
	opened := false
	prepared := &PreparedStream{Lease: lease, Model: model, HeartbeatInterval: heartbeat}
	prepared.Cancel = func() {
		cancel()
		bodyMu.Lock()
		activeBody := body
		bodyMu.Unlock()
		if activeBody != nil {
			_ = activeBody.Close()
		}
	}
	prepared.Open = func() (*http.Response, error) {
		openMu.Lock()
		if opened {
			openMu.Unlock()
			return nil, errors.New("prepared stream already opened")
		}
		opened = true
		openMu.Unlock()
		if err := upstreamCtx.Err(); err != nil {
			return nil, err
		}
		client, err := s.clients.ForProvider(upstreamCtx, model.ProviderID)
		if err != nil {
			return nil, err
		}
		response, err := client.Stream(upstreamCtx, payload)
		if err != nil {
			return nil, err
		}
		if response == nil || response.Body == nil {
			return nil, &upstream.Error{Code: "upstream_response_invalid"}
		}
		response.Body = &cancelOnClose{ReadCloser: response.Body, cancel: cancel}
		bodyMu.Lock()
		body = response.Body
		bodyMu.Unlock()
		if err := upstreamCtx.Err(); err != nil {
			_ = response.Body.Close()
			return nil, err
		}
		s.setUpstream(lease.reservation.ID, cancel, response.Body)
		return response, nil
	}
	return prepared, nil
}

func upstreamFailure(err error) (int, string) {
	var upstreamErr *upstream.Error
	if errors.As(err, &upstreamErr) {
		return upstreamErr.Status, upstreamErr.Code
	}
	if errors.Is(err, context.Canceled) {
		return 0, "client_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, "request_duration_exceeded"
	}
	return 0, "upstream_connection_failed"
}

func (l *Lease) closeAfterFailure(ctx context.Context, upstreamErr error) error {
	statusCode, code := upstreamFailure(upstreamErr)
	status := "failed"
	if code == "client_canceled" {
		status = "aborted"
	}
	if err := l.Close(ctx, Outcome{Status: status, HTTPStatus: statusCode, ErrorCode: code, FailureStage: "upstream", UsageSource: "unknown", Protocol: l.protocol}); err != nil {
		return errors.Join(ErrSettlementUnavailable, upstreamErr, err)
	}
	return upstreamErr
}

// requestContext 给上游请求套上 MAX_STREAM_DURATION 的上限（流式与非流式共用）。
// 0 或负数表示不限，与其它配置项「0 = 不限」的口径一致。
func (s *Service) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s == nil || s.settings == nil {
		return context.WithCancel(ctx)
	}
	if limit := s.settings().MaxStreamDuration; limit > 0 {
		return context.WithTimeout(ctx, limit)
	}
	return context.WithCancel(ctx)
}

// cancelOnClose 把「释放超时 context」绑到响应体上：调用方本来就会 defer Close()，
// 这样既不会漏掉 cancel（导致 timer 与上游连接泄漏），也不必要求每个调用点都记得它。
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

func (c *cancelOnClose) Close() error {
	c.once.Do(func() {
		c.err = c.ReadCloser.Close()
		c.cancel()
	})
	return c.err
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

// PriceUsage is the single billing implementation shared by non-stream and
// stream settlement. Cached prompt tokens (a subset of PromptTokens) are
// billed at the model's cached input price; when no cached price is
// configured they fall back to the regular input price.
func PriceUsage(usage upstream.Usage, model store.Model) int64 {
	input, output := valueOrZero(model.InputPriceMicroyuan), valueOrZero(model.OutputPriceMicroyuan)
	cachedPrice := input
	if model.CachedInputPriceMicroyuan != nil {
		cachedPrice = *model.CachedInputPriceMicroyuan
	}
	prompt := int64(usage.PromptTokens)
	cached := int64(usage.PromptTokensDetails.CachedTokens)
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt
	}
	parts := []int64{
		pricePart(prompt-cached, input),
		pricePart(cached, cachedPrice),
		pricePart(int64(usage.CompletionTokens), output),
	}
	var total int64
	for _, part := range parts {
		if part > math.MaxInt64-total {
			return math.MaxInt64
		}
		total += part
	}
	return total
}
func pricePart(tokens, price int64) int64 {
	if tokens <= 0 || price <= 0 {
		return 0
	}
	var product, quotient, remainder big.Int
	product.Mul(big.NewInt(tokens), big.NewInt(price))
	quotient.QuoRem(&product, big.NewInt(1_000_000), &remainder)
	if remainder.Sign() > 0 {
		quotient.Add(&quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return math.MaxInt64
	}
	return quotient.Int64()
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
