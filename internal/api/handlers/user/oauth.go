package user

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	management "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
)

func (h *Handler) RequestAnthropicToken(c *gin.Context) {
	if h.oauthUnavailable(c) {
		return
	}
	h.oauth.RequestAnthropicToken(c)
}

func (h *Handler) RequestCodexToken(c *gin.Context) {
	if h.oauthUnavailable(c) {
		return
	}
	h.oauth.RequestCodexToken(c)
}

func (h *Handler) RequestAntigravityToken(c *gin.Context) {
	if h.oauthUnavailable(c) {
		return
	}
	h.oauth.RequestAntigravityToken(c)
}

func (h *Handler) RequestKimiToken(c *gin.Context) {
	if h.oauthUnavailable(c) {
		return
	}
	h.oauth.RequestKimiToken(c)
}

func (h *Handler) RequestXAIToken(c *gin.Context) {
	if h.oauthUnavailable(c) {
		return
	}
	h.oauth.RequestXAIToken(c)
}

func (h *Handler) PostOAuthCallback(c *gin.Context) {
	if h.oauthUnavailable(c) {
		return
	}
	body, errRead := io.ReadAll(io.LimitReader(c.Request.Body, 64<<10))
	if errRead != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid body"})
		return
	}
	var request struct {
		State       string `json:"state"`
		RedirectURL string `json:"redirect_url"`
	}
	if errUnmarshal := json.Unmarshal(body, &request); errUnmarshal != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid body"})
		return
	}
	state := strings.TrimSpace(request.State)
	if state == "" && strings.TrimSpace(request.RedirectURL) != "" {
		if redirectURL, errParse := url.Parse(request.RedirectURL); errParse == nil {
			state = strings.TrimSpace(redirectURL.Query().Get("state"))
		}
	}
	user, _ := currentUser(c)
	if state == "" || !management.OAuthSessionBelongsTo(state, user.ID) {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "unknown or expired state"})
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	h.oauth.PostOAuthCallback(c)
}

func (h *Handler) GetAuthStatus(c *gin.Context) {
	if h.oauthUnavailable(c) {
		return
	}
	state := strings.TrimSpace(c.Query("state"))
	user, _ := currentUser(c)
	if state == "" || !management.OAuthSessionBelongsTo(state, user.ID) {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "unknown or expired state"})
		return
	}
	h.oauth.GetAuthStatus(c)
}

func (h *Handler) oauthUnavailable(c *gin.Context) bool {
	if h == nil || h.oauth == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "OAuth handler unavailable"})
		return true
	}
	return false
}
