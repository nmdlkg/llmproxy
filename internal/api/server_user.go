package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/userpanel"
)

func (s *Server) registerUserRoutes() {
	if s == nil || s.engine == nil || s.cfg == nil || !s.cfg.Tenancy.Enabled || s.user == nil {
		return
	}
	s.engine.GET("/user", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", userpanel.HTML())
	})
	s.user.RegisterRoutes(s.engine, AuthMiddleware(s.accessManager))
}
