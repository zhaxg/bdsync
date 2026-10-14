package baidu

import "strings"

// cookieKeepKeys — 真正需要的 cookie 项。
//
// 浏览器整行复制出来的 cookie 有几十项（PSTM、RT、PANPSC、BDCLND、各种 Hm_lvt…），
// 与网盘上传无关的占绝大多数，其中还不乏长期有效的标识。全串照单全收有两个坏处：
// credentials.dat 里躺着一堆用不上的凭据；而某些值本身不合法（百度的 RT 是带引号的
// `"…"`），net/http 每次带它出门都会打一行
//
//	net/http: invalid byte '"' in Cookie.Value; dropping invalid bytes
//
// 实测 BDUSS + STOKEN（列目录必需）+ BAIDUID 就够跑。
var cookieKeepKeys = map[string]bool{
	"BDUSS":   true,
	"STOKEN":  true,
	"BAIDUID": true,
}

// SanitizeCookie — 规范化 cookie 串，两件事一起做：
//
//  1. 只保留 cookieKeepKeys 里的三项（键统一大写输出）
//  2. 清掉值里 net/http 会丢弃并告警的字节（0x20 以下、0x7f 以上，以及 " ; \）
//
// 保持原有顺序，用 "; " 重新拼接。解析不了的片段（没有 '='）直接丢弃 ——
// cookie 本体就是 k=v 列表。结果可能为空串（例如压根没给凭据），调用方自行判断。
//
// 入口有三个：看板/向导粘贴（cookieFromPayload）、环境变量、credentials.dat 读回。
// 统一在这里过一遍，老文件里存的整串 cookie 也能自愈，不必等人重新粘一次。
func SanitizeCookie(cookie string) string {
	var keep []string
	for _, p := range strings.Split(cookie, ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		k = strings.ToUpper(strings.TrimSpace(k))
		if !cookieKeepKeys[k] {
			continue
		}
		if v = sanitizeCookieValue(v); v == "" {
			continue
		}
		keep = append(keep, k+"="+v)
	}
	return strings.Join(keep, "; ")
}

// sanitizeCookieValue — 逐字节剔除 RFC 6265 cookie-octet 之外的字符。
// 字节级过滤是有意的：cookie 值本就该是 ASCII，混进 UTF-8 只能说明粘错了东西。
func sanitizeCookieValue(v string) string {
	v = strings.TrimSpace(v)
	out := make([]byte, 0, len(v))
	for i := 0; i < len(v); i++ {
		b := v[i]
		if b < 0x20 || b >= 0x7f || b == '"' || b == ';' || b == '\\' {
			continue
		}
		out = append(out, b)
	}
	return strings.TrimSpace(string(out))
}
