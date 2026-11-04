// Package config 负责 CP 的 YAML 配置加载。
// 字段与默认值对应说明书：UPF 对端地址（5.10）、UP socket Endpoint（6.1.1）、
// SQLite 文件（10.2）、分页步长与快照批大小（5.10.4 / 6.7.2）。
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config 是 CP 的全部运行配置。
type Config struct {
	Active   bool   `yaml:"active"`   // 仅 Active CP 允许向 UP 写规则、向 UPF 注册订阅
	DBPath   string `yaml:"dbPath"`   // SQLite 文件，默认 /var/lib/fusion-access-gw/cp.db
	LogLevel string `yaml:"logLevel"` // debug / info / warn / error

	UPF struct {
		BaseURL string `yaml:"baseURL"` // 如 https://10.216.4.160:8153；联调可用 http://127.0.0.1:18153
		User    string `yaml:"user"`    // UPF 内置用户，如 ehgw
		// PwdMD5 为 32 位小写 MD5（UPF V1.1 要求），请勿在配置文件中放明文密码。
		PwdMD5 string `yaml:"pwdMD5"`
		// CallbackURL 是 CP 提供给 UPF 的回调地址，UPF 用 POST 向该地址推送
		// UE create/delete。示例：http://10.0.0.10:8080/ue/callback
		CallbackURL string `yaml:"callbackURL"`
		// TokenTTLMin 为 token 有效期（UPF 默认 30 分钟），用于提前刷新计算。
		TokenTTLMin int `yaml:"tokenTTLMin"`
		// TokenRefreshBeforeSec 为过期前多少秒提前刷新。
		TokenRefreshBeforeSec int `yaml:"tokenRefreshBeforeSec"`
		// PingIntervalSec 为 RUNNING 阶段心跳间隔；心跳失败不等价 UE 全部下线。
		PingIntervalSec int `yaml:"pingIntervalSec"`
		TimeoutSec      int `yaml:"timeoutSec"`
	} `yaml:"upf"`

	Callback struct {
		Listen string `yaml:"listen"` // 回调 HTTP 服务监听地址，如 0.0.0.0:8080
		Path   string `yaml:"path"`   // 回调路径，如 /ue/callback
	} `yaml:"callback"`

	OAM struct {
		Listen string `yaml:"listen"` // OAM 配置接口监听地址，如 127.0.0.1:10001
		Path   string `yaml:"path"`   // 如 /cp/v1/common/call
	} `yaml:"oam"`

	UP struct {
		// Socket 为 CP-UP socket Endpoint（说明书 6.1.1，建议 UDS）。
		Socket string `yaml:"socket"` // 如 unix:///var/run/fusion-access-gw/up.sock
		// SnapshotBatchSize 为 RULE_SYNC 批次大小，默认 500，允许 100~1000。
		SnapshotBatchSize int `yaml:"snapshotBatchSize"`
		TimeoutSec        int `yaml:"timeoutSec"`
		// MaxRetry 为单条规则 UPSERT/DELETE 的有限重试次数（不含首次）。
		MaxRetry int `yaml:"maxRetry"`
	} `yaml:"up"`

	Sessions struct {
		// PageStep 为全量会话分页步长，UPF 默认 10；联调建议 100。
		PageStep int `yaml:"pageStep"`
	} `yaml:"sessions"`
}

// Load 读 YAML 配置文件并补默认值。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.DBPath == "" {
		c.DBPath = "/var/lib/fusion-access-gw/cp.db"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.UPF.TokenTTLMin <= 0 {
		c.UPF.TokenTTLMin = 30
	}
	if c.UPF.TokenRefreshBeforeSec <= 0 {
		c.UPF.TokenRefreshBeforeSec = 300
	}
	if c.UPF.PingIntervalSec <= 0 {
		c.UPF.PingIntervalSec = 30
	}
	if c.UPF.TimeoutSec <= 0 {
		c.UPF.TimeoutSec = 10
	}
	if c.Callback.Listen == "" {
		c.Callback.Listen = "0.0.0.0:8080"
	}
	if c.Callback.Path == "" {
		c.Callback.Path = "/ue/callback"
	}
	if c.OAM.Listen == "" {
		c.OAM.Listen = "127.0.0.1:10001"
	}
	if c.OAM.Path == "" {
		c.OAM.Path = "/cp/v1/common/call"
	}
	if c.UP.Socket == "" {
		c.UP.Socket = "unix:///var/run/fusion-access-gw/up.sock"
	}
	if c.UP.SnapshotBatchSize <= 0 {
		c.UP.SnapshotBatchSize = 500
	}
	if c.UP.TimeoutSec <= 0 {
		c.UP.TimeoutSec = 10
	}
	if c.UP.MaxRetry < 0 {
		c.UP.MaxRetry = 0
	}
	if c.UP.MaxRetry == 0 {
		c.UP.MaxRetry = 3
	}
	if c.Sessions.PageStep <= 0 {
		c.Sessions.PageStep = 100
	}
}

func (c *Config) validate() error {
	if c.UPF.BaseURL == "" {
		return fmt.Errorf("upf.baseURL is required")
	}
	if c.UPF.User == "" || c.UPF.PwdMD5 == "" {
		return fmt.Errorf("upf.user and upf.pwdMD5 are required")
	}
	if c.UPF.CallbackURL == "" {
		return fmt.Errorf("upf.callbackURL is required")
	}
	return nil
}
