package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintAlignedTableKeepsColumnsWithANSI(t *testing.T) {
	var buf bytes.Buffer
	headers := []string{"NAME", "READY", "STATUS"}
	rows := [][]string{
		{"monitoring", "True", "synced"},
		{"x", "False", "error"},
	}
	printAlignedTableTo(&buf, headers, rows,
		func(_ int, plain string) string { return colorHeader(true, plain) },
		func(col int, plain string) string {
			if col == 1 {
				return paintReady(true, plain)
			}
			return colorData(true, plain)
		},
	)
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got %d\n%s", len(lines), out)
	}
	// Strip ANSI and compare column starts via fixed gaps.
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = stripANSI(line)
	}
	// NAME column width = len("monitoring")=10; READY starts at 12.
	for i, p := range plain {
		if !strings.HasPrefix(strings.TrimRight(p, " "), "NAME") && i == 0 {
			if !strings.Contains(p, "NAME") {
				t.Fatalf("header missing NAME: %q", p)
			}
		}
	}
	readyIdx := strings.Index(plain[0], "READY")
	if readyIdx < 0 {
		t.Fatalf("READY not found in %q", plain[0])
	}
	for i := 1; i < len(plain); i++ {
		if strings.Trim(plain[i], "- ") == "" {
			continue
		}
		rest := plain[i]
		if len(rest) < readyIdx+4 {
			t.Fatalf("row too short: %q", rest)
		}
		cell := strings.TrimSpace(rest[readyIdx : readyIdx+5])
		if cell != "True" && cell != "False" {
			t.Fatalf("row %d READY misaligned: %q (from %q)", i, cell, rest)
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') {
				inEsc = false
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
