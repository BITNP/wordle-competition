// Package auth 完成后端侧的 Keycloak 登录，换发 HMAC token。
//
// 只有这一种凭证，没有服务端会话、没有会话表、不存 refresh token。
// Keycloak 自己的 SSO cookie 就是续期依据：token 过期时前端重走一次
// /auth/login 静默换新，sub 不变，所以不丢本轮用时。
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"wordle/internal/config"
)

var (
	ErrNoToken     = errors.New("没有 Bearer token")
	ErrBadToken    = errors.New("token 签名错或格式错")
	ErrTokenExpiry = errors.New("token 已过期")
)

type Auth struct {
	secret []byte
	ttl    time.Duration
	oidc   config.OIDC

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func New(cfg *config.Config) *Auth {
	return &Auth{
		secret: []byte(cfg.Auth.HMACSecret),
		ttl:    cfg.TokenTTL,
		oidc:   cfg.Auth.OIDC,
	}
}

// ---------- HMAC token ----------

type payload struct {
	Sub  string `json:"sub"`
	Name string `json:"name"`
	Exp  int64  `json:"exp"`
}

// Issue 签发 token：base64url(payload) + "." + base64url(HMAC-SHA256(payload, secret))。
// 载荷里带 name 是因为没有用户表，昵称只能跟着 token 走；前端不解析、原样回传。
func (a *Auth) Issue(sub, name string, now time.Time) string {
	body, _ := json.Marshal(payload{
		Sub:  sub,
		Name: name,
		Exp:  now.Add(a.ttl).Unix(),
	})
	sig := a.sign(body)
	return b64(body) + "." + b64(sig)
}

// Verify 校验 Authorization: Bearer <token>，返回 sub 与昵称。
// HMAC 只防篡改，不证明身份；身份来源是 Keycloak。
func (a *Auth) Verify(r *http.Request) (string, string, error) {
	raw := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(raw, "Bearer ")
	if !ok || token == "" {
		return "", "", ErrNoToken
	}
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", "", ErrBadToken
	}
	rawBody, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", "", ErrBadToken
	}
	rawSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return "", "", ErrBadToken
	}
	if !hmac.Equal(rawSig, a.sign(rawBody)) {
		return "", "", ErrBadToken
	}
	var p payload
	if err := json.Unmarshal(rawBody, &p); err != nil || p.Sub == "" {
		return "", "", ErrBadToken
	}
	if time.Now().Unix() >= p.Exp {
		return "", "", ErrTokenExpiry
	}
	return p.Sub, p.Name, nil
}

func (a *Auth) sign(msg []byte) []byte {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write(msg)
	return mac.Sum(nil)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// ---------- 无状态 CSRF 防护 ----------

// NewState 生成 state = base64url(nonce) + "." + base64url(HMAC(nonce, secret))，
// nonce 同时作为 OIDC nonce 传给 Keycloak。不存 map，重启也不影响。
func (a *Auth) NewState() (state, nonce string) {
	buf := make([]byte, 16)
	rand.Read(buf)
	nonce = b64(buf)
	return nonce + "." + b64(a.sign([]byte(nonce))), nonce
}

// CheckState 校验 state 并取回 nonce。
func (a *Auth) CheckState(state string) (string, error) {
	nonce, sig, ok := strings.Cut(state, ".")
	if !ok || nonce == "" {
		return "", ErrBadToken
	}
	rawSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return "", ErrBadToken
	}
	if !hmac.Equal(rawSig, a.sign([]byte(nonce))) {
		return "", ErrBadToken
	}
	return nonce, nil
}

// ---------- OIDC ----------

// LoginURL 生成去 Keycloak 的地址。
//
// provider 是懒加载的：启动时不去打 Keycloak，免得 Keycloak 抖一下就开不了服；
// 真正登录时才发现不可达会返回明确的错误。
func (a *Auth) LoginURL(ctx context.Context, state, nonce string) (string, error) {
	oc, _, err := a.ensure(ctx)
	if err != nil {
		return "", err
	}
	return oc.AuthCodeURL(state, oidc.Nonce(nonce)), nil
}

// Exchange 用 code 换 id_token，验签名 / issuer / audience / nonce，取出 sub 与昵称。
func (a *Auth) Exchange(ctx context.Context, code, nonce string) (sub, name string, err error) {
	oc, verifier, err := a.ensure(ctx)
	if err != nil {
		return "", "", err
	}
	tok, err := oc.Exchange(ctx, code)
	if err != nil {
		return "", "", fmt.Errorf("换 code 失败: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return "", "", errors.New("keycloak 没返回 id_token")
	}
	idToken, err := verifier.Verify(ctx, rawID)
	if err != nil {
		return "", "", fmt.Errorf("校验 id_token 失败: %w", err)
	}
	if idToken.Nonce != nonce {
		return "", "", errors.New("id_token 的 nonce 不匹配")
	}
	var claims struct {
		Sub               string `json:"sub"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
		Email             string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "", "", fmt.Errorf("解析 id_token claims 失败: %w", err)
	}
	if claims.Sub == "" {
		return "", "", errors.New("id_token 里没有 sub")
	}
	name = a.displayName(claims.PreferredUsername, claims.Name, claims.Email, claims.Sub)
	return claims.Sub, name, nil
}

// displayName 按配置的 claim 取昵称，取不到就退回 sub。
func (a *Auth) displayName(preferred, full, email, sub string) string {
	switch a.oidc.DisplayNameClaim {
	case "name":
		if full != "" {
			return full
		}
	case "email":
		if email != "" {
			return email
		}
	default: // preferred_username（配置默认值）
		if preferred != "" {
			return preferred
		}
	}
	if preferred != "" {
		return preferred
	}
	return sub
}

// ensure 懒加载 OIDC provider（discovery）并缓存；失败不缓存，下次登录重试。
func (a *Auth) ensure(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.oauth != nil {
		return a.oauth, a.verifier, nil
	}
	oc := a.oidc
	provider, err := oidc.NewProvider(ctx, oc.Issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("连不上 Keycloak issuer %s: %w", oc.Issuer, err)
	}
	a.verifier = provider.Verifier(&oidc.Config{ClientID: oc.ClientID})
	a.oauth = &oauth2.Config{
		ClientID:     oc.ClientID,
		ClientSecret: oc.ClientSecret,
		RedirectURL:  oc.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
	return a.oauth, a.verifier, nil
}
