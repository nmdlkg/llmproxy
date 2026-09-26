package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Server) registerUserRoutes() {
	if s == nil || s.engine == nil || s.cfg == nil || !s.cfg.Tenancy.Enabled || s.fork == nil || s.fork.user == nil {
		return
	}
	panelAsset := s.fork.userPanelAsset
	s.engine.GET("/user", func(c *gin.Context) {
		if panelAsset == nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		panelAsset.ServeHTTP(c.Writer, c.Request)
	})
	s.fork.user.RegisterRoutes(s.engine, AuthMiddleware(s.accessManager))
}
