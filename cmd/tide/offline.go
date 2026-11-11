package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/valthaide/tide/internal/fleet"
)

// Offline subcommands shipped in the controller image for fleet CI. They
// never touch the controller, the DB, Docker, or the network.
//
//	tide schema host [--version vX.Y.Z]   print the generated host JSON Schema
//	tide validate [flags] <files...>      validate fleet host files offline
//
// runOfflineCommand returns (exitCode, true) when args start with an
// offline subcommand, so main can run it instead of the server.
func runOfflineCommand(args []string, version string, stdout, stderr io.Writer) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "schema":
		return runSchemaCommand(args[1:], version, stdout, stderr), true
	case "validate":
		return runValidateCommand(args[1:], stdout, stderr), true
	}
	return 0, false
}

const schemaUsage = "usage: tide schema host [--version vX.Y.Z]\n"

func runSchemaCommand(args []string, version string, stdout, stderr io.Writer) int {
	ver := strings.TrimSpace(version)
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--version":
			if i+1 >= len(args) {
				_, _ = fmt.Fprint(stderr, "error: --version requires a value\n"+schemaUsage)
				return 2
			}
			i++
			ver = strings.TrimSpace(args[i])
		case strings.HasPrefix(arg, "--version="):
			ver = strings.TrimSpace(strings.TrimPrefix(arg, "--version="))
		case arg == "-h" || arg == "--help":
			_, _ = io.WriteString(stdout, schemaUsage)
			return 0
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) != 1 || positional[0] != "host" {
		_, _ = fmt.Fprint(stderr, "error: expected 'host'\n"+schemaUsage)
		return 2
	}
	schema, err := fleet.HostSchemaJSON(ver)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: generate schema: %v\n", err)
		return 1
	}
	_, _ = stdout.Write(schema)
	return 0
}

const validateUsage = "usage: tide validate [-o json|text] [--strict warn|enforce] [--with-compose] [--apps-dir DIR] <host-file.yaml> [more...]\n"

type validateCLIOptions struct {
	format      string
	strict      string
	withCompose bool
	appsDir     string
	files       []string
}

func parseValidateArgs(args []string) (validateCLIOptions, error) {
	opts := validateCLIOptions{format: "text"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		switch {
		case arg == "-o" || arg == "--format":
			v, err := next()
			if err != nil {
				return opts, err
			}
			opts.format = v
		case strings.HasPrefix(arg, "--format="):
			opts.format = strings.TrimPrefix(arg, "--format=")
		case strings.HasPrefix(arg, "-o="):
			opts.format = strings.TrimPrefix(arg, "-o=")
		case arg == "--strict":
			v, err := next()
			if err != nil {
				return opts, err
			}
			opts.strict = v
		case strings.HasPrefix(arg, "--strict="):
			opts.strict = strings.TrimPrefix(arg, "--strict=")
		case arg == "--with-compose":
			opts.withCompose = true
		case arg == "--apps-dir":
			v, err := next()
			if err != nil {
				return opts, err
			}
			opts.appsDir = v
		case strings.HasPrefix(arg, "--apps-dir="):
			opts.appsDir = strings.TrimPrefix(arg, "--apps-dir=")
		case arg == "-h" || arg == "--help":
			return opts, fmt.Errorf("help")
		case strings.HasPrefix(arg, "-") && arg != "-":
			return opts, fmt.Errorf("unknown flag %q", arg)
		default:
			opts.files = append(opts.files, arg)
		}
	}
	if opts.format != "text" && opts.format != "json" {
		return opts, fmt.Errorf("unknown format %q (want text or json)", opts.format)
	}
	return opts, nil
}

type validateFilesOutput struct {
	OK    bool           `json:"ok"`
	Files []fleet.Report `json:"files"`
}

func runValidateCommand(args []string, stdout, stderr io.Writer) int {
	opts, err := parseValidateArgs(args)
	if err != nil {
		if err.Error() == "help" {
			_, _ = io.WriteString(stdout, validateUsage)
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "error: %v\n%s", err, validateUsage)
		return 2
	}
	if len(opts.files) == 0 {
		_, _ = fmt.Fprint(stderr, "error: no host files given\n"+validateUsage)
		return 2
	}
	reports := fleet.ValidateFiles(opts.files, fleet.ValidateOptions{
		AppsDir:     opts.appsDir,
		WithCompose: opts.withCompose,
		Strict:      opts.strict,
	})
	ok := fleet.ReportsOK(reports)
	if opts.format == "json" {
		out, _ := json.MarshalIndent(validateFilesOutput{OK: ok, Files: reports}, "", "  ")
		_, _ = fmt.Fprintln(stdout, string(out))
	} else {
		for _, report := range reports {
			if report.OK && len(report.Unknown) == 0 {
				_, _ = fmt.Fprintf(stdout, "%s: ok\n", report.File)
				continue
			}
			for _, field := range report.Unknown {
				_, _ = fmt.Fprintf(stdout, "%s: %s: warning: unknown_field: %s (ignored unless --strict enforce)\n", report.File, field, field)
			}
			for _, issue := range report.Issues {
				path := issue.Path
				if path == "" {
					path = "(document)"
				}
				_, _ = fmt.Fprintf(stdout, "%s: %s: %s\n", report.File, path, issue.Message)
			}
			if report.OK {
				_, _ = fmt.Fprintf(stdout, "%s: ok (with warnings)\n", report.File)
			}
		}
	}
	if !ok {
		return 1
	}
	return 0
}
