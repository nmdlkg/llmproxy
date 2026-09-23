package user

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/authfiles"
	xaiauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/xai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func (h *Handler) ListCredentials(c *gin.Context) {
	user, _ := currentUser(c)
	credentials := make([]gin.H, 0)
	if h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if authfiles.OwnerUserID(auth) != user.ID {
				continue
			}
			credentials = append(credentials, credentialResponse(auth))
		}
	}
	c.JSON(http.StatusOK, gin.H{"credentials": credentials})
}

func (h *Handler) UploadCredential(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "credential manager unavailable"})
		return
	}
	user, _ := currentUser(c)
	data, errRead := readCredentialBody(c)
	if errRead != nil {
		status := http.StatusBadRequest
		if errors.Is(errRead, errCredentialTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		c.JSON(status, gin.H{"error": errRead.Error()})
		return
	}

	metadata := make(map[string]any)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&metadata); errDecode != nil || metadata == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid credential JSON"})
		return
	}
	if errTrailing := decoder.Decode(&struct{}{}); !errors.Is(errTrailing, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid credential JSON"})
		return
	}
	metadata, errSanitize := sanitizeUploadedCredential(metadata, user.ID)
	if errSanitize != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errSanitize.Error()})
		return
	}

	name := credentialUploadName(c, metadata)
	if authfiles.IsUnsafeAuthFileName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid credential name"})
		return
	}
	h.credentialMu.Lock()
	defer h.credentialMu.Unlock()
	if !h.credentialNameAvailableToUser(name, user.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": authfiles.ErrCredentialExists.Error()})
		return
	}
	raw, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid credential JSON"})
		return
	}
	if errWrite := authfiles.WriteAuthFile(c.Request.Context(), h.cfg, h.authManager, name, raw); errWrite != nil {
		if errors.Is(errWrite, authfiles.ErrCredentialExists) {
			c.JSON(http.StatusConflict, gin.H{"error": authfiles.ErrCredentialExists.Error()})
			return
		}
		log.WithError(errWrite).Error("user credentials: upload failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store credential"})
		return
	}
	h.invalidateQuota(user.ID)
	auth, _ := authfiles.FindAuth(h.authManager, name)
	c.JSON(http.StatusCreated, gin.H{"status": "ok", "credential": credentialResponse(auth)})
}

func (h *Handler) PatchCredential(c *gin.Context) {
	user, _ := currentUser(c)
	h.credentialMu.Lock()
	defer h.credentialMu.Unlock()
	auth, ok := h.ownedCredential(c.Param("id"), user.ID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}

	var request map[string]json.RawMessage
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 64<<10))
	if errDecode := decoder.Decode(&request); errDecode != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if errTrailing := decoder.Decode(&struct{}{}); !errors.Is(errTrailing, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if len(request) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}
	for field := range request {
		if field != "shared" && field != "disabled" && field != "note" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "only shared, disabled, and note may be updated"})
			return
		}
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	if raw, exists := request["shared"]; exists {
		var shared bool
		if errUnmarshal := json.Unmarshal(raw, &shared); errUnmarshal != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "shared must be a boolean"})
			return
		}
		auth.Metadata["shared"] = shared
		auth.Attributes["shared"] = strconv.FormatBool(shared)
	}
	if raw, exists := request["disabled"]; exists {
		var disabled bool
		if errUnmarshal := json.Unmarshal(raw, &disabled); errUnmarshal != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "disabled must be a boolean"})
			return
		}
		auth.Metadata["disabled"] = disabled
		auth.Disabled = disabled
		if disabled {
			auth.Status = coreauth.StatusDisabled
			auth.StatusMessage = "disabled via user API"
		} else {
			auth.Status = coreauth.StatusActive
			auth.StatusMessage = ""
		}
	}
	if raw, exists := request["note"]; exists {
		var note string
		if errUnmarshal := json.Unmarshal(raw, &note); errUnmarshal != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "note must be a string"})
			return
		}
		note = strings.TrimSpace(note)
		if len(note) > 2048 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "note is too long"})
			return
		}
		auth.Metadata["note"] = note
		if note == "" {
			delete(auth.Attributes, "note")
		} else {
			auth.Attributes["note"] = note
		}
	}
	auth.Metadata["owner_user_id"] = user.ID
	auth.Attributes["owner_user_id"] = user.ID
	auth.UpdatedAt = time.Now()
	if errSave := h.saveCredential(c, auth); errSave != nil {
		log.WithError(errSave).WithField("auth_id", auth.ID).Error("user credentials: patch failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update credential"})
		return
	}
	h.invalidateQuota(user.ID)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "credential": credentialResponse(auth)})
}

