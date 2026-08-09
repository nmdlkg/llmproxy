package user

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	log "github.com/sirupsen/logrus"
)

func (h *Handler) ListUsers(c *gin.Context) {
	users, errList := h.store().ListUsers()
	if errList != nil {
		log.WithError(errList).Error("user admin: list users failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list users"})
		return
	}
	items := make([]gin.H, 0, len(users))
	for index := range users {
		items = append(items, userResponse(&users[index]))
	}
	c.JSON(http.StatusOK, gin.H{"users": items})
}

func (h *Handler) CreateUser(c *gin.Context) {
	var request struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
		Tier        string `json:"tier"`
		Disabled    bool   `json:"disabled"`
		IssueKey    *bool  `json:"issue_key"`
		KeyLabel    string `json:"key_label"`
	}
	if errBind := c.ShouldBindJSON(&request); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	user := &tenancy.User{
		Email:       request.Email,
		DisplayName: request.DisplayName,
		Role:        request.Role,
		Tier:        request.Tier,
		Disabled:    request.Disabled,
	}
	if errCreate := h.store().CreateUser(user); errCreate != nil {
		status := http.StatusBadRequest
		if !errors.Is(errCreate, tenancy.ErrInvalidRole) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": "failed to create user"})
		return
	}
	response := gin.H{"user": userResponse(user)}
	issueKey := request.IssueKey == nil || *request.IssueKey
	if issueKey {
		label := strings.TrimSpace(request.KeyLabel)
		if label == "" {
			label = "initial"
		}
		plaintext, key, errIssue := h.store().IssueAPIKey(user.ID, label)
		if errIssue != nil {
			_ = h.store().DeleteUser(user.ID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue initial API key"})
			return
		}
		response["api_key"] = plaintext
		response["key"] = apiKeyResponse(*key)
	}
	c.JSON(http.StatusCreated, response)
}

func (h *Handler) UpdateUser(c *gin.Context) {
	target, errGet := h.store().GetUser(strings.TrimSpace(c.Param("id")))
	if errGet != nil {
		if errors.Is(errGet, tenancy.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user"})
		return
	}
	var request struct {
		Email       *string `json:"email"`
		DisplayName *string `json:"display_name"`
		Role        *string `json:"role"`
		Tier        *string `json:"tier"`
		Disabled    *bool   `json:"disabled"`
	}
	if errBind := c.ShouldBindJSON(&request); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if request.Email != nil {
		target.Email = *request.Email
	}
	if request.DisplayName != nil {
		target.DisplayName = *request.DisplayName
	}
	if request.Role != nil {
		target.Role = *request.Role
	}
	if request.Tier != nil {
		target.Tier = *request.Tier
	}
	if request.Disabled != nil {
		target.Disabled = *request.Disabled
	}
	if errUpdate := h.store().UpdateUser(target); errUpdate != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to update user"})
		return
	}
	h.invalidateQuota(target.ID)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "user": userResponse(target)})
}

func (h *Handler) DeleteUser(c *gin.Context) {
	current, _ := currentUser(c)
	targetID := strings.TrimSpace(c.Param("id"))
	if targetID == current.ID {
		c.JSON(http.StatusConflict, gin.H{"error": "cannot delete the current admin"})
		return
	}
	if errDelete := h.store().DeleteUser(targetID); errDelete != nil {
		if errors.Is(errDelete, tenancy.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete user"})
		return
	}
	h.invalidateQuota(targetID)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func userResponse(user *tenancy.User) gin.H {
	if user == nil {
		return nil
	}
	return gin.H{
		"id":           user.ID,
		"email":        user.Email,
		"display_name": user.DisplayName,
		"role":         user.Role,
		"tier":         user.Tier,
		"disabled":     user.Disabled,
		"created_at":   user.CreatedAt,
		"updated_at":   user.UpdatedAt,
	}
}
