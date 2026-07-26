package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	log "github.com/sirupsen/logrus"
)

const (
	defaultBaseURL      = "https://openrouter.ai/api/v1"
	catalogFetchTimeout = 30 * time.Second
	maxCatalogBodyBytes = 16 << 20
	defaultMaxResults   = 50
)

// ErrBenchmarkAPIKeyRequired identifies a benchmark request rejected before
// transmission because no OpenRouter API key is configured.
var ErrBenchmarkAPIKeyRequired = errors.New("OpenRouter API key is required for benchmarks")

// HTTPStatusError reports an unsuccessful catalog response without retaining or
// exposing its response body.
type HTTPStatusError struct {
	Endpoint   string
	StatusCode int
	Err        error
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return fmt.Sprintf("OpenRouter %s request failed with status %d: %v", e.Endpoint, e.StatusCode, e.Err)
	}
	return fmt.Sprintf("OpenRouter %s request failed with status %d", e.Endpoint, e.StatusCode)
}

func (e *HTTPStatusError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// BenchmarkQuery contains supported /benchmarks query parameters.
type BenchmarkQuery struct {
	Source     BenchmarkSource
	TaskType   TaskType
	Category   string
	MaxResults int
	Arena      string
}

// Client fetches OpenRouter's public model catalog and authenticated benchmarks.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a proxy-aware OpenRouter client. The 30-second timeout is
// intentionally limited to background catalog fetches, following the established
// internal/registry/model_updater.go precedent; it is not used for inference.
func NewClient(ctx context.Context, cfg *config.Config) *Client {
	baseURL := defaultBaseURL
	apiKey := ""
	if cfg != nil {
		if configured := strings.TrimRight(strings.TrimSpace(cfg.OpenRouter.BaseURL), "/"); configured != "" {
			baseURL = configured
		}
		apiKey = strings.TrimSpace(cfg.OpenRouter.APIKey)
	}
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: helps.NewProxyAwareHTTPClient(ctx, cfg, nil, catalogFetchTimeout),
	}
}

// FetchPricing fetches and parses the public /models pricing catalog.
func (c *Client) FetchPricing(ctx context.Context) (map[string]ModelPricing, error) {
	data, errFetch := c.fetch(ctx, "models", nil, false)
	if errFetch != nil {
		return nil, errFetch
	}
	return parsePricingResponse(data)
}

// FetchBenchmarks fetches and normalizes one /benchmarks source.
func (c *Client) FetchBenchmarks(ctx context.Context, query BenchmarkQuery) (map[string]ModelQuality, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, &HTTPStatusError{
			Endpoint:   "benchmarks",
			StatusCode: http.StatusUnauthorized,
			Err:        ErrBenchmarkAPIKeyRequired,
		}
	}

	params, errParams := benchmarkQueryValues(query)
	if errParams != nil {
		return nil, errParams
	}
	data, errFetch := c.fetch(ctx, "benchmarks", params, true)
	if errFetch != nil {
		return nil, errFetch
	}
	return parseBenchmarkResponse(data)
}

func benchmarkQueryValues(query BenchmarkQuery) (url.Values, error) {
	switch query.Source {
	case BenchmarkArtificialAnalysis, BenchmarkDesignArena:
	default:
		return nil, fmt.Errorf("unsupported benchmark source %q", query.Source)
	}

	if query.TaskType != "" {
		switch query.TaskType {
		case TaskCoding, TaskIntelligence, TaskAgentic:
		default:
			return nil, fmt.Errorf("unsupported benchmark task type %q", query.TaskType)
		}
	}

	maxResults := query.MaxResults
	if maxResults == 0 {
		maxResults = defaultMaxResults
	}
	if maxResults < 1 || maxResults > 100 {
		return nil, fmt.Errorf("benchmark max_results must be between 1 and 100")
	}

	arena := strings.TrimSpace(query.Arena)
	if arena != "" {
		if query.Source != BenchmarkDesignArena {
			return nil, fmt.Errorf("benchmark arena is only valid for design-arena")
		}
		switch arena {
		case "models", "builders", "agents":
		default:
			return nil, fmt.Errorf("unsupported design-arena value %q", arena)
		}
	}

	values := make(url.Values)
	values.Set("source", string(query.Source))
	values.Set("max_results", strconv.Itoa(maxResults))
	if query.TaskType != "" {
		values.Set("task_type", string(query.TaskType))
	}
	if category := strings.TrimSpace(query.Category); category != "" {
		values.Set("category", category)
	}
	if arena != "" {
		values.Set("arena", arena)
	}
	return values, nil
}

func (c *Client) fetch(ctx context.Context, endpoint string, query url.Values, authenticated bool) ([]byte, error) {
	endpointURL := strings.TrimRight(c.baseURL, "/") + "/" + endpoint
	parsedURL, errURL := url.Parse(endpointURL)
	if errURL != nil {
		return nil, fmt.Errorf("parse OpenRouter %s URL: %w", endpoint, errURL)
	}
	if len(query) > 0 {
		parsedURL.RawQuery = query.Encode()
	}

	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create OpenRouter %s request: %w", endpoint, errRequest)
	}
	req.Header.Set("Accept", "application/json")
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch OpenRouter %s catalog: %w", endpoint, errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("openrouter: close catalog response body")
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &HTTPStatusError{Endpoint: endpoint, StatusCode: resp.StatusCode}
	}

	data, errRead := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBodyBytes+1))
	if errRead != nil {
		return nil, fmt.Errorf("read OpenRouter %s response: %w", endpoint, errRead)
	}
	if len(data) > maxCatalogBodyBytes {
		return nil, fmt.Errorf("OpenRouter %s response exceeds %d bytes", endpoint, maxCatalogBodyBytes)
	}
	return data, nil
}

func decodeDataRows(data []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty JSON response")
	}

	if trimmed[0] == '[' {
		var rows []json.RawMessage
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return nil, fmt.Errorf("decode top-level array: %w", err)
		}
		return rows, nil
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, fmt.Errorf("decode response envelope: %w", err)
	}
	rawRows, ok := envelope["data"]
	if !ok {
		return nil, fmt.Errorf("response envelope is missing data")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(rawRows, &rows); err != nil {
		return nil, fmt.Errorf("response data is not an array: %w", err)
	}
	return rows, nil
}
