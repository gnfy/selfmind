package cliapp

import (
	"fmt"
	"io"
	"strings"
	"time"

	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

func (a *App) runAuthCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "auth" {
		return false, 0
	}
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath, CreateIfMissing: true})
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return true, 1
	}
	args := a.args[2:]
	if len(args) == 0 {
		fmt.Fprintln(a.stderr, "usage: selfmind auth [login|status|logout] ...")
		return true, 2
	}
	switch args[0] {
	case "login":
		return true, a.runAuthLogin(cfg, args[1:])
	case "status":
		return true, a.runAuthStatus(cfg, args[1:])
	case "logout":
		return true, a.runAuthLogout(cfg, args[1:])
	default:
		fmt.Fprintln(a.stderr, "usage: selfmind auth [login|status|logout] ...")
		return true, 2
	}
}

func (a *App) runAuthLogin(cfg *config.Config, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.stderr, "usage: selfmind auth login minimax-oauth [--region global|cn] [--no-browser]")
		return 2
	}
	provider := modelruntime.NormalizeProviderID(args[0])
	switch provider {
	case modelruntime.MiniMaxOAuthProvider:
		region := "global"
		openBrowser := true
		for i := 1; i < len(args); i++ {
			switch {
			case args[i] == "--no-browser":
				openBrowser = false
			case args[i] == "--region" && i+1 < len(args):
				region = args[i+1]
				i++
			case strings.HasPrefix(args[i], "--region="):
				region = strings.TrimPrefix(args[i], "--region=")
			default:
				fmt.Fprintf(a.stderr, "unknown option: %s\n", args[i])
				return 2
			}
		}
		status, err := modelruntime.LoginMiniMaxOAuth(a.ctx, cfg.Auth.CredentialsFile, modelruntime.MiniMaxOAuthLoginOptions{
			Region:      region,
			OpenBrowser: openBrowser,
		}, a.stdout)
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		printMiniMaxOAuthStatus(a.stdout, status)
		return 0
	default:
		fmt.Fprintf(a.stderr, "unsupported auth provider: %s\n", args[0])
		return 2
	}
}

func (a *App) runAuthStatus(cfg *config.Config, args []string) int {
	provider := modelruntime.MiniMaxOAuthProvider
	if len(args) > 0 {
		provider = modelruntime.NormalizeProviderID(args[0])
	}
	switch provider {
	case modelruntime.MiniMaxOAuthProvider:
		status, err := modelruntime.MiniMaxOAuthAuthStatus(cfg.Auth.CredentialsFile)
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		printMiniMaxOAuthStatus(a.stdout, status)
		return 0
	default:
		fmt.Fprintf(a.stderr, "unsupported auth provider: %s\n", provider)
		return 2
	}
}

func (a *App) runAuthLogout(cfg *config.Config, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.stderr, "usage: selfmind auth logout minimax-oauth")
		return 2
	}
	provider := modelruntime.NormalizeProviderID(args[0])
	switch provider {
	case modelruntime.MiniMaxOAuthProvider:
		if err := modelruntime.LogoutMiniMaxOAuth(cfg.Auth.CredentialsFile); err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		fmt.Fprintln(a.stdout, "MiniMax OAuth logged out.")
		return 0
	default:
		fmt.Fprintf(a.stderr, "unsupported auth provider: %s\n", provider)
		return 2
	}
}

func printMiniMaxOAuthStatus(out io.Writer, status modelruntime.MiniMaxOAuthStatus) {
	state := "not logged in"
	if status.LoggedIn {
		state = "logged in"
	}
	fmt.Fprintf(out, "MiniMax OAuth: %s\n", state)
	if status.Region != "" {
		fmt.Fprintf(out, "region: %s\n", status.Region)
	}
	if status.InferenceBaseURL != "" {
		fmt.Fprintf(out, "base_url: %s\n", status.InferenceBaseURL)
	}
	if !status.ExpiresAt.IsZero() {
		fmt.Fprintf(out, "expires_at: %s\n", status.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if status.LastAuthError != "" {
		fmt.Fprintf(out, "last_error: %s\n", status.LastAuthError)
	}
	if status.CredentialFilePath != "" {
		fmt.Fprintf(out, "credentials: %s\n", status.CredentialFilePath)
	}
}
