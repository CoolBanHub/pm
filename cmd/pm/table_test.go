package main

import (
	"strings"
	"testing"
)

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"中文", 4},    // 两个 CJK 表意文字，各 2 宽
		{"a中b", 4},   // 1 + 2 + 1
		{"ＡＢ", 4},    // 全角 ASCII，各 2 宽
		{"：￥", 4},    // 全角符号（U+FF1A / U+FFE5）
		{"🎉", 2},     // emoji
		{"abc\n", 3}, // 控制字符不计宽
		{"api-1", 5},
	}
	for _, c := range cases {
		if got := displayWidth(c.in); got != c.want {
			t.Errorf("displayWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestRenderTableAlignment(t *testing.T) {
	rows := [][]string{
		{"NAME", "STATE"},
		{"用户网关", "RUNNING"},
		{"api", "STOPPED"},
	}
	// 第 0 列最大显示宽度为 8（"用户网关"），pad = 列宽 - 显示宽 + 2：
	// NAME(4) 后补 6 空格、用户网关(8) 后补 2 空格、api(3) 后补 7 空格，
	// 三行的第二列起始位置（10 个显示列宽）完全对齐。
	want := "NAME" + strings.Repeat(" ", 6) + "STATE" + "\n" +
		"用户网关" + strings.Repeat(" ", 2) + "RUNNING" + "\n" +
		"api" + strings.Repeat(" ", 7) + "STOPPED" + "\n"
	if got := renderTable(rows); got != want {
		t.Errorf("renderTable misaligned:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

func TestRenderTableEmptyAndRagged(t *testing.T) {
	if got := renderTable(nil); got != "" {
		t.Errorf("renderTable(nil) = %q, want empty", got)
	}
	// 行长度不一致时，按各列独立对齐；第 0 列被 "only-one"(8) 撑宽，
	// 因此 "a"(1) 后补 9 空格（列宽 8 - 1 + 2）。
	got := renderTable([][]string{{"a", "b"}, {"only-one"}})
	if want := "a" + strings.Repeat(" ", 9) + "b\nonly-one\n"; got != want {
		t.Errorf("renderTable ragged = %q, want %q", got, want)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"abc", 10, "abc"},   // 未超限原样返回
		{"abc", 3, "abc"},    // 恰好等于上限
		{"abc", 2, "a…"},     // 截断 + 省略号
		{"一二三四五", 7, "一二三…"}, // 3 个 CJK（6 宽）+ 省略号（1 宽）= 7
		{"一二三四五", 1, "…"},    // 上限仅容省略号
		{"任意串", 0, ""},       // 上限非正，返回空
	}
	for _, c := range cases {
		if got := truncate(c.in, c.max); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
	// 截断后显示宽度不应超过上限。
	if w := displayWidth(truncate("一二三四五六七八九十", 9)); w > 9 {
		t.Errorf("truncated width %d exceeds limit 9", w)
	}
}

func TestFormatTCPPorts(t *testing.T) {
	if got := formatTCPPorts([]int{443, 8080, 9000}); got != "443,8080,9000" {
		t.Fatalf("formatTCPPorts = %q", got)
	}
	if got := formatTCPPorts(nil); got != "-" {
		t.Fatalf("formatTCPPorts(nil) = %q", got)
	}
}
