package giterr

import "strings"

const (
	ReasonAuth     = "auth"
	ReasonNetwork  = "network"
	ReasonNotFound = "not_found"
	ReasonOther    = "other"
)

// Reason classifies a git transport or resolution error.
func Reason(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "authentication") ||
		strings.Contains(msg, "authorization") ||
		strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "access denied") ||
		strings.Contains(msg, "invalid username") ||
		strings.Contains(msg, "invalid credentials") ||
		strings.Contains(msg, "http basic") ||
		strings.Contains(msg, "status code: 401") ||
		strings.Contains(msg, "status code: 403") ||
		strings.Contains(msg, " 401") ||
		strings.Contains(msg, " 403"):
		return ReasonAuth
	case strings.Contains(msg, "not found") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "status code: 404") ||
		strings.Contains(msg, "repository not found"):
		return ReasonNotFound
	case strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "tls") ||
		strings.Contains(msg, "temporary failure"):
		return ReasonNetwork
	default:
		return ReasonOther
	}
}
