// Package auth 用户认证服务（MVP 内存版）：
// - bcrypt 密码哈希
// - JWT 签发/校验（24h 有效期）
// - 注册/登录/Token 解析
// 生产环境：用户库落 MySQL，JWT 密钥由 KMS 托管，支持 2FA/KYC 等级。
package auth

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrEmailTaken   = errors.New("email already registered")
	ErrBadPassword  = errors.New("password must be at least 8 characters")
	ErrBadEmail     = errors.New("invalid email")
	ErrLoginFailed  = errors.New("invalid email or password")
	ErrInvalidToken = errors.New("invalid or expired token")
)

// User 用户
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
}

// Service 认证服务
type Service struct {
	mu           sync.RWMutex
	usersByID    map[string]*User
	usersByEmail map[string]*User
	secret       []byte
	seq          atomic.Int64
}

func New(secret string) *Service {
	return &Service{
		usersByID:    make(map[string]*User),
		usersByEmail: make(map[string]*User),
		secret:       []byte(secret),
	}
}

// Register 注册新用户
func (s *Service) Register(email, password string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(email) < 3 {
		return nil, ErrBadEmail
	}
	if len(password) < 8 {
		return nil, ErrBadPassword
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.usersByEmail[email]; exists {
		return nil, ErrEmailTaken
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	u := &User{
		ID:           fmt.Sprintf("U%08d", s.seq.Add(1)),
		Email:        email,
		PasswordHash: string(hash),
		CreatedAt:    time.Now(),
	}
	s.usersByID[u.ID] = u
	s.usersByEmail[email] = u
	return u, nil
}

// Login 登录，返回 JWT（24h）
func (s *Service) Login(email, password string) (string, *User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	s.mu.RLock()
	u, ok := s.usersByEmail[email]
	s.mu.RUnlock()
	if !ok {
		return "", nil, ErrLoginFailed
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return "", nil, ErrLoginFailed
	}
	token, err := s.sign(u.ID)
	if err != nil {
		return "", nil, err
	}
	return token, u, nil
}

// UserFromToken 解析 JWT 并返回用户
func (s *Service) UserFromToken(tokenStr string) (*User, error) {
	claims := &jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return s.secret, nil
	})
	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}
	sub, ok := (*claims)["sub"].(string)
	if !ok {
		return nil, ErrInvalidToken
	}
	s.mu.RLock()
	u, ok := s.usersByID[sub]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrInvalidToken
	}
	return u, nil
}

func (s *Service) sign(userID string) (string, error) {
	claims := jwt.MapClaims{
		"sub": userID,
		"exp": time.Now().Add(24 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}
