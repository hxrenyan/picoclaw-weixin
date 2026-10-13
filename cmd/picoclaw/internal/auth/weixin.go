package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sipeed/picoclaw/cmd/picoclaw/internal"
	"github.com/sipeed/picoclaw/pkg/channels/weixin"
	"github.com/sipeed/picoclaw/pkg/config"
)

func newWeixinCommand() *cobra.Command {
	var baseURL string
	var proxy string
	var timeout int
	var account string
	var name string
	var channelName string

	cmd := &cobra.Command{
		Use:   "weixin",
		Short: "Connect a WeChat bot via QR code",
		Long: `Start the interactive Weixin (WeChat personal) QR code login flow.

A QR code is displayed in the terminal. Scan it from WeChat with
Settings → Plugins → WeChat ClawBot → Scan.
On success, the bot token is saved to the picoclaw config.

Repeat the command with a new --account key to link another WeChat bot.
Each bot polls independently and keeps its own conversation.

Example:
  picoclaw auth weixin
  picoclaw auth weixin --account shop --name 门店号
  picoclaw auth weixin list`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWeixinOnboard(
				baseURL,
				proxy,
				account,
				name,
				channelName,
				time.Duration(timeout)*time.Second,
			)
		},
	}

	cmd.Flags().StringVar(&baseURL, "base-url", "https://ilinkai.weixin.qq.com/", "iLink API base URL")
	cmd.Flags().StringVar(&proxy, "proxy", "", "HTTP proxy URL (e.g. http://localhost:7890)")
	cmd.Flags().IntVar(&timeout, "timeout", 300, "Login timeout in seconds")
	cmd.Flags().StringVar(&account, "account", "", "Bot key to create or update. Empty updates the default bot")
	cmd.Flags().StringVar(&name, "name", "", "Display name for this bot")
	cmd.Flags().StringVar(&channelName, "channel", config.ChannelWeixin, "channel_list key that owns these bots")
	cmd.AddCommand(newWeixinListCommand())

	return cmd
}

func newWeixinListCommand() *cobra.Command {
	var channelName string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List WeChat bots configured for the weixin channel",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWeixinList(channelName)
		},
	}
	cmd.Flags().StringVar(&channelName, "channel", config.ChannelWeixin, "channel_list key")
	return cmd
}

func runWeixinList(channelName string) error {
	cfg, err := config.LoadConfig(internal.GetConfigPath())
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	bc := cfg.Channels.Get(channelName)
	if bc == nil {
		fmt.Printf("No %q channel configured.\n", channelName)
		return nil
	}
	settings, err := bc.GetDecoded()
	if err != nil {
		return err
	}
	weixinCfg, ok := settings.(*config.WeixinSettings)
	if !ok || weixinCfg == nil {
		return fmt.Errorf("channel %q is not a weixin channel", channelName)
	}
	specs, err := weixin.ListAccounts(weixinCfg)
	if err != nil {
		return err
	}
	fmt.Printf("WeChat bots on channel %q (%d/%d):\n", channelName, len(specs), weixin.MaxAccounts)
	for _, spec := range specs {
		state := "not linked"
		if strings.TrimSpace(spec.Settings.Token.String()) != "" {
			state = "linked"
		}
		if spec.Disabled {
			state = "disabled"
		}
		label := spec.Name
		if label == "" {
			label = spec.Key
		}
		fmt.Printf("  %-12s  %-10s  %s", spec.Key, state, label)
		if spec.Settings.AccountID != "" {
			fmt.Printf("  bot=%s", spec.Settings.AccountID)
		}
		fmt.Println()
	}
	return nil
}

func runWeixinOnboard(baseURL, proxy, account, name, channelName string, timeout time.Duration) error {
	fmt.Println("Starting Weixin (WeChat personal) login...")
	fmt.Println()

	botToken, userID, accountID, returnedBaseURL, err := weixin.PerformLoginInteractive(
		context.Background(),
		weixin.AuthFlowOpts{
			BaseURL: baseURL,
			Timeout: timeout,
			Proxy:   proxy,
		},
	)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	fmt.Println()
	fmt.Println("✅ Login successful!")
	fmt.Printf("   Account ID : %s\n", accountID)
	if userID != "" {
		fmt.Printf("   User ID    : %s\n", userID)
	}
	fmt.Println()

	// Prefer the server-returned base URL (may be region-specific)
	effectiveBaseURL := returnedBaseURL
	if effectiveBaseURL == "" {
		effectiveBaseURL = baseURL
	}

	target := account
	if strings.TrimSpace(target) == "" {
		target = weixin.DefaultAccountKey
	}
	if err := saveWeixinConfig(channelName, target, name, botToken, userID, accountID, effectiveBaseURL, proxy); err != nil {
		fmt.Printf("⚠️  Could not auto-save to config: %v\n", err)
		printManualWeixinConfig(botToken, effectiveBaseURL)
		return nil
	}

	fmt.Printf("✓ Bot %q saved on channel %q. Start the gateway with:\n", target, channelName)
	fmt.Println()
	fmt.Println("  picoclaw gateway")
	fmt.Println()
	fmt.Println("Link another WeChat bot with:")
	fmt.Println()
	fmt.Println("  picoclaw auth weixin --account shop --name 门店号")
	fmt.Println()
	fmt.Println("To restrict which WeChat users can send messages, add their user IDs")
	fmt.Println("to channels.weixin.allow_from in your config.")

	return nil
}

// saveWeixinConfig stores a bot token on the default WeChat bot or a named account.
func saveWeixinConfig(channelName, accountKey, displayName, token, userID, botID, baseURL, proxy string) error {
	cfgPath := internal.GetConfigPath()

	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	_, settings, err := weixin.EnsureWeixinChannel(cfg, channelName)
	if err != nil {
		return err
	}
	if err := weixin.UpsertCredentials(settings, accountKey, displayName, token, userID, botID, baseURL, proxy); err != nil {
		return err
	}
	return config.SaveConfig(cfgPath, cfg)
}

func printManualWeixinConfig(token, baseURL string) {
	fmt.Println()
	fmt.Println("Add the following to the channels section of your picoclaw config:")
	fmt.Println()
	fmt.Println(`  "weixin": {`)
	fmt.Println(`    "enabled": true,`)
	fmt.Printf("    \"token\": %q,\n", token)
	const defaultBase = "https://ilinkai.weixin.qq.com/"
	if baseURL != "" && baseURL != defaultBase {
		fmt.Printf("    \"base_url\": %q,\n", baseURL)
	}
	fmt.Println(`    "allow_from": []`)
	fmt.Println(`  }`)
}
