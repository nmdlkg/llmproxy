package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const remoteCredentialUploadTimeout = 30 * time.Second

// RemoteStore uploads a locally acquired OAuth credential to a user-scoped server API.
type RemoteStore struct {
	baseURL *url.URL
	userKey string
	client  *http.Client
}

var _ coreauth.Store = (*RemoteStore)(nil)

// NewRemoteStore validates the remote endpoint and creates a credential store.
func NewRemoteStore(remote, userKey string) (*RemoteStore, error) {
	remote = strings.TrimSpace(remote)
	userKey = strings.TrimSpace(userKey)
	if remote == "" {
		return nil, fmt.Errorf("remote credential store: remote URL is required")
	}
	if userKey == "" {
		return nil, fmt.Errorf("remote credential store: user key is required")
	}
	parsed, errParse := url.Parse(remote)
	if errParse != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("remote credential store: remote URL must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("remote credential store: remote URL must not contain user information")
	}
	if parsed.Scheme == "http" && !isLoopbackRemoteHost(parsed.Hostname()) {
		return nil, fmt.Errorf("remote credential store: HTTPS is required for non-loopback hosts")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return &RemoteStore{
		baseURL: parsed,
		userKey: userKey,
		client:  &http.Client{Timeout: remoteCredentialUploadTimeout},
	}, nil
}

func isLoopbackRemoteHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// List is not used by one-shot login and intentionally returns no remote credentials.
func (s *RemoteStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

// Save serializes the authenticator result and uploads it without logging key or body.
func (s *RemoteStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if s == nil || s.baseURL == nil || s.client == nil {
		return "", fmt.Errorf("remote credential store: not initialized")
	}
	if auth == nil {
		return "", fmt.Errorf("remote credential store: auth is nil")
	}
	metadata := make(map[string]any, len(auth.Metadata)+2)
	for key, value := range auth.Metadata {
		if key == "owner_user_id" {
			continue
		}
		metadata[key] = value
	}
	var payload map[string]any
	var errMerge error
	if auth.Storage != nil {
		payload, errMerge = misc.MergeMetadata(auth.Storage, metadata)
	} else {
		payload, errMerge = misc.MergeMetadata(metadata, nil)
	}
	if errMerge != nil {
		return "", fmt.Errorf("remote credential store: serialize credential: %w", errMerge)
	}
	payload["type"] = strings.TrimSpace(auth.Provider)
	payload["disabled"] = auth.Disabled
	delete(payload, "owner_user_id")

	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return "", fmt.Errorf("remote credential store: encode credential: %w", errMarshal)
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = strings.TrimSpace(auth.ID)
	}
	name = filepath.Base(name)
	if name == "." || name == "" || !strings.HasSuffix(strings.ToLower(name), ".json") {
		return "", fmt.Errorf("remote credential store: auth file name must end with .json")
	}

	endpoint := *s.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v0/user/credentials"
	query := endpoint.Query()
	query.Set("name", name)
	endpoint.RawQuery = query.Encode()
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if errRequest != nil {
		return "", fmt.Errorf("remote credential store: create upload request: %w", errRequest)
	}
	request.Header.Set("Authorization", "Bearer "+s.userKey)
	request.Header.Set("Content-Type", "application/json")

	response, errDo := s.client.Do(request)
	if errDo != nil {
		return "", fmt.Errorf("remote credential store: upload credential: %w", errDo)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("remote credential store: close upload response")
		}
	}()
	responseBody, errRead := io.ReadAll(io.LimitReader(response.Body, 32<<10))
	if errRead != nil {
		return "", fmt.Errorf("remote credential store: read upload response: %w", errRead)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message := remoteErrorMessage(responseBody)
		if message == "" {
			message = response.Status
		}
		return "", fmt.Errorf("remote credential store: upload rejected: %s", message)
	}
	return s.baseURL.String() + "/v0/user/credentials", nil
}

// Delete is not supported by the one-shot remote login store.
func (s *RemoteStore) Delete(context.Context, string) error {
	return fmt.Errorf("remote credential store: delete is not supported")
}

func remoteErrorMessage(body []byte) string {
	var response struct {
		Error string `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(body, &response); errUnmarshal != nil {
		return ""
	}
	return strings.TrimSpace(response.Error)
}
