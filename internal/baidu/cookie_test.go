package baidu

import "testing"

func TestSanitizeCookie(t *testing.T) {
	// 浏览器整行复制：几十项里只留三项
	raw := "PSTM=1699999999; BDUSS=abc123; UID=123456; STOKEN=tok; " +
		"BAIDUID=DEADBEEF:FG; BDSFRCVID=xyz; H_PS_PSSID=1_2_3; PTOKEN=zzz"
	if got, want := SanitizeCookie(raw), "BDUSS=abc123; STOKEN=tok; BAIDUID=DEADBEEF:FG"; got != want {
		t.Fatalf("SanitizeCookie = %q, want %q", got, want)
	}

	// 顺序保持原样（浏览器里 BAIDUID 常在最前）
	if got := SanitizeCookie("BAIDUID=x; junk; BDUSS=y"); got != "BAIDUID=x; BDUSS=y" {
		t.Fatalf("顺序/过滤不符: %q", got)
	}
	// 无用项里带非法字节的（百度 RT 的值是 "…"）必须整项丢掉
	if got := SanitizeCookie(`BDUSS=a; RT="deadbeef"`); got != "BDUSS=a" {
		t.Fatalf("RT 该被丢弃: %q", got)
	}
	// 值外层的引号剥掉，内里的非法字节逐个剔除（"ab"c → abc）
	if got := SanitizeCookie(`BDUSS="ab"c;d; STOKEN=t`); got != "BDUSS=abc; STOKEN=t" {
		t.Fatalf("非法字节没清干净: %q", got)
	}
	// 键统一大写（小写 bduss 也能过 baidu.New 的 BDUSS= 检查）
	if got := SanitizeCookie("bduss=a; stoken=b"); got != "BDUSS=a; STOKEN=b" {
		t.Fatalf("键大小写未归一: %q", got)
	}
	// 非 ASCII / 控制字符一律剔除
	if got := SanitizeCookie("BDUSS=中文" + "\x00" + "abc"); got != "BDUSS=abc" {
		t.Fatalf("非 ASCII 未清: %q", got)
	}
	// 没有 BDUSS 的项也不会被伪造出来：留下的就是白名单里真实存在的
	if got := SanitizeCookie("PSTM=1; STOKEN=t"); got != "STOKEN=t" {
		t.Fatalf("应只剩 STOKEN: %q", got)
	}
	// 全是无用项 → 空串，由调用方判断（baidu.New 会报"缺少 BDUSS"）
	if got := SanitizeCookie("PSTM=1; UID=2"); got != "" {
		t.Fatalf("应为空: %q", got)
	}
	if got := SanitizeCookie(""); got != "" {
		t.Fatalf("空输入应为空: %q", got)
	}
}
