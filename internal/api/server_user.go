package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Server) registerUserRoutes() {
	if s == nil || s.engine == nil || s.cfg == nil || !s.cfg.Tenancy.Enabled || s.user == nil {
		return
	}
	s.engine.GET("/user", func(c *gin.Context) {
		if s.userPanelAsset == nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		s.userPanelAsset.ServeHTTP(c.Writer, c.Request)
	})
	s.user.RegisterRoutes(s.engine, AuthMiddleware(s.accessManager))
}
