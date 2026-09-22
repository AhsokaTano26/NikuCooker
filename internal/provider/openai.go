package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ChatRequest is one completion request.
type ChatRequest struct {
	// System is the instruction: the prompt template plus the glossary.
	System string

	// User is the payload to operate on — the lines to translate, the transcript
	// to analyse.
	User string

	Temperature float64

	// MaxTokens bounds the response. Zero omits the field and lets the server
	// choose, which is the right default for endpoints that reject a value
	// above their own ceiling.
	MaxTokens int

	// JSON asks the server to constrain the response to a JSON object.
	//
	// It is a request, not a guarantee: the field is optional in the de facto
	// standard, and the parsers downstream are built to survive a server that
	// ignored it. A server that rejects it outright is handled by Client.
	JSON bool
}

// ChatResponse is a completion.
type ChatResponse struct {
	Content string

	PromptTokens     int
	CompletionTokens int
	TotalTokens      int

	// Model is what the server reports having used, which is not always what was
	// requested — a provider may route a name to a different revision.
	Model string

	FinishReason string
}

// Truncated reports whether the response was cut off by a token limit.
//
// It is checked rather than ignored because a truncated JSON response is
// *invalid JSON*, and the natural reaction to invalid JSON is to retry — which
// would truncate again, at the same place, forever.
func (r *ChatResponse) Truncated() bool { return r.FinishReason == "length" }

// ErrTruncated reports a response cut off by the token limit.
var ErrTruncated = errors.New("provider: the response was cut off by the token limit; raise max_tokens or lower the batch size")

// APIError is a non-success response from the endpoint.
type APIError struct {
	StatusCode int

	// Message is the server's own explanation. It is worth surfacing verbatim: a
	// provider saying "invalid api key" is far more useful than a bare 401, and
	// only the provider knows why it refused.
	Message string

	// Body is the raw response, truncated. It is what a user pastes into a bug
	// report when the message is unhelpful.
	Body string

	// RetryAfter is the server's own guidance, when it sent any.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("provider: HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("provider: HTTP %d", e.StatusCode)
}

// Retryable reports whether retrying the same request could plausibly succeed.
//
// 429 and the 5xx family are transient by definition. The remaining 4xx are not:
// a bad key, a malformed request or an unknown model will fail identically on
// every attempt, and retrying them only delays the error the user needs to see.
func (e *APIError) Retryable() bool {
	switch e.StatusCode {
	case http.StatusRequestTimeout,
		http.StatusConflict,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		529: // Cloudflare's "origin is overloaded", which OpenAI surfaces too.
		return true
	}
	return false
}

// maxErrorBody is how much of a failing response is kept for the error.
const maxErrorBody = 4 << 10

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// ClientOptions configures a client.
type ClientOptions struct {
	// Timeout bounds a single request. Zero uses DefaultRequestTimeout.
	Timeout time.Duration

	// HTTPClient overrides the transport, for tests.
	HTTPClient *http.Client
}

// DefaultRequestTimeout bounds one completion request.
//
// Generous, because a long batch on a reasoning model is legitimately slow, and
// the transport timeouts below are what catch an endpoint that is not answering
// at all.
const DefaultRequestTimeout = 5 * time.Minute

// Client calls an OpenAI-compatible chat endpoint.
type Client struct {
	endpoint string
	apiKey   string
	model    string
	timeout  time.Duration
	http     *http.Client

	// compat records what this endpoint turned out to accept. It is shared
	// across calls, so a server that rejects the JSON-mode field is only asked
	// once — which matters because the discovery costs a round trip.
	compat compat
}

// compat remembers negotiated request options.
type compat struct {
	mu sync.Mutex

	// jsonModeSet is false until the endpoint's answer is known.
	jsonModeSet bool
	jsonMode    bool

	// tokenField is the request field carrying the response limit.
	tokenField string
}

// NewClient builds a client for a provider record.
func NewClient(record *Provider, opts ClientOptions) (*Client, error) {
	if record == nil {
		return nil, errors.New("provider: no provider was given")
	}
	if record.Type != TypeOpenAICompatible {
		return nil, fmt.Errorf("provider: %q is not an OpenAI-compatible provider", record.Type)
	}

	endpoint, err := chatEndpoint(record.BaseURL)
	if err != nil {
		return nil, err
	}
	if record.Model == "" {
		return nil, errors.New("provider: no model is set for this provider")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Transport: defaultTransport()}
	}

	return &Client{
		endpoint: endpoint,
		apiKey:   record.APIKey,
		model:    record.Model,
		timeout:  timeout,
		http:     httpClient,
		compat:   compat{tokenField: "max_tokens"},
	}, nil
}

