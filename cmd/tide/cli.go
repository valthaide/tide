package main

import (
	"fmt"
	"io"
	"strings"
)

type serverCLIOptions struct {
	DataDir string
	Exit    bool
	Code    int
}

const tideUsageText = `Tide — GitOps Docker Compose controller

Usage:
  tide [flags]

Flags:
  --data-dir PATH   Controller state directory (default: TIDE_DATA_DIR, /data, /opt/tide/data, or .)
  -h, --help        Print this help message and exit
  -v, --version     Print controller version and exit

Environment:
  TIDE_API_ADDR             HTTPS API listen address (default :8443)
  TIDE_METRICS_ADDR         Metrics/health listen address (default 127.0.0.1:8081)
  TIDE_API_TOKEN            Admin API bearer token
  TIDE_FLEET_REPO_URL       Fleet Git repository URL
  TIDE_FLEET_HOST           Host name in fleet configuration
  TIDE_GIT_TOKEN            Git HTTPS token (read_repository)
  TIDE_GITLAB_API_TOKEN     GitLab API token for CI pipeline gate (read_api)
`

// parseServerArgs handles --help, --version, and flag validation before touching DB, files, locks, or Docker.
func parseServerArgs(args []string, version string, stdout, stderr io.Writer) serverCLIOptions {
	var opts serverCLIOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			_, _ = io.WriteString(stdout, tideUsageText)
			return serverCLIOptions{Exit: true, Code: 0}
		case arg == "-v" || arg == "--version" || arg == "version":
			v := strings.TrimSpace(version)
			if v == "" {
				v = "dev"
			}
			_, _ = fmt.Fprintf(stdout, "tide %s\n", v)
			return serverCLIOptions{Exit: true, Code: 0}
		case arg == "--data-dir":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
				_, _ = fmt.Fprintln(stderr, "error: --data-dir requires a directory path\nRun 'tide --help' for usage.")
				return serverCLIOptions{Exit: true, Code: 2}
			}
			i++
			opts.DataDir = strings.TrimSpace(args[i])
		case strings.HasPrefix(arg, "--data-dir="):
			val := strings.TrimSpace(strings.TrimPrefix(arg, "--data-dir="))
			if val == "" {
				_, _ = fmt.Fprintln(stderr, "error: --data-dir requires a directory path\nRun 'tide --help' for usage.")
				return serverCLIOptions{Exit: true, Code: 2}
			}
			opts.DataDir = val
		default:
			_, _ = fmt.Fprintf(stderr, "error: unknown flag or argument %q\nRun 'tide --help' for usage.\n", arg)
			return serverCLIOptions{Exit: true, Code: 2}
		}
	}
	return opts
}
