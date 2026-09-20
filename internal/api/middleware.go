package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"blockchain-exchange/internal/auth"
)

const ctxUserID = "userID"

// AuthRequired JWT 鉴权中间件：Authorization: Bearer <token>
func AuthRequired(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing token"})
			return
		}
		token := strings.TrimPrefix(h, "Bearer ")
		user, err := authSvc.UserFromToken(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "invalid token"})
			return
		}
		c.Set(ctxUserID, user.ID)
		c.Next()
	}
}
