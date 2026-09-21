package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 网关完整配置
type Config struct {
	Gateway     GatewayConfig   `yaml:"gateway"`
	Auth        AuthConfig      `yaml:"auth"`         // 入口治理：API Key 鉴权 + 按 Key 限流
	Backends    []BackendConfig `yaml:"backends"`
	Models      []ModelConfig   `yaml:"models"`       // model 名 → 后端池 映射
	DefaultPool string          `yaml:"default_pool"` // 未匹配 model 的兜底池
}

// AuthConfig 入口治理配置。
//
// 网关此前只做「透明转发」：谁在调、调了多少、能不能调，全无管控。
// 这一层补上后，网关才配得上「控制平面」而不是「四层代理」。
type AuthConfig struct {
	// Enabled 是否启用鉴权。false（默认）时网关行为与之前完全一致，
	// 保证老用户升级后不会被突如其来的 401 打断。
	Enabled bool `yaml:"enabled"`

	// Header 取 Key 的请求头名。留空（默认）时同时接受两种写法：
	//   Authorization: Bearer <key>   ← OpenAI SDK / curl 惯例
	//   X-API-Key: <key>              ← 部分内部系统惯例
	// 显式指定（如 "X-Auth-Token"）则只认这一个头，值原样取用（Authorization 除外，仍按 Bearer 解析）。
	Header string `yaml:"header"`

	// ProtectMetrics 是否要求访问 /metrics 也带 Key。默认 false：
	// Prometheus 抓取通常在内网，强制鉴权会让监控链路配置变复杂却挡不住真正的攻击面。
	// 若网关直接暴露在公网，应设为 true。
	ProtectMetrics bool `yaml:"protect_metrics"`

	Keys []KeyConfig `yaml:"keys"`
}

// KeyConfig 单个 API Key
type KeyConfig struct {
	Name string `yaml:"name"` // 用于日志与指标标签的标识，如 "team-a"。不参与鉴权。

	// Key 明文 Key。启动时立即算 sha256 并只保留摘要，明文不长期驻留内存。
	// 与 KeyHash 二选一，两者都填以 KeyHash 为准。
	Key string `yaml:"key"`

	// KeyHash sha256 十六进制摘要（64 字符）。配置里不落明文，安全性更高，
	// 但排障时无法反推原始 Key，需自行保管映射关系。
	KeyHash string `yaml:"key_hash"`

	// RateLimit 该 Key 每秒允许的请求数（令牌桶填充速率）。0 = 不限流（只鉴权）。
	RateLimit float64 `yaml:"rate_limit"`

	// Burst 突发容量（令牌桶上限）。0 = 自动取 max(1, RateLimit)。
	// 推理请求天然是突发的（用户点一下发一批），纯 QPS 限制会把正常交互误杀，
	// 所以默认给一拍突发额度。
	Burst int `yaml:"burst"`
}

// ModelConfig 映射一个模型名到某个后端池
type ModelConfig struct {
	Name string `yaml:"name"` // 模型名，如 "qwen2.5-7b"
	Pool string `yaml:"pool"` // 所属后端池名
}

// GatewayConfig 网关自身配置
type GatewayConfig struct {
	Port       int    `yaml:"port"`        // 监听端口
	Timeout    string `yaml:"timeout"`     // 后端请求超时，如 "60s"
	MaxRetries int    `yaml:"max_retries"` // 失败重试次数（0=不重试，默认 1）

	// MaxBodySize 单个请求体的字节上限，如 "16MB" / "512KB" / "1G"。
	// Go 的 net/http 默认不限制请求体大小，任何人都能 POST 一个 GB 级 body 打满网关内存
	// （Nginx 默认 1MB，但对长上下文偏小），故必须显式设限。填 "0" 表示不限制（不推荐）。
	MaxBodySize string `yaml:"max_body_size"`

	// StreamIdleTimeout 流式（SSE）空闲超时：两次数据块之间的最大间隔，如 "120s"。
	// 超时会主动断开。首字节等待同样受它约束，所以它同时覆盖了「后端连上了但不返回」
	// 和「吐了一半卡死」两种故障。填 "0" 表示不启用（后端卡死时连接永久挂起，不建议）。
	StreamIdleTimeout string `yaml:"stream_idle_timeout"`

	// StreamMaxDuration 单条流的最长总时长，如 "10m"。兜底「极慢但一直在吐」的连接。
	// 填 "0" 表示不限制（默认）。注意：正常的多轮长生成不应被此项误杀，默认关闭。
	StreamMaxDuration string `yaml:"stream_max_duration"`
}