func (h *Handler) DeleteCredential(c *gin.Context) {
	user, _ := currentUser(c)
	h.credentialMu.Lock()
	defer h.credentialMu.Unlock()
	auth, ok := h.ownedCredential(c.Param("id"), user.ID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}
	if h.tokenStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "credential store unavailable"})
		return
	}
	deleteID, errPath := h.safeCredentialPath(auth)
	if errPath != nil {
		log.WithError(errPath).WithField("auth_id", auth.ID).Error("user credentials: invalid stored path")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "credential path is invalid"})
		return
	}
	if errDelete := h.tokenStore.Delete(c.Request.Context(), deleteID); errDelete != nil {
		log.WithError(errDelete).WithField("auth_id", auth.ID).Error("user credentials: delete failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete credential"})
		return
	}
	h.authManager.Remove(c.Request.Context(), auth.ID)
	h.invalidateQuota(user.ID)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

var errCredentialTooLarge = errors.New("credential upload exceeds 4 MiB")

func readCredentialBody(c *gin.Context) ([]byte, error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil, fmt.Errorf("credential body is required")
	}
	reader := io.LimitReader(c.Request.Body, authfiles.MaxUploadBytes+1)
	data, errRead := io.ReadAll(reader)
	if errRead != nil {
		return nil, fmt.Errorf("read credential body: %w", errRead)
	}
	if int64(len(data)) > authfiles.MaxUploadBytes {
		return nil, errCredentialTooLarge
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("credential body is required")
	}
	return data, nil
}

