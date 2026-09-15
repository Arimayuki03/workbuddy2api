// config_admin_test.go — admin 配置段：缺省关闭 + 冷却默认值/兜底。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAdminConfigDefaultsAndNormalize(t *testing.T) {
	c := Default()
	if c.Admin.Enabled {
		t.Fatal("admin 必须缺省关闭")
	}
	if c.Admin.CreditRefreshMinIntervalSec != 600 {
		t.Fatalf("冷却默认应为 600，实得 %d", c.Admin.CreditRefreshMinIntervalSec)
	}
	// normalize 兜底：显式 0/负数回落 600（不给"配错=无冷却"留口子）
	c.Admin.CreditRefreshMinIntervalSec = 0
	c.normalizePrompt() // 绕开 normalize() 其余段对空值的连锁要求，直接验证目标兜底
	if c.Admin.CreditRefreshMinIntervalSec != 0 {
		t.Fatal("normalizePrompt 不应碰 admin 段")
	}
	_ = c.normalize() // 若 normalize 因别的段报错也无妨，此处只看 admin 兜底是否先执行
	if c.Admin.CreditRefreshMinIntervalSec != 600 {
		t.Fatalf("显式 0 应回落 600，实得 %d", c.Admin.CreditRefreshMinIntervalSec)
	}
}

func TestAdminConfigLoadFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"admin":{"enabled":true,"credit_refresh_min_interval_sec":120}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Admin.Enabled || c.Admin.CreditRefreshMinIntervalSec != 120 {
		t.Fatalf("文件值未生效：%+v", c.Admin)
	}
}
