package main

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Config 网关完整配置
type Config struct {
	Gateway  GatewayConfig   `yaml:"gateway"`
	Backends []BackendConfig `yaml:"backends"`
}

// GatewayConfig 网关自身配置
type GatewayConfig struct {
	Port       int    `yaml:"port"`        // 监听端口
	Timeout    string `yaml:"timeout"`     // 后端请求超时，如 "60s"
	MaxRetries int    `yaml:"max_retries"` // 失败重试次数（0=不重试，默认 1）
}

// BackendConfig 单个 vLLM 后端配置
type BackendConfig struct {
	Name                string `yaml:"name"`                  // 标识名，如 "vllm-4080"
	URL                 string `yaml:"url"`                   // 后端地址，如 "http://10.0.0.5:8000"
	HealthCheckInterval string `yaml:"health_check_interval"` // 健康检查间隔，如 "10s"
}

// LoadConfig 从 YAML 文件加载配置
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Gateway: GatewayConfig{
			Port:       8080,
			Timeout:    "60s",
			MaxRetries: 1,
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

	return cfg, nil
}
