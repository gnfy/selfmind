package cliapp

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"selfmind/internal/gateway/weixin"
	"selfmind/internal/platform/config"
)

func (a *App) runWeixinCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "weixin" {
		return false, 0
	}
	args := a.args[2:]
	action := "status"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		action = args[0]
		args = args[1:]
	}
	switch action {
	case "login":
		return true, a.weixinLogin(args)
	case "status":
		return true, a.weixinStatus(args)
	default:
		fmt.Fprintf(a.stderr, "unknown weixin command: %s\n", action)
		fmt.Fprintln(a.stderr, "usage: selfmind weixin [login|status]")
		return true, 2
	}
}

func (a *App) weixinLogin(args []string) int {
	fs := flag.NewFlagSet("selfmind weixin login", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	timeout := fs.Duration("timeout", 8*time.Minute, "QR login timeout")
	ownerPersonID := fs.String("owner-person-id", "", "bind WeChat account to an existing person id")
	disableGateway := fs.Bool("no-enable", false, "save credentials but do not enable gateway.weixin")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath, CreateIfMissing: true})
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	cred, err := weixin.QRLogin(a.ctx, "", a.stdout, *timeout)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	cfg.Gateway.Weixin.Enabled = !*disableGateway
	cfg.Gateway.Weixin.AccountID = cred.AccountID
	cfg.Gateway.Weixin.Token = cred.Token
	cfg.Gateway.Weixin.BaseURL = firstNonEmpty(cred.BaseURL, weixin.DefaultBaseURL)
	cfg.Gateway.Weixin.CDNBaseURL = firstNonEmpty(cfg.Gateway.Weixin.CDNBaseURL, weixin.DefaultCDNBaseURL)
	cfg.Gateway.Weixin.DMPolicy = firstNonEmpty(cfg.Gateway.Weixin.DMPolicy, "open")
	cfg.Gateway.Weixin.GroupPolicy = firstNonEmpty(cfg.Gateway.Weixin.GroupPolicy, "disabled")
	if strings.TrimSpace(*ownerPersonID) != "" {
		cfg.Gateway.Weixin.OwnerPersonID = strings.TrimSpace(*ownerPersonID)
	}
	if err := config.SaveConfig(cfg.Path, cfg); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Saved Weixin credentials to config: %s\n", cfg.Path)
	fmt.Fprintf(a.stdout, "Credential file: %s\n", weixin.AccountFilePath("", cred.AccountID))
	if cfg.Gateway.Weixin.Enabled {
		fmt.Fprintln(a.stdout, "Start or restart the gateway to receive WeChat messages: selfmind gateway restart")
	}
	return 0
}

func (a *App) weixinStatus(args []string) int {
	fs := flag.NewFlagSet("selfmind weixin status", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath, CreateIfMissing: true})
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	wx := cfg.Gateway.Weixin
	accountFile := ""
	accountFileExists := false
	syncBufFile := ""
	syncBufAge := ""
	if strings.TrimSpace(wx.AccountID) != "" {
		accountFile = weixin.AccountFilePath("", wx.AccountID)
		if _, err := os.Stat(accountFile); err == nil {
			accountFileExists = true
		}
		syncBufFile = weixin.SyncBufFilePath("", wx.AccountID)
		if info, err := os.Stat(syncBufFile); err == nil {
			syncBufAge = time.Since(info.ModTime()).Round(time.Second).String()
		}
	}
	fmt.Fprintln(a.stdout, "SelfMind Weixin")
	fmt.Fprintf(a.stdout, "enabled: %t\n", wx.Enabled)
	fmt.Fprintf(a.stdout, "account_id: %s\n", blankAsDash(maskConfigID(wx.AccountID)))
	fmt.Fprintf(a.stdout, "token: %s\n", configuredMark(wx.Token))
	fmt.Fprintf(a.stdout, "base_url: %s\n", blankAsDash(wx.BaseURL))
	fmt.Fprintf(a.stdout, "cdn_base_url: %s\n", blankAsDash(wx.CDNBaseURL))
	fmt.Fprintf(a.stdout, "owner_person_id: %s\n", blankAsDash(wx.OwnerPersonID))
	fmt.Fprintf(a.stdout, "dm_policy: %s\n", blankAsDash(wx.DMPolicy))
	fmt.Fprintf(a.stdout, "group_policy: %s\n", blankAsDash(wx.GroupPolicy))
	if accountFile != "" {
		fmt.Fprintf(a.stdout, "credential_file: %s (%s)\n", accountFile, existsMark(accountFileExists))
	}
	if syncBufFile != "" {
		fmt.Fprintf(a.stdout, "sync_state: %s", syncBufFile)
		if syncBufAge != "" {
			fmt.Fprintf(a.stdout, " (updated %s ago)", syncBufAge)
		}
		fmt.Fprintln(a.stdout)
	}
	fmt.Fprintf(a.stdout, "config: %s\n", cfg.Path)
	return 0
}

func maskConfigID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 10 {
		return value
	}
	return value[:6] + "..." + value[len(value)-4:]
}

func existsMark(ok bool) string {
	if ok {
		return "exists"
	}
	return "missing"
}