// Model reports the configured model name.
func (c *Client) Model() string { return c.model }

// defaultTransport is a transport with per-phase timeouts.
//
// There is deliberately no overall client timeout. One request may legitimately
// take minutes to generate, while a host that never answers should fail in
// seconds, and a single whole-request deadline cannot express both.
func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// Covers the TLS handshake separately from the dial, so a host that
		// accepts the connection and then stalls is still caught.
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}
}

// chatEndpoint resolves a base URL to the completions path.
//
// Users type every variation of this — with a trailing slash, with /v1, with the
// full path already on the end — and rejecting any of them would be a support
// burden for no benefit, since all of them are unambiguous.
func chatEndpoint(baseURL string) (string, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return "", errors.New("provider: no base URL is set for this provider")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("provider: %q is not a valid URL", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("provider: %q must start with http:// or https://", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("provider: %q has no host", raw)
	}

	trimmed := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(trimmed, "/chat/completions") {
		return parsed.String(), nil
	}

	// A base URL that already names an API version keeps it; one that names only
	// a host gets /v1. Every OpenAI-compatible server in practice serves under
	// /v1, and a user pasting a bare host almost always means that.
	if !strings.Contains(trimmed, "/v") {
		trimmed += "/v1"
	}
	parsed.Path = trimmed + "/chat/completions"
	return parsed.String(), nil
}

// Chat sends a completion request.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// At most two corrections: one for the JSON-mode field and one for the token
	// limit field. Bounding it here is what makes the loop terminate, since each
	// correction is only ever applied once.
	corrections := 0

	for {
		jsonMode := c.jsonModeEnabled(req.JSON)
		tokenField := c.tokenFieldName()

		response, err := c.attempt(ctx, req, jsonMode, tokenField)
		if err == nil {
			return response, nil
		}

		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
			return nil, err
		}

		changed := false
		if corrections < 2 {
			switch {
			case jsonMode && mentions(apiErr, "response_format"):
				c.disableJSONMode()
				changed = true
			case tokenField == "max_tokens" && mentions(apiErr, "max_tokens"):
				c.setTokenField("max_completion_tokens")
				changed = true
			}
		}
		if !changed {
			return nil, err
		}
		corrections++
	}
}

// mentions reports whether the server's complaint names a field.
//
// Both the machine-readable message and the raw body are searched, because
// servers disagree about which one carries the field name.
func mentions(err *APIError, field string) bool {
	return strings.Contains(err.Message, field) || strings.Contains(err.Body, field)
}

func (c *Client) jsonModeEnabled(wanted bool) bool {
	if !wanted {
		return false
	}

	c.compat.mu.Lock()
	defer c.compat.mu.Unlock()

	if c.compat.jsonModeSet {
		return c.compat.jsonMode
	}
	// Optimistic until the endpoint says otherwise: nearly every
	// OpenAI-compatible server in use today supports the field, and asking the
	// ones that do not costs one extra round trip on the first request only.
	return true
}

func (c *Client) disableJSONMode() {
	c.compat.mu.Lock()
	defer c.compat.mu.Unlock()
	c.compat.jsonModeSet = true
	c.compat.jsonMode = false
}

func (c *Client) tokenFieldName() string {
	c.compat.mu.Lock()
	defer c.compat.mu.Unlock()
	if c.compat.tokenField == "" {
		return "max_tokens"
	}
	return c.compat.tokenField
}

func (c *Client) setTokenField(name string) {
	c.compat.mu.Lock()
	defer c.compat.mu.Unlock()
	c.compat.tokenField = name
}

// wireRequest is the request body as the API expects it.
//
// A map rather than a struct, because the token-limit field name varies between
// deployments and a struct tag cannot.
type wireRequest map[string]any

