package main

import "testing"

// 看板整行粘贴的 cookie 有几十项，只该留 BDUSS / STOKEN / BAIDUID，
// 且值里的引号等非法字节必须清掉 —— 否则 net/http 每次请求都会刷
// 「invalid byte '"' in Cookie.Value; dropping invalid bytes」。
func TestCookieFromPayloadTrims(t *testing.T) {
	cookie, warn, empty, err := cookieFromPayload(map[string]any{
		"cookie": "PSTM=1; BDUSS=abc; STOKEN=t; BAIDUID=b; PTOKEN=p; RT=\"quoted\"",
	})
	if err != nil {
		t.Fatalf("意外报错: %v", err)
	}
	if empty {
		t.Fatal("不该判空")
	}
	if cookie != "BDUSS=abc; STOKEN=t; BAIDUID=b" {
		t.Fatalf("cookie = %q", cookie)
	}
	if warn == "" {
		t.Fatal("丢弃了字段就该在提示里说明")
	}

	// 值外层被引号包着（百度 RT 的写法）→ 引号必须剥掉
	cookie, _, _, err = cookieFromPayload(map[string]any{"cookie": `BDUSS="abc"; STOKEN="t"`})
	if err != nil {
		t.Fatalf("意外报错: %v", err)
	}
	if cookie != "BDUSS=abc; STOKEN=t" {
		t.Fatalf("引号没剥干净: %q", cookie)
	}

	// 缺 BDUSS → 报错（而不是悄悄存一串没用的字段）
	if _, _, _, err := cookieFromPayload(map[string]any{
		"cookie": "PSTM=1; STOKEN=t",
	}); err == nil {
		t.Fatal("缺少 BDUSS 应报错")
	}

	// 只有 BDUSS → 提示但不阻断
	cookie, warn, _, err = cookieFromPayload(map[string]any{"cookie": "PSTM=1; BDUSS=abc"})
	if err != nil || cookie != "BDUSS=abc" {
		t.Fatalf("cookie=%q err=%v", cookie, err)
	}
	if warn == "" {
		t.Fatal("缺 STOKEN 该有提醒")
	}

	// 分别填 BDUSS / STOKEN 的路径同样过规范化
	cookie, warn, _, err = cookieFromPayload(map[string]any{"bduss": "xyz", "stoken": "k"})
	if err != nil || cookie != "BDUSS=xyz; STOKEN=k" || warn != "" {
		t.Fatalf("cookie=%q warn=%q err=%v", cookie, warn, err)
	}
}
