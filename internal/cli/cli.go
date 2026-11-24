// Package cli implements the changefeed command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yousafkhamza/changefeed/internal/auth"
	"github.com/yousafkhamza/changefeed/internal/config"
	"github.com/yousafkhamza/changefeed/internal/version"
)

const usage = `changefeed — one timeline of every production change

Usage:
  changefeed serve    [--config FILE] [--demo]        run the server (API, UI, collectors)
  changefeed emit     [flags] "summary"               send a change event
  changefeed since    DURATION [flags]                list recent changes, e.g. "since 1h"
  changefeed suspects --service NAME [flags]          rank likely causes of an incident
  changefeed token                                    create an API token and its config hash
  changefeed check-config FILE                        validate a server config without starting
  changefeed mcp      [--endpoint URL]                MCP server on stdio for AI assistants (read-only)
  changefeed version

Client commands read CHANGEFEED_ENDPOINT and CHANGEFEED_TOKEN from the environment.
Run "changefeed <command> -h" for the flags of a command.
`

// Run executes the CLI and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "serve":
		err = serve(rest, stderr)
	case "emit":
		err = emit(rest, stdout)
	case "since":
		err = since(rest, stdout)
	case "suspects":
		err = suspects(rest, stdout)
	case "token":
		err = token(stdout)
	case "check-config":
		err = checkConfig(rest, stdout)
	case "mcp":
		err = mcpServe(rest, os.Stdin, stdout)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "changefeed %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		var ue usageError
		if errors.As(err, &ue) {
			return 2
		}
		return 1
	}
	return 0
}

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// clientFlags are shared by the commands that talk to a server.
type clientFlags struct {
	endpoint, token string
	json            bool
}

func (c *clientFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.endpoint, "endpoint", os.Getenv("CHANGEFEED_ENDPOINT"), "server URL (env CHANGEFEED_ENDPOINT)")
	fs.StringVar(&c.token, "token", "", "API token (default: env CHANGEFEED_TOKEN; prefer the env var so it stays out of shell history)")
}

func (c *clientFlags) resolve() error {
	if c.token == "" {
		c.token = os.Getenv("CHANGEFEED_TOKEN")
	}
	c.endpoint = strings.TrimRight(c.endpoint, "/")
	if c.endpoint == "" {
		return usageError{"set --endpoint or CHANGEFEED_ENDPOINT, e.g. https://changefeed.example.com"}
	}
	if !strings.HasPrefix(c.endpoint, "https://") && !strings.HasPrefix(c.endpoint, "http://") {
		return usageError{"endpoint must start with https:// (or http:// for local testing)"}
	}
	return nil
}

func checkConfig(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return usageError{"usage: changefeed check-config FILE"}
	}
	cfg, err := config.Load(args[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s is valid: %d token(s), GitHub OIDC %s, Kubernetes collector %s, %d service dependency map(s)\n",
		args[0], len(cfg.Auth.Tokens), onOff(cfg.Auth.GitHub.Enabled), onOff(cfg.Collectors.Kubernetes.Enabled), len(cfg.Services))
	return nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func token(stdout io.Writer) error {
	tok, hash, err := auth.NewToken()
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, `Token (shown once, store it in your secret manager):
  %s

Add this to the server config (only the hash is stored):
  auth:
    tokens:
      - name: CHANGE-ME          # e.g. argocd, aws-eventbridge, runbooks
        sha256: %s
        scopes: [write]          # read, write
        sources: []              # limit to lanes, e.g. [argocd]
`, tok, hash)
	return nil
}
