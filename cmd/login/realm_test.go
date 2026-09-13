package main

import (
	"reflect"
	"testing"
)

// TestParseRealmArgs 覆盖 --realm 参数解析：缺省 cn、等号/分离式写法、
// 非法值/缺值报错、大小写归一、剥离 flag 后剩余参数保持相对顺序。
func TestParseRealmArgs(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantRealm string
		wantRest  []string
		wantErr   bool
	}{
		{name: "缺省 cn", args: []string{"url"}, wantRealm: "cn", wantRest: []string{"url"}},
		{name: "等号 global 在前", args: []string{"--realm=global", "url"}, wantRealm: "global", wantRest: []string{"url"}},
		{name: "等号 cn 在后", args: []string{"poll", "--realm=cn"}, wantRealm: "cn", wantRest: []string{"poll"}},
		{name: "分离式 global", args: []string{"--realm", "global", "url"}, wantRealm: "global", wantRest: []string{"url"}},
		{name: "非法值报错", args: []string{"--realm=foo", "url"}, wantErr: true},
		{name: "分离式缺值报错", args: []string{"--realm", "url"}, wantErr: true},
		{name: "大小写归一", args: []string{"--realm=GLOBAL", "url"}, wantRealm: "global", wantRest: []string{"url"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			realm, rest, err := parseRealmArgs(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseRealmArgs(%v) err=nil, want error", c.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRealmArgs(%v) err=%v", c.args, err)
			}
			if realm != c.wantRealm {
				t.Errorf("realm=%q want %q", realm, c.wantRealm)
			}
			if !reflect.DeepEqual(rest, c.wantRest) {
				t.Errorf("rest=%v want %v", rest, c.wantRest)
			}
		})
	}
}