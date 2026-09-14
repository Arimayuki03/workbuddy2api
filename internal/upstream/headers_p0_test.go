package upstream

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestCommonHeadersCodeBuddyRequest 所有出站请求统一注入
// X-CodeBuddy-Request: 1（官方客户端风控闸门头，D1）。
// chat/billing/refresh 三类出站均覆盖。
func TestCommonHeadersCodeBuddyRequest(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{}

	// chat 路径
	req := mustRequest(t)
	c.ChatHeaders(req, a, "", ChatMeta{})
	if got := req.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("chat X-CodeBuddy-Request = %q want %q", got, "1")
	}

	// billing 路径
	req2 := mustRequest(t)
	c.BillingHeaders(req2, a)
	if got := req2.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("billing X-CodeBuddy-Request = %q want %q", got, "1")
	}

	// refresh 路径
	req3 := mustRequest(t)
	c.RefreshHeaders(req3, a)
	if got := req3.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("refresh X-CodeBuddy-Request = %q want %q", got, "1")
	}
}
