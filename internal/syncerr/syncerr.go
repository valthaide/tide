package syncerr

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Common error codes for alerts and cps.
const (
	CodeGitAuth      = "git_auth"
	CodeGitFetch     = "git_fetch"
	CodeComposeApply = "compose_apply"
	CodeSecretsSOPS  = "secrets_sops"
	CodeSecretsVault = "secrets_vault"
	CodeCredentials  = "credentials"
	CodeTimeout        = "timeout"
	CodeEnvFileMissing = "env_file_missing"
	CodeUnknown        = "unknown"
)

// Stages of a sync pipeline.
const (
	StageCredentials = "credentials"
	StageFetch       = "fetch"
	StageSecrets     = "secrets"
	StageCompose     = "compose"
	StageUnknown     = "unknown"
)

// Error is a structured sync failure.
type Error struct {
	Code    string `json:"code"`
	Stage   string `json:"stage"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// GetCode returns the error code (for metrics).
func (e *Error) GetCode() string {
	if e == nil || e.Code == "" {
		return CodeUnknown
	}
	return e.Code
}

// GetStage returns the error stage (for metrics).
func (e *Error) GetStage() string {
	if e == nil || e.Stage == "" {
		return StageUnknown
	}
	return e.Stage
}

// New builds a structured error.
func New(code, stage, message string) *Error {
	code = strings.TrimSpace(code)
	stage = strings.TrimSpace(stage)
	message = strings.TrimSpace(message)
	if code == "" {
		code = CodeUnknown
	}
	if stage == "" {
		stage = StageUnknown
	}
	return &Error{Code: code, Stage: stage, Message: message}
}

// Wrap annotates an error with code/stage.
func Wrap(code, stage string, err error) *Error {
	if err == nil {
		return nil
	}
	var se *Error
	if errors.As(err, &se) && se != nil {
		if code != "" {
			se.Code = code
		}
		if stage != "" {
			se.Stage = stage
		}
		return se
	}
	return New(code, stage, err.Error())
}

// Encode serializes for DB storage (JSON). Empty if nil/empty message.
func Encode(err error) string {
	if err == nil {
		return ""
	}
	var se *Error
	if !errors.As(err, &se) || se == nil {
		se = New(CodeUnknown, StageUnknown, err.Error())
	}
	data, encErr := json.Marshal(se)
	if encErr != nil {
		return se.Message
	}
	return string(data)
}

// Decode parses a stored sync error into structured fields.
// Plaintext legacy values become code=unknown.
func Decode(raw string) (code, stage, message string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", ""
	}
	if strings.HasPrefix(raw, "{") {
		var se Error
		if err := json.Unmarshal([]byte(raw), &se); err == nil && (se.Code != "" || se.Message != "") {
			if se.Code == "" {
				se.Code = CodeUnknown
			}
			if se.Stage == "" {
				se.Stage = StageUnknown
			}
			if se.Message == "" {
				se.Message = raw
			}
			return se.Code, se.Stage, se.Message
		}
	}
	return CodeUnknown, StageUnknown, raw
}

// Classify maps a free-form apply/git error into code/stage heuristics.
func Classify(err error) *Error {
	if err == nil {
		return nil
	}
	var se *Error
	if errors.As(err, &se) && se != nil {
		return se
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "context deadline exceeded") || strings.Contains(lower, "timed out"):
		return New(CodeTimeout, StageCompose, msg)
	case strings.Contains(lower, "authentication") || strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "invalid deploy key") || strings.Contains(lower, "git token"):
		return New(CodeGitAuth, StageFetch, msg)
	case strings.Contains(lower, "env_file missing") || strings.Contains(lower, "env_file_missing"):
		return New(CodeEnvFileMissing, StageCompose, msg)
	case strings.Contains(lower, "prepare repo") || strings.Contains(lower, "fetch") || strings.Contains(lower, "clone"):
		return New(CodeGitFetch, StageFetch, msg)
	case strings.Contains(lower, "sops"):
		return New(CodeSecretsSOPS, StageSecrets, msg)
	case strings.Contains(lower, "vault"):
		return New(CodeSecretsVault, StageSecrets, msg)
	case strings.Contains(lower, "credential"):
		return New(CodeCredentials, StageCredentials, msg)
	case strings.Contains(lower, "compose") || strings.Contains(lower, "docker"):
		return New(CodeComposeApply, StageCompose, msg)
	default:
		return New(CodeUnknown, StageUnknown, msg)
	}
}

// FormatHuman returns a short display string.
func FormatHuman(code, stage, message string) string {
	message = strings.TrimSpace(message)
	if code == "" || code == CodeUnknown {
		return message
	}
	if stage == "" || stage == StageUnknown {
		return fmt.Sprintf("[%s] %s", code, message)
	}
	return fmt.Sprintf("[%s/%s] %s", code, stage, message)
}
