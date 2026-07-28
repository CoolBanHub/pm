package main

import "strings"

// displayWidth 返回 s 在等宽终端中的近似显示列宽。
//
// 采用 Unicode East Asian Width 的近似实现：CJK 表意文字、全角符号、
// Hangul 音节、常用 emoji 符号块等记为 2；C0/C1 控制字符记为 0；其余记为 1。
// 不处理零宽连接符（ZWJ）组合的 emoji 序列与区域变体，但对进程名、分组、
// 备注（CJK 为主）的对齐场景足够。与 text/tabwriter 不同，它感知双宽字符，
// 因此含中文的行不会让后续列错位。
func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		width += runeDisplayWidth(r)
	}
	return width
}

// runeDisplayWidth 返回单个 rune 的近似显示列宽。
func runeDisplayWidth(r rune) int {
	if r == 0 || r < 0x20 || (r >= 0x7f && r < 0xa0) {
		// C0/C1 控制字符不计宽。
		return 0
	}
	if isWide(r) {
		return 2
	}
	return 1
}

// isWide 判断 r 是否属于 East Asian Wide/Fullwidth（双宽）范围。
// 范围参照 Unicode EastAsianWidth.txt 的 W/F 分类，覆盖常用中日韩、全角、emoji。
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F: // Hangul Jamo
		return true
	case r >= 0x2E80 && r <= 0x303E: // CJK 部首、康熙部首等
		return true
	case r >= 0x3041 && r <= 0x33FF: // 平假名/片假名/CJK 符号与标点/注音
		return true
	case r >= 0x3400 && r <= 0x4DBF: // CJK 统一表意文字扩展 A
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // CJK 统一表意文字
		return true
	case r >= 0xA000 && r <= 0xA4CF: // 彝文
		return true
	case r >= 0xAC00 && r <= 0xD7A3: // Hangul 音节
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK 兼容表意文字
		return true
	case r >= 0xFE10 && r <= 0xFE19: // 竖排形式
		return true
	case r >= 0xFE30 && r <= 0xFE6F: // CJK 兼容形式
		return true
	case r >= 0xFF00 && r <= 0xFF60: // 全角 ASCII 与标点
		return true
	case r >= 0xFFE0 && r <= 0xFFE6: // 全角符号
		return true
	case r >= 0x1F000 && r <= 0x1F02F: // 麻将牌
		return true
	case r >= 0x1F300 && r <= 0x1FAFF: // Emoji 符号与象形文字扩展
		return true
	case r >= 0x20000 && r <= 0x3FFFD: // CJK 统一表意文字扩展 B 及以后
		return true
	}
	return false
}

// renderTable 按显示列宽对齐渲染表格行。每列补齐到该列最大显示宽度，
// 列间以两个空格分隔；最后一列不补尾随空白。
func renderTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, row := range rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	widths := make([]int, cols)
	for _, row := range rows {
		for i := 0; i < len(row); i++ {
			if w := displayWidth(row[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}
	var b strings.Builder
	for _, row := range rows {
		for i := 0; i < len(row); i++ {
			b.WriteString(row[i])
			if i < len(row)-1 {
				pad := widths[i] - displayWidth(row[i]) + 2
				b.WriteString(strings.Repeat(" ", pad))
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// truncate 把 s 截断到不超过 maxDisplayWidth 的显示宽度；超出时以省略号
// "…"（U+2026，近似 1 宽）收尾。用于 CLI 表格中可能过长的描述列。
func truncate(s string, maxDisplayWidth int) string {
	if maxDisplayWidth <= 0 {
		return ""
	}
	if displayWidth(s) <= maxDisplayWidth {
		return s
	}
	const ellipsis = "…"
	limit := maxDisplayWidth - displayWidth(ellipsis)
	if limit <= 0 {
		return ellipsis
	}
	width := 0
	var out strings.Builder
	for _, r := range s {
		rw := runeDisplayWidth(r)
		if width+rw > limit {
			break
		}
		out.WriteRune(r)
		width += rw
	}
	out.WriteString(ellipsis)
	return out.String()
}