// BackendConfig 单个推理后端配置
type BackendConfig struct {
	Name                string `yaml:"name"`                  // 标识名，如 "vllm-4080"
	URL                 string `yaml:"url"`                   // 后端地址，如 "http://10.0.0.5:8000"
	HealthCheckPath     string `yaml:"health_check_path"`     // 健康检查路径，默认 "/health"。各家推理引擎约定不一：vLLM/TGI=/health，Ollama=/，SGLang=/health_generate
	HealthCheckTimeout  string `yaml:"health_check_timeout"`  // 健康检查独立超时，默认 3s（不复用网关请求超时，避免慢后端拖死探测）
	HealthCheckInterval string `yaml:"health_check_interval"` // 健康检查间隔，如 "10s"
	Pool                string `yaml:"pool"`                  // 所属后端池名，按 model 路由使用
}

// LoadConfig 从 YAML 文件加载配置
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Gateway: GatewayConfig{
			Port:              8080,
			Timeout:           "60s",
			MaxRetries:        1,
			MaxBodySize:       "16MB",
			StreamIdleTimeout: "120s",
			StreamMaxDuration: "0",
		},
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// 默认值
	if cfg.Gateway.Port == 0 {
		cfg.Gateway.Port = 8080
	}
	if cfg.Gateway.Timeout == "" {
		cfg.Gateway.Timeout = "60s"
	}
	if cfg.Gateway.MaxRetries < 0 {
		cfg.Gateway.MaxRetries = 0
	}
	// 三项新配置只在「完全缺省」时补默认值；显式写 "0" 是用户的主动选择，必须尊重。
	if cfg.Gateway.MaxBodySize == "" {
		cfg.Gateway.MaxBodySize = "16MB"
	}
	if cfg.Gateway.StreamIdleTimeout == "" {
		cfg.Gateway.StreamIdleTimeout = "120s"
	}
	if cfg.Gateway.StreamMaxDuration == "" {
		cfg.Gateway.StreamMaxDuration = "0"
	}

	// 鉴权配置的 fail-closed 校验：
	// 开了 auth.enabled 却没配任何可用 Key，等于「所有请求一律 401」——
	// 这是典型的配置漂移（改了 enabled 忘了加 keys，或 yaml 缩进错位导致 keys 没解析进来）。
	// 与其让网关带着一个全拒绝的配置启动、上线后才发现，不如启动即失败。
	if err := cfg.Auth.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validate 校验鉴权配置。只在 auth.enabled=true 时才要求 Key 非空。
func (a *AuthConfig) validate() error {
	if !a.Enabled {
		return nil
	}
	if len(a.Keys) == 0 {
		return fmt.Errorf("auth.enabled=true 但未配置任何 auth.keys —— 这会让所有请求一律 401；" +
			"请补充 keys，或把 auth.enabled 设为 false")
	}

	seenName := make(map[string]bool, len(a.Keys))
	for i, k := range a.Keys {
		if strings.TrimSpace(k.Name) == "" {
			return fmt.Errorf("auth.keys[%d] 缺少 name（name 用于日志与指标标签，必填）", i)
		}
		if seenName[k.Name] {
			return fmt.Errorf("auth.keys[%d] name %q 重复：重名会让按 Key 的用量归因无法区分", i, k.Name)
		}
		seenName[k.Name] = true

		if strings.TrimSpace(k.Key) == "" && strings.TrimSpace(k.KeyHash) == "" {
			return fmt.Errorf("auth.keys[%d] (%s) 必须填 key 或 key_hash 之一", i, k.Name)
		}
		if k.RateLimit < 0 {
			return fmt.Errorf("auth.keys[%d] (%s) rate_limit 不能为负数", i, k.Name)
		}
		if k.Burst < 0 {
			return fmt.Errorf("auth.keys[%d] (%s) burst 不能为负数", i, k.Name)
		}
	}
	return nil
}

// ParseSize 解析带单位的字节数：支持 "16MB" / "512K" / "1.5G" / "16777216"（纯数字按字节）。
// 单位按二进制换算（1KB=1024B）。空串与 "0" 均返回 0，语义为「不限制」。
//
// 标准库没有字节量解析器（time.ParseDuration 只管时间），为保持零第三方依赖自己写一个，
// 25 行换来一个人类可读的配置项，比让使用者填 16777216 划算。
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	// 分离数字前缀与单位后缀
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || s[i] == '+' || s[i] == '-') {
		i++
	}
	numStr, unit := s[:i], strings.TrimSpace(s[i:])
	if numStr == "" {
		return 0, fmt.Errorf("无法解析大小 %q：缺少数字部分", s)
	}
	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析大小 %q：%w", s, err)
	}

	var mult float64
	switch strings.ToUpper(unit) {
	case "", "B":
		mult = 1
	case "K", "KB", "KIB":
		mult = 1 << 10
	case "M", "MB", "MIB":
		mult = 1 << 20
	case "G", "GB", "GIB":
		mult = 1 << 30
	case "T", "TB", "TIB":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("无法解析大小 %q：未知单位 %q（支持 B/KB/MB/GB/TB）", s, unit)
	}

	v := num * mult
	if v < 0 {
		return 0, fmt.Errorf("大小不能为负数: %q", s)
	}
	return int64(v), nil
}
