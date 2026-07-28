package config

import (
	"strings"
	"testing"
)

func TestDescriptionValidation(t *testing.T) {
	configText := func(desc string) []byte {
		return []byte("programs:\n  - name: api\n    command: app\n    description: " + desc + "\n")
	}
	// 合法描述（含中文与全角标点）应通过并原样保留。
	cfg, err := Parse(configText(`"账号网关，承接登录注册"`))
	if err != nil {
		t.Fatalf("valid description rejected: %v", err)
	}
	if cfg.Programs[0].Description != "账号网关，承接登录注册" {
		t.Fatalf("description = %q", cfg.Programs[0].Description)
	}
	// 含换行（YAML 双引号转义为真实 LF）被拒。
	if _, err := Parse(configText(`"line1\nline2"`)); err == nil {
		t.Fatal("expected newline rejection")
	}
	// 超过 256 字符被拒。
	long := `"` + strings.Repeat("中", 257) + `"`
	if _, err := Parse(configText(long)); err == nil {
		t.Fatal("expected length rejection")
	}
}
