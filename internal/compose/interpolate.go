package compose

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MissingInterpolations returns variable names that are referenced without a
// default and are not present in env. Names are sorted and unique.
func MissingInterpolations(composeText string, env map[string]string) []string {
	seen := map[string]struct{}{}
	var missing []string
	for _, name := range interpolationNames(composeText) {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if strings.TrimSpace(env[name]) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// interpolationNames lists ${NAME} references that have no default.
// ${NAME:-default} and ${NAME-default} are skipped. ${NAME:?err} is required.
func interpolationNames(text string) []string {
	var names []string
	for i := 0; i < len(text); i++ {
		if text[i] != '$' {
			continue
		}
		if i+1 < len(text) && text[i+1] == '$' {
			i++
			continue
		}
		if i+1 >= len(text) || text[i+1] != '{' {
			continue
		}
		end := strings.IndexByte(text[i+2:], '}')
		if end < 0 {
			break
		}
		body := text[i+2 : i+2+end]
		i = i + 2 + end
		name, required := interpolationBody(body)
		if required && name != "" {
			names = append(names, name)
		}
	}
	return names
}

func interpolationBody(body string) (name string, required bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", false
	}
	i := 0
	for i < len(body) {
		c := body[i]
		if i == 0 {
			if c != '_' && !isAlpha(c) {
				return "", false
			}
		} else if c != '_' && !isAlphaNum(c) {
			break
		}
		i++
	}
	name = body[:i]
	rest := body[i:]
	switch {
	case rest == "":
		return name, true
	case strings.HasPrefix(rest, ":-"), strings.HasPrefix(rest, "-"):
		return name, false
	case strings.HasPrefix(rest, ":+"), strings.HasPrefix(rest, "+"):
		return name, false
	default:
		// :? and ? and anything else: the value is required.
		return name, true
	}
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isAlphaNum(c byte) bool {
	return isAlpha(c) || (c >= '0' && c <= '9')
}

// interpolationEnv merges process env, the compose project .env, and fleet vars.
// Fleet vars win.
func interpolationEnv(composeDir string, vars map[string]string) map[string]string {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		k, v, ok := strings.Cut(entry, "=")
		if !ok || k == "" {
			continue
		}
		env[k] = v
	}
	for k, v := range readDotEnv(composeDir) {
		env[k] = v
	}
	for k, v := range vars {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		env[k] = v
	}
	return env
}

func readDotEnv(dir string) map[string]string {
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" {
			out[k] = v
		}
	}
	return out
}