func credentialUploadName(c *gin.Context, metadata map[string]any) string {
	name := strings.TrimSpace(c.Query("name"))
	if name == "" {
		name = strings.TrimSpace(c.GetHeader("X-Auth-File-Name"))
	}
	if name != "" {
		return name
	}
	provider, _ := metadata["type"].(string)
	email, _ := metadata["email"].(string)
	base := sanitizeCredentialName(provider)
	if base == "" {
		base = "credential"
	}
	if emailPart := sanitizeCredentialName(email); emailPart != "" {
		base += "-" + emailPart
	} else {
		base += "-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	}
	return base + ".json"
}

func sanitizeCredentialName(value string) string {
	var builder strings.Builder
	for _, character := range strings.TrimSpace(value) {
		switch {
		case character >= 'a' && character <= 'z':
			builder.WriteRune(character)
		case character >= 'A' && character <= 'Z':
			builder.WriteRune(character)
		case character >= '0' && character <= '9':
			builder.WriteRune(character)
		case character == '@' || character == '.' || character == '_' || character == '-':
			builder.WriteRune(character)
		default:
			builder.WriteRune('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}

// Codex id_token is intentionally excluded because its client-controlled plan
// claims influence quota contribution; a later server-side refresh may restore it.
var uploadedCredentialFields = map[string][]string{
	"claude":      {"id_token", "access_token", "refresh_token", "last_refresh", "email", "expired"},
	"codex":       {"access_token", "refresh_token", "account_id", "last_refresh", "email", "expired"},
	"antigravity": {"access_token", "refresh_token", "expires_in", "timestamp", "expired", "email", "project_id"},
	"kimi":        {"access_token", "refresh_token", "token_type", "scope", "timestamp", "expired", "device_id"},
	"xai":         {"access_token", "refresh_token", "id_token", "token_type", "expires_in", "expired", "last_refresh", "email", "sub"},
}

func sanitizeUploadedCredential(input map[string]any, userID string) (map[string]any, error) {
	provider, _ := input["type"].(string)
	provider = strings.ToLower(strings.TrimSpace(provider))
	fields, supported := uploadedCredentialFields[provider]
	if !supported {
		return nil, fmt.Errorf("unsupported credential type")
	}
	accessToken, _ := input["access_token"].(string)
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("credential access_token is required")
	}

	output := make(map[string]any, len(fields)+5)
	output["type"] = provider
	for _, field := range fields {
		if value, exists := input[field]; exists {
			output[field] = value
		}
	}
	if provider == "xai" {
		output["base_url"] = xaiauth.DefaultAPIBaseURL
		output["auth_kind"] = "oauth"
		if rawEndpoint, exists := input["token_endpoint"]; exists {
			tokenEndpoint, ok := rawEndpoint.(string)
			if !ok {
				return nil, fmt.Errorf("invalid xai token_endpoint")
			}
			tokenEndpoint = strings.TrimSpace(tokenEndpoint)
			if tokenEndpoint != "" {
				validated, errValidate := xaiauth.ValidateOAuthEndpoint(tokenEndpoint, "token_endpoint")
				if errValidate != nil {
					return nil, fmt.Errorf("invalid xai token_endpoint")
				}
				output["token_endpoint"] = validated
			}
		}
	}
	output["owner_user_id"] = strings.TrimSpace(userID)
	output["shared"] = false
	output["disabled"] = false
	return output, nil
}

func (h *Handler) credentialNameAvailableToUser(name, userID string) bool {
	if auth, ok := authfiles.FindAuth(h.authManager, name); ok {
		return authfiles.OwnerUserID(auth) == userID
	}
	path := filepath.Join(h.cfg.AuthDir, filepath.Base(name))
	if info, errLstat := os.Lstat(path); errLstat == nil && info.Mode()&os.ModeSymlink != 0 {
		return false
	} else if errLstat != nil && !os.IsNotExist(errLstat) {
		return false
	}
	data, errRead := os.ReadFile(path)
	if os.IsNotExist(errRead) {
		return true
	}
	if errRead != nil {
		return false
	}
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
		return false
	}
	owner, _ := metadata["owner_user_id"].(string)
	return strings.TrimSpace(owner) == userID
}

func (h *Handler) ownedCredential(id, userID string) (*coreauth.Auth, bool) {
	auth, ok := authfiles.FindAuth(h.authManager, strings.TrimSpace(id))
	if !ok || authfiles.OwnerUserID(auth) != userID {
		return nil, false
	}
	return auth, true
}

func (h *Handler) saveCredential(c *gin.Context, auth *coreauth.Auth) error {
	if h.tokenStore == nil {
		return fmt.Errorf("credential store unavailable")
	}
	path, errPath := h.safeCredentialPath(auth)
	if errPath != nil {
		return errPath
	}
	auth.Attributes[coreauth.AttributePath] = path
	auth.Attributes[coreauth.AttributeSource] = path
	if _, errSave := h.tokenStore.Save(c.Request.Context(), auth); errSave != nil {
		return fmt.Errorf("save credential: %w", errSave)
	}
	if _, errUpdate := h.authManager.Update(c.Request.Context(), auth); errUpdate != nil {
		return fmt.Errorf("update credential runtime record: %w", errUpdate)
	}
	return nil
}

func (h *Handler) safeCredentialPath(auth *coreauth.Auth) (string, error) {
	if h == nil || h.cfg == nil || auth == nil {
		return "", fmt.Errorf("credential path is unavailable")
	}
	path := authfiles.AuthPath(auth)
	if path == "" {
		path = strings.TrimSpace(auth.FileName)
	}
	if path == "" {
		path = strings.TrimSpace(auth.ID)
	}
	if !filepath.IsAbs(path) {
		if authfiles.IsUnsafeAuthFileName(path) {
			return "", fmt.Errorf("credential path is invalid")
		}
		path = filepath.Join(h.cfg.AuthDir, path)
	}
	if !authfiles.PathWithinAuthDir(h.cfg, path) {
		return "", fmt.Errorf("credential path is outside auth directory")
	}
	return path, nil
}

func credentialResponse(auth *coreauth.Auth) gin.H {
	if auth == nil {
		return nil
	}
	response := gin.H{
		"id":       auth.ID,
		"name":     auth.FileName,
		"provider": auth.Provider,
		"disabled": auth.Disabled,
		"shared":   metadataBool(auth.Metadata, auth.Attributes, "shared"),
	}
	if email, ok := auth.Metadata["email"].(string); ok && strings.TrimSpace(email) != "" {
		response["email"] = strings.TrimSpace(email)
	}
	if note, ok := auth.Metadata["note"].(string); ok && strings.TrimSpace(note) != "" {
		response["note"] = strings.TrimSpace(note)
	}
	return response
}

func metadataBool(metadata map[string]any, attributes map[string]string, key string) bool {
	if value, ok := metadata[key].(bool); ok {
		return value
	}
	if value, ok := metadata[key].(string); ok {
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(value))
		return errParse == nil && parsed
	}
	parsed, errParse := strconv.ParseBool(strings.TrimSpace(attributes[key]))
	return errParse == nil && parsed
}
