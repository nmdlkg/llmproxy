package autoroute

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	log "github.com/sirupsen/logrus"
)

const classifyIntentPath = "/api/v1/classify/intent"

// Classification is a semantic-router intent classification.
type Classification struct {
	Category   string
	Confidence float64
	Valid      bool
}

// Client calls the vLLM semantic-router apiserver.
type Client struct {
	httpClient *http.Client
	routerURL  string
	timeout    time.Duration
}

// NewClient constructs a proxy-aware semantic-router client.
func NewClient(ctx context.Context, cfg config.AutoRoutingConfig, proxyCfg *config.SDKConfig) *Client {
	var appCfg *config.Config
	if proxyCfg != nil {
		appCfg = &config.Config{SDKConfig: *proxyCfg}
	}
	return &Client{
		httpClient: helps.NewProxyAwareHTTPClient(ctx, appCfg, nil, 0),
		routerURL:  strings.TrimRight(strings.TrimSpace(cfg.RouterURL), "/"),
		timeout:    time.Duration(cfg.TimeoutMS) * time.Millisecond,
	}
}

// Classify returns an invalid classification for every sidecar failure. Classification
// must never make the user's inference request fail.
func (c *Client) Classify(ctx context.Context, text string) Classification {
	if c == nil || c.httpClient == nil || c.routerURL == "" || strings.TrimSpace(text) == "" || c.timeout <= 0 {
		return Classification{}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	body, errMarshal := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	if errMarshal != nil {
		log.WithError(errMarshal).Debug("auto-routing classifier request encoding failed")
		return Classification{}
	}

	// docs/multi-tenant-and-auto-routing.md §7 explicitly permits this bounded
	// timeout: classification is a local pre-inference call and must never stall
	// the user's request.
	classifyCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, errRequest := http.NewRequestWithContext(classifyCtx, http.MethodPost, c.routerURL+classifyIntentPath, bytes.NewReader(body))
	if errRequest != nil {
		log.WithError(errRequest).Debug("auto-routing classifier request creation failed")
		return Classification{}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		log.WithError(errDo).Debug("auto-routing classifier request failed")
		return Classification{}
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("auto-routing classifier response close failed")
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		log.WithField("status", resp.StatusCode).Debug("auto-routing classifier returned non-success status")
		return Classification{}
	}

	var decoded struct {
		Classification struct {
			Category   string  `json:"category"`
			Confidence float64 `json:"confidence"`
		} `json:"classification"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if errDecode := decoder.Decode(&decoded); errDecode != nil {
		log.WithError(errDecode).Debug("auto-routing classifier response decoding failed")
		return Classification{}
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); errTrailing != io.EOF {
		log.WithError(errTrailing).Debug("auto-routing classifier response contained trailing data")
		return Classification{}
	}

	category := strings.ToLower(strings.TrimSpace(decoded.Classification.Category))
	confidence := decoded.Classification.Confidence
	if category == "" || confidence < 0 || confidence > 1 || math.IsNaN(confidence) || math.IsInf(confidence, 0) {
		log.Debug("auto-routing classifier returned a malformed classification")
		return Classification{}
	}
	return Classification{Category: category, Confidence: confidence, Valid: true}
}
