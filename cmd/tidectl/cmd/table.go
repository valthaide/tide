package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// printAlignedTable prints a fixed-width table. Cells may contain ANSI codes;
// width is computed from visible (non-ANSI) rune length so columns stay aligned.
func printAlignedTable(headers []string, rows [][]string, paintHeader, paintCell func(col int, plain string) string) {
	printAlignedTableTo(os.Stdout, headers, rows, paintHeader, paintCell)
}

func printAlignedTableTo(w io.Writer, headers []string, rows [][]string, paintHeader, paintCell func(col int, plain string) string) {
	if len(headers) == 0 {
		return
	}
	cols := len(headers)
	widths := make([]int, cols)
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, row := range rows {
		for i := 0; i < cols && i < len(row); i++ {
			if n := utf8.RuneCountInString(row[i]); n > widths[i] {
				widths[i] = n
			}
		}
	}

	writePad := func(plain string, col int, painter func(int, string) string) {
		pad := widths[col] - utf8.RuneCountInString(plain)
		if pad < 0 {
			pad = 0
		}
		cell := plain
		if painter != nil {
			cell = painter(col, plain)
		}
		fmt.Fprint(w, cell)
		if pad > 0 {
			fmt.Fprint(w, strings.Repeat(" ", pad))
		}
	}

	for i, h := range headers {
		if i > 0 {
			fmt.Fprint(w, "  ")
		}
		writePad(h, i, paintHeader)
	}
	fmt.Fprintln(w)
	for i := range headers {
		if i > 0 {
			fmt.Fprint(w, "  ")
		}
		fmt.Fprint(w, strings.Repeat("-", widths[i]))
	}
	fmt.Fprintln(w)

	for _, row := range rows {
		for i := 0; i < cols; i++ {
			if i > 0 {
				fmt.Fprint(w, "  ")
			}
			plain := ""
			if i < len(row) {
				plain = row[i]
			}
			writePad(plain, i, paintCell)
		}
		fmt.Fprintln(w)
	}
}
