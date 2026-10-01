// Package config 加载并校验 YAML 配置。
//
// 只有一个 flag：-config config.yaml。选 YAML 是因为现场配置需要注释、
// OIDC 需要嵌套。
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type OIDC struct {
	Issuer           string `yaml:"issuer"`
	ClientID         string `yaml:"client_id"`
	ClientSecret     string `yaml:"client_secret"`
	RedirectURL      string `yaml:"redirect_url"`
	DisplayNameClaim string `yaml:"display_name_claim"`
}

type Config struct {
	Server struct {
		Addr          string `yaml:"addr"`
		AdminPassword string `yaml:"admin_password"`
	} `yaml:"server"`
	DB struct {
		Path string `yaml:"path"`
	} `yaml:"db"`
	Auth struct {
		HMACSecret string `yaml:"hmac_secret"`
		TokenTTL   string `yaml:"token_ttl"` // 如 "2h"；留空默认 2h
		OIDC       OIDC   `yaml:"oidc"`
	} `yaml:"auth"`

	TokenTTL time.Duration `yaml:"-"`
}

// Load 读配置并校验。任何必填项缺失都返回错误，调用方 log.Fatal 退出。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读配置文件 %s: %w", path, err)
	}
	c := &Config{}
	if err := yaml.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	missing := func(what, how string) error {
		return fmt.Errorf("配置缺失：%s 为空。%s", what, how)
	}
	if c.Auth.HMACSecret == "" {
		return missing("auth.hmac_secret", "用 `openssl rand -base64 32` 生成后填进 config.yaml（不自动生成，避免重启后旧 token 全部失效）。")
	}
	if c.Server.AdminPassword == "" {
		return missing("server.admin_password", "现场管理口令，填进 config.yaml。")
	}
	if c.Auth.OIDC.Issuer == "" {
		return missing("auth.oidc.issuer", "指到内网 Keycloak realm，例如 https://keycloak.intra/realms/wordle；本服务没有免 Keycloak 旁路。")
	}
	if c.Auth.OIDC.ClientID == "" {
		return missing("auth.oidc.client_id", "Keycloak 里 confidential client 的 client id。")
	}
	if c.Auth.OIDC.ClientSecret == "" {
		return missing("auth.oidc.client_secret", "Keycloak 里 confidential client 的 client secret。")
	}
	if c.Auth.OIDC.RedirectURL == "" {
		return missing("auth.oidc.redirect_url", "必须与 Keycloak client 里登记的一致，例如 http://192.168.1.10:8080/auth/callback。")
	}

	c.TokenTTL = 2 * time.Hour
	if c.Auth.TokenTTL != "" {
		d, err := time.ParseDuration(c.Auth.TokenTTL)
		if err != nil {
			return fmt.Errorf("auth.token_ttl 无法解析（%q）：%w", c.Auth.TokenTTL, err)
		}
		c.TokenTTL = d
	}
	if c.Auth.OIDC.DisplayNameClaim == "" {
		c.Auth.OIDC.DisplayNameClaim = "preferred_username"
	}
	// 剩余项给默认值，现场配置可以只写必填项。
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.DB.Path == "" {
		c.DB.Path = "data/wordle.db"
	}
	return nil
}
