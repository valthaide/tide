package cmd

import (
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiHeader = "\033[36m" // cyan
	ansiData   = "\033[37m" // light gray / white
	ansiGreen  = "\033[32m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
)

func colorEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPS_COLOR"))) {
	case "always", "1", "true", "yes", "on":
		return true
	case "never", "0", "false", "no", "off":
		return false
	}
	if strings.TrimSpace(os.Getenv("NO_COLOR")) != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func paint(enabled bool, code, s string) string {
	if !enabled || s == "" {
		return s
	}
	return code + s + ansiReset
}

func paintReady(enabled bool, ready string) string {
	switch ready {
	case "True":
		return paint(enabled, ansiGreen, ready)
	case "False":
		return paint(enabled, ansiRed, ready)
	default:
		return paint(enabled, ansiYellow, ready)
	}
}

func paintBool(enabled bool, value string) string {
	// Non-READY True/False stay in data color (not traffic-light).
	return paint(enabled, ansiData, value)
}

func colorHeader(enabled bool, s string) string {
	return paint(enabled, ansiBold+ansiHeader, s)
}

func colorData(enabled bool, s string) string {
	return paint(enabled, ansiData, s)
}

