package netdiskname

import "testing"

func TestValidate(t *testing.T) {
	// 注意：这两个是**被测输入**（netdiskname 要能接受 emoji；要能识别上标保留名变体），
	// 所以用码点转义写，不写字面量 —— 任何批量字符替换都会误伤它们，
	// 而那时编译照过、测试语义静默改变，最难查。
	const (
		emojiFile      = "emoji\U0001F389.bin"
		lptSuperscript = "LPT\u00B3\u2074"
	)
	valid := []string{
		"", ".", "a.txt", "中文文件.txt", emojiFile, "space in name.txt",
		"archive.tar.gz", ".hidden", "COM10.txt", "COM1x.txt", "lpt0.dat",
		"a/b/c.txt", "CONSOLES.txt", "COM123", lptSuperscript,
	}
	for _, p := range valid {
		if err := Validate(p); err != nil {
			t.Errorf("Validate(%q) = %v, 期望通过", p, err)
		}
	}

	invalid := []struct {
		path string
		why  string
	}{
		{"a:b.txt", "非法字符"},
		{"a*b.txt", "非法字符"},
		{"a?b.txt", "非法字符"},
		{`a\b.txt`, "反斜杠"},
		{`a"b.txt`, "引号"},
		{"a<b>.txt", "尖括号"},
		{"a|b.txt", "竖线"},
		{"trailing.", "结尾点"},
		{"trailing ", "结尾空格"},
		{" leading.txt", "开头空格"},
		{"CON", "保留名"},
		{"con.txt", "保留名（基名）"},
		{"NUL.log", "保留名"},
		{"nul.txt.bak", "保留名（首点前基名，与 Windows RtlIsDosDeviceName 一致）"},
		{"com1.dat", "保留名"},
		{"LPT9", "保留名"},
		// 上标数字用码点转义：它们是**被测输入**（网盘保留名的变体），
		// 不是输出字符。写成字面量会被任何批量字符替换误伤，届时编译能过、
		// 测试语义静默改变，最难查。
		{"COM\u00B9", "上标变体保留名（COM\u00B9 → com1）"},
		{"lpt\u00B3.txt", "上标变体保留名（lpt\u00B3 → lpt3）"},
		{"sub/COM2.txt", "子目录保留名"},
	}
	for _, c := range invalid {
		if err := Validate(c.path); err == nil {
			t.Errorf("Validate(%q) 通过了, 期望拦截（%s）", c.path, c.why)
		}
	}
}

func TestMarked(t *testing.T) {
	if !Marked(ErrPrefix + "x") {
		t.Error("Marked 应识别本包前缀")
	}
	if Marked("分片 1/3 失败") {
		t.Error("Marked 不应误判普通错误")
	}
}