func (c *Client) attempt(
	ctx context.Context,
	req ChatRequest,
	jsonMode bool,
	tokenField string,
) (*ChatResponse, error) {
	messages := make([]map[string]string, 0, 2)
	if req.System != "" {
		messages = append(messages, map[string]string{"role": "system", "content": req.System})
	}
	messages = append(messages, map[string]string{"role": "user", "content": req.User})

	body := wireRequest{
		"model":    c.model,
		"messages": messages,
		// Stated rather than left to the server's default: some deployments
		// stream by default, and a streamed response is not JSON.
		"stream": false,
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if req.MaxTokens > 0 {
		body[tokenField] = req.MaxTokens
	}
	if jsonMode {
		body["response_format"] = map[string]string{"type": "json_object"}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("provider: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("provider: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		// Classified as retryable so the caller's backoff handles a flaky
		// network or a provider that drops connections under load. A cancelled
		// context is not retryable, and is distinguished here so that a user
		// pressing stop does not cause five more requests.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("provider: %w", ctx.Err())
		}
		return nil, &TransportError{Err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, 1<<16))
		_ = httpResp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBytes))
	if err != nil {
		return nil, &TransportError{Err: fmt.Errorf("read response: %w", err)}
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return nil, newAPIError(httpResp, raw)
	}

	return parseChatResponse(raw)
}

// maxResponseBytes bounds a response body.
//
// A batch of a few hundred lines is tens of kilobytes. The limit is here so that
// an endpoint returning an unbounded stream cannot exhaust memory, and it is
// well above any legitimate response.
const maxResponseBytes = 32 << 20

// TransportError is a failure to complete the HTTP exchange.
//
// Distinct from APIError because the server never answered: there is no status
// and no message, only the network error.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string { return fmt.Sprintf("provider: request failed: %v", e.Err) }
func (e *TransportError) Unwrap() error { return e.Err }

// Retryable reports that the request may succeed if repeated.
func (e *TransportError) Retryable() bool { return true }

// newAPIError builds an error from a failing response.
func newAPIError(resp *http.Response, raw []byte) *APIError {
	apiErr := &APIError{StatusCode: resp.StatusCode}

	body := strings.TrimSpace(string(raw))
	if len(body) > maxErrorBody {
		body = body[:maxErrorBody] + "…"
	}
	apiErr.Body = body
	apiErr.Message = extractMessage(body)
	apiErr.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	return apiErr
}

// extractMessage pulls the server's own explanation out of an error body.
func extractMessage(body string) string {
	if body == "" {
		return ""
	}

	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err == nil {
		if envelope.Error.Message != "" {
			return envelope.Error.Message
		}
		// Some gateways report the same shape but flatten it.
		if envelope.Message != "" {
			return envelope.Message
		}
		if envelope.Detail != "" {
			return envelope.Detail
		}
	}

	// Not JSON, or JSON without a message: an HTML error page from a proxy, say.
	// The raw text is still the most useful thing available, so it is truncated
	// to something that fits in a log line.
	if len(body) > 300 {
		return body[:300] + "…"
	}
	return body
}

// parseRetryAfter reads the Retry-After header, in either of its two forms.
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if wait := time.Until(when); wait > 0 {
			return wait
		}
	}
	return 0
}

// wireResponse is the response body as the API returns it.
type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		// Some servers report the same figures under other names.
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// parseChatResponse decodes a successful response.
func parseChatResponse(raw []byte) (*ChatResponse, error) {
	var wire wireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("provider: the response was not JSON: %w", err)
	}

	// A provider that returns 200 with an error object inside is a real pattern;
	// without this the content would be empty and the failure would be reported
	// as "the model returned nothing", which points at the wrong thing.
	if wire.Error != nil && wire.Error.Message != "" {
		return nil, &APIError{StatusCode: http.StatusOK, Message: wire.Error.Message}
	}
	if len(wire.Choices) == 0 {
		return nil, errors.New("provider: the response contained no choices")
	}

	choice := wire.Choices[0]

	// A refusal is a deliberate answer, not a malfunction. Reporting it as an
	// empty response would send the user looking for a parsing bug.
	if choice.Message.Refusal != "" && choice.Message.Content == "" {
		return nil, fmt.Errorf("provider: the model refused the request: %s", choice.Message.Refusal)
	}

	response := &ChatResponse{
		Content:      choice.Message.Content,
		Model:        wire.Model,
		FinishReason: choice.FinishReason,
	}

	response.PromptTokens = wire.Usage.PromptTokens
	if response.PromptTokens == 0 {
		response.PromptTokens = wire.Usage.InputTokens
	}
	response.CompletionTokens = wire.Usage.CompletionTokens
	if response.CompletionTokens == 0 {
		response.CompletionTokens = wire.Usage.OutputTokens
	}
	response.TotalTokens = wire.Usage.TotalTokens
	if response.TotalTokens == 0 {
		response.TotalTokens = response.PromptTokens + response.CompletionTokens
	}

	if response.Truncated() {
		return response, ErrTruncated
	}
	return response, nil
}
