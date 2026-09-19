package redisstore

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		token string
		want  string
	}{
		{"完整rediss", "rediss://default:tok@host:6379", "ignored", "rediss://default:tok@host:6379"},
		{"完整redis", "redis://default:tok@host:6379", "ignored", "redis://default:tok@host:6379"},
		{"https host", "https://foo.upstash.io", "tok", "rediss://default:tok@foo.upstash.io:6379"},
		{"裸host", "foo.upstash.io", "tok", "rediss://default:tok@foo.upstash.io:6379"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeURL(c.url, c.token); got != c.want {
				t.Errorf("normalizeURL(%q,%q)=%q want %q", c.url, c.token, got, c.want)
			}
		})
	}
}

func TestNormalizeURLStripsTrailingPath(t *testing.T) {
	// 用户照抄 Upstash 控制台的 REST 地址，可能带任意路径——剥 scheme 只取 host:port 之前段。
	got := normalizeURL("https://foo.upstash.io", "t")
	if strings.Contains(got, "://foo.upstash.io") && !strings.HasSuffix(got, "foo.upstash.io:6379") {
		t.Errorf("unexpected: %s", got)
	}
}

func TestNewEmptyURLReturnsNoop(t *testing.T) {
	if _, ok := New("", "").(Noop); !ok {
		t.Fatalf("empty url should return Noop")
	}
}

func TestNewBadSchemeReturnsNoop(t *testing.T) {
	// 组装出的连接串含空格 → ParseURL 解析失败 → 降级 Noop，不 panic、不发网络请求。
	if _, ok := New("://bad host", "").(Noop); !ok {
		t.Fatalf("bad url should return Noop")
	}
}

func TestNoopMethods(t *testing.T) {
	n := Noop{}
	n.SetBind("k", "u", time.Minute) // 不 panic
	n.DelBind("k")
	n.SaveState([]byte("{}"))
	if _, ok := n.LoadState(); ok {
		t.Error("Noop.LoadState should report not-found")
	}
}

func TestBindKeyPrefix(t *testing.T) {
	if got := bindKey("abc"); got != bindPrefix+"abc" {
		t.Errorf("bindKey=%q want prefix", got)
	}
}

func TestRedactURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // 精确期望；空串则只断言不含凭证（见下方 Contains 检查）
	}{
		// 1) userinfo 凭证：user:pass@ → ***@，host:port 保留（可定位问题）
		{"userinfo凭证", "rediss://default:SECRET@host:6379", "rediss://***@host:6379"},
		// 2) query 里的 password/token：值一律 ***（不做白名单，键名保留）
		{"token query", "rediss://host:6379?password=SECRET1&token=SECRET2", "rediss://host:6379?password=***&token=***"},
		// 2b) 只有部分键是凭证类的 query
		{"混合 query", "rediss://host:6379?dial_timeout=3&password=SECRET1", "rediss://host:6379?dial_timeout=***&password=***"},
		// 3) 纯畸形串（net/url 解析失败，但凭证在串里）：只保留合法 scheme
		{"畸形-host含空格", "rediss://default:SECRET@ho st:6379", "rediss://<redacted>"},
		// 3b) 畸形串：无 "://"，形如凭证的串不能被误当 scheme 打出
		{"畸形-无scheme", "default:SECRET@host", "<redacted>"},
		// 3c) 畸形串：scheme 位置是非法字符（可能藏凭证），整体打码
		{"畸形-scheme非法", "de fault:SECRET@host", "<redacted>"},
		// 附) path 里藏 token（Upstash REST 风格）：path 整体丢弃，query 值打码
		{"path藏token", "https://host/rest/SECRET/path?key=SECRET", "https://host?key=***"},
		// 附) 空串
		{"空串", "", "<redacted>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactURL(c.raw)
			if c.want != "" && got != c.want {
				t.Errorf("redactURL(%q)=%q want %q", c.raw, got, c.want)
			}
			for _, frag := range []string{"SECRET1", "SECRET2", "SECRET"} {
				if strings.Contains(got, frag) {
					t.Errorf("redactURL(%q)=%q 泄漏凭证片段 %q", c.raw, got, frag)
				}
			}
		})
	}
}

func TestParseErrReason(t *testing.T) {
	// *url.Error：Error() 以 %q 内嵌原始 URL（审计探针实测），只允许底层原因短语通过。
	raw := "rediss://default:SECRET@ho st:6379"
	_, err := url.Parse(raw)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("前置假设不成立：err 文本应内嵌原始 URL，got %q", err.Error())
	}
	if got := parseErrReason(err, raw); strings.Contains(got, "SECRET") || got == "" {
		t.Errorf("parseErrReason 泄漏或为空: %q", got)
	}

	// 非 *url.Error：全文剔除原始 URL 的原样与 %q 两种形态。
	raw2 := "rediss://default:SECRET@host:6379"
	err2 := fmt.Errorf("boom: %s", raw2)
	if got := parseErrReason(err2, raw2); strings.Contains(got, "SECRET") {
		t.Errorf("parseErrReason 泄漏: %q", got)
	}
	if got := parseErrReason(errors.New("redis: invalid URL scheme: redis"), raw2); got != "redis: invalid URL scheme: redis" {
		t.Errorf("无关错误应原样保留（其文本不含 URL）: %q", got)
	}
}

func TestNewBadURLLogDoesNotLeakToken(t *testing.T) {
	// 端到端验收：畸形连接串（含 token）走 New 的 ParseURL 失败路径，
	// 最终日志串不得出现 token（审计探针实测原实现会把完整 URL 打进日志）。
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	if _, ok := New("://bad host", "SECRET").(Noop); !ok {
		t.Fatal("bad url should return Noop")
	}
	got := buf.String()
	if !strings.Contains(got, "解析失败") {
		t.Errorf("日志缺少解析失败告警: %q", got)
	}
	if strings.Contains(got, "SECRET") {
		t.Errorf("日志泄漏凭证: %q", got)
	}
	if !strings.Contains(got, "rediss://<redacted>") {
		t.Errorf("日志应打 redactURL 后的形态: %q", got)
	}
}
