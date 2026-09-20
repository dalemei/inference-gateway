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
	Gateway    GatewayConfig   `yaml:"gateway"`
	Backends   []BackendConfig `yaml:"backends"`
	Models     []ModelConfig   `yaml:"models"`      // model 名 → 后端池 映射
	DefaultPool string         `yaml:"default_pool"` // 未匹配 model 的兜底池
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

	return cfg, nil
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
