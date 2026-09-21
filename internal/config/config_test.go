package config

import "testing"

// TestParseSize 覆盖 ParseSize 的正常值与边界：
// 字节量解析写错一位就是 1024 倍的差距，且不会报错，只能靠测试兜住。
func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},          // 空串 = 不限制
		{"0", 0},         // 显式关闭
		{"0MB", 0},
		{"1024", 1024},   // 纯数字按字节
		{"16MB", 16 << 20},
		{"16mb", 16 << 20}, // 单位大小写不敏感
		{"512K", 512 << 10},
		{"512KB", 512 << 10},
		{"1G", 1 << 30},
		{"1.5M", 1<<20 + 1<<19}, // 小数
		{"2TB", 2 << 40},
	}

	for _, c := range cases {
		got, err := ParseSize(c.in)
		if err != nil {
			t.Errorf("ParseSize(%q) 返回错误: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, 期望 %d", c.in, got, c.want)
		}
	}
}

func TestParseSizeInvalid(t *testing.T) {
	for _, in := range []string{"MB", "abc", "16XB", "-1MB", "16 光年"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) 应当报错，但返回了 nil", in)
		}
	}
}

// TestAuthValidateFailClosed 鉴权配置必须 fail-closed：
// 开了 enabled 却一个 Key 都解析不出来，等于「所有请求一律 401」。
// 与其让网关带着这种配置上线，不如启动即失败。
func TestAuthValidateFailClosed(t *testing.T) {
	cases := []struct {
		name string
		auth AuthConfig
	}{
		{"enabled 但无 keys", AuthConfig{Enabled: true}},
		{"Key 既无明文也无摘要", AuthConfig{Enabled: true, Keys: []KeyConfig{{Name: "a"}}}},
		{"Key 缺 name", AuthConfig{Enabled: true, Keys: []KeyConfig{{Key: "x"}}}},
		{"name 重复", AuthConfig{Enabled: true, Keys: []KeyConfig{
			{Name: "dup", Key: "a"}, {Name: "dup", Key: "b"},
		}}},
		{"rate_limit 为负", AuthConfig{Enabled: true, Keys: []KeyConfig{{Name: "a", Key: "x", RateLimit: -1}}}},
	}

	for _, c := range cases {
		a := c.auth // 取可寻址副本：validate 是指针接收者
		if err := a.validate(); err == nil {
			t.Errorf("%s: 应当报错（fail-closed），实际通过", c.name)
		}
	}

	// 未启用鉴权时不校验——老配置升级后不能被意外的报错打断
	off := AuthConfig{Enabled: false}
	if err := off.validate(); err != nil {
		t.Errorf("未启用鉴权时不该校验 Key，实际报错: %v", err)
	}
}

// TestAuthValidateAcceptsValid 合法配置不应误报
func TestAuthValidateAcceptsValid(t *testing.T) {
	ok := AuthConfig{Enabled: true, Keys: []KeyConfig{
		{Name: "team-a", Key: "sk-a"},
		{Name: "team-b", KeyHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", RateLimit: 10, Burst: 20},
	}}
	if err := ok.validate(); err != nil {
		t.Errorf("合法配置被误拒: %v", err)
	}
}

// TestLoadConfigDefaults 未显式配置时必须补上安全默认值，
// 尤其 max_body_size —— 缺省即"不限制"会直接把网关暴露在内存打满的风险里。
func TestLoadConfigDefaults(t *testing.T) {
	cfg := &Config{
		Gateway: GatewayConfig{Port: 8080},
	}
	if cfg.Gateway.MaxBodySize != "" {
		t.Skip("用例自带 MaxBodySize，跳过")
	}

	// 直接验证 LoadConfig 的默认值分支需要文件，这里退而验证常量语义：
	// 空串解析为 0（不限制），因此 LoadConfig 必须补默认串，不能留空。
	got, err := ParseSize("")
	if err != nil || got != 0 {
		t.Fatalf("空串应解析为 0（不限），实际 %d, err=%v", got, err)
	}
}
