package redact

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/valthaide/tide/internal/envcompat"
)

const minSecretLen = 8

var (
	userinfoURL = regexp.MustCompile(`(?i)(https?://)([^/@\s]+)@`)
	gitlabToken = regexp.MustCompile(`(?i)\b(glpat|gldt|glrt|glcbt)-[A-Za-z0-9_\-]+`)
)

// Redactor masks secrets and URL userinfo in strings.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
	extraMu sync.Mutex
	extras  []string
}

// Default is the process-wide redactor used by the slog wrapper and helpers.
var Default = New()

// New builds a redactor seeded from known secret env vars.
func New() *Redactor {
	r := &Redactor{}
	r.ReloadFromEnv()
	return r
}

// ReloadFromEnv refreshes the secret value list from the process environment.
func (r *Redactor) ReloadFromEnv() {
	if r == nil {
		return
	}
	var secrets []string
	for _, key := range envcompat.SecretEnvKeys() {
		if v := strings.TrimSpace(os.Getenv(key)); len(v) >= minSecretLen {
			secrets = append(secrets, v)
		}
	}
	r.mu.Lock()
	r.secrets = secrets
	r.mu.Unlock()
}

// AddSecret registers an additional secret value (e.g. app serviceEnvs / deploy key).
// Values shorter than minSecretLen are ignored to avoid masking everything.
func (r *Redactor) AddSecret(value string) {
	if r == nil {
		return
	}
	value = strings.TrimSpace(value)
	if len(value) < minSecretLen {
		return
	}
	r.extraMu.Lock()
	defer r.extraMu.Unlock()
	for _, existing := range r.extras {
		if existing == value {
			return
		}
	}
	r.extras = append(r.extras, value)
}

// AddSecrets registers many secret values.
func (r *Redactor) AddSecrets(values ...string) {
	for _, v := range values {
		r.AddSecret(v)
	}
}

// String redacts secrets from s.
func (r *Redactor) String(s string) string {
	if s == "" || r == nil {
		return s
	}
	out := userinfoURL.ReplaceAllString(s, `${1}***@`)
	out = gitlabToken.ReplaceAllStringFunc(out, func(m string) string {
		parts := strings.SplitN(m, "-", 2)
		if len(parts) != 2 {
			return "***"
		}
		return parts[0] + "-***"
	})

	r.mu.RLock()
	secrets := append([]string(nil), r.secrets...)
	r.mu.RUnlock()
	r.extraMu.Lock()
	extras := append([]string(nil), r.extras...)
	r.extraMu.Unlock()

	for _, secret := range append(secrets, extras...) {
		if secret == "" {
			continue
		}
		out = strings.ReplaceAll(out, secret, "***")
	}
	return out
}

// String is a convenience wrapper around Default.
func String(s string) string {
	if Default == nil {
		return s
	}
	return Default.String(s)
}

// Error redacts err.Error(); returns nil if err is nil.
func Error(err error) error {
	if err == nil {
		return nil
	}
	msg := String(err.Error())
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, cause: err}
}

type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }

// WrapSlog returns an slog.Handler that redacts messages and string attrs.
func WrapSlog(inner slog.Handler, r *Redactor) slog.Handler {
	if r == nil {
		r = Default
	}
	return &slogHandler{inner: inner, redactor: r}
}

type slogHandler struct {
	inner    slog.Handler
	redactor *Redactor
	attrs    []slog.Attr
	groups   []string
}

func (h *slogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *slogHandler) Handle(ctx context.Context, record slog.Record) error {
	record.Message = h.redactor.String(record.Message)
	var attrs []slog.Attr
	attrs = append(attrs, h.attrs...)
	record.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, h.redactAttr(a))
		return true
	})
	out := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	out.AddAttrs(attrs...)
	return h.inner.Handle(ctx, out)
}

func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cloned := *h
	cloned.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	for i := range cloned.attrs {
		cloned.attrs[i] = h.redactAttr(cloned.attrs[i])
	}
	return &cloned
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	cloned := *h
	cloned.groups = append(append([]string{}, h.groups...), name)
	cloned.inner = h.inner.WithGroup(name)
	return &cloned
}

func (h *slogHandler) redactAttr(a slog.Attr) slog.Attr {
	a.Value = h.redactValue(a.Value)
	return a
}

func (h *slogHandler) redactValue(v slog.Value) slog.Value {
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(h.redactor.String(v.String()))
	case slog.KindAny:
		if s, ok := v.Any().(string); ok {
			return slog.StringValue(h.redactor.String(s))
		}
		if err, ok := v.Any().(error); ok && err != nil {
			return slog.StringValue(h.redactor.String(err.Error()))
		}
	case slog.KindGroup:
		group := v.Group()
		out := make([]slog.Attr, len(group))
		for i, a := range group {
			out[i] = h.redactAttr(a)
		}
		return slog.GroupValue(out...)
	}
	return v
}

// StripUserinfoFromURL returns a copy of rawURL without userinfo, if it is an HTTP(S) URL.
func StripUserinfoFromURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User == nil {
		return rawURL
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return rawURL
	}
	parsed.User = nil
	return parsed.String()
}
