package weixin

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

func TestListAccountsKeepsLegacyBotAsDefault(t *testing.T) {
	settings := &config.WeixinSettings{}
	settings.SetToken("token-default")
	settings.AccountID = "bot-default"
	settings.Proxy = "http://127.0.0.1:7890"

	specs, err := ListAccounts(settings)
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("len(specs) = %d, want 1", len(specs))
	}
	if specs[0].Key != DefaultAccountKey {
		t.Fatalf("key = %q, want %q", specs[0].Key, DefaultAccountKey)
	}
	if specs[0].Settings.Token.String() != "token-default" {
		t.Fatalf("token = %q", specs[0].Settings.Token.String())
	}
	if specs[0].Settings.Proxy != "http://127.0.0.1:7890" {
		t.Fatalf("proxy = %q", specs[0].Settings.Proxy)
	}
}

func TestRuntimeAccountsStartsOnlyLinkedBots(t *testing.T) {
	settings := &config.WeixinSettings{Proxy: "http://127.0.0.1:7890"}
	settings.SetToken("token-default")
	settings.AccountID = "bot-default"
	if err := UpsertCredentials(settings, "shop", "门店号", "token-shop", "user-shop", "bot-shop", "", ""); err != nil {
		t.Fatalf("UpsertCredentials() error = %v", err)
	}
	enabled := false
	settings.Accounts["paused"] = config.WeixinAccountSettings{
		Name:      "暂停",
		Enabled:   &enabled,
		Token:     *config.NewSecureString("token-paused"),
		AccountID: "bot-paused",
	}

	specs, err := RuntimeAccounts(settings)
	if err != nil {
		t.Fatalf("RuntimeAccounts() error = %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("len(specs) = %d, want 2", len(specs))
	}
	if specs[0].Key != DefaultAccountKey || specs[1].Key != "shop" {
		t.Fatalf("keys = %q, %q", specs[0].Key, specs[1].Key)
	}
	if specs[1].Name != "门店号" {
		t.Fatalf("shop name = %q", specs[1].Name)
	}
	if specs[1].Settings.Proxy != "http://127.0.0.1:7890" {
		t.Fatalf("shop proxy = %q, want inherited proxy", specs[1].Settings.Proxy)
	}
	if specs[1].Settings.Token.String() != "token-shop" {
		t.Fatalf("shop token = %q", specs[1].Settings.Token.String())
	}
}

func TestUpsertCredentialsRejectsDuplicateBot(t *testing.T) {
	settings := &config.WeixinSettings{}
	settings.SetToken("token-default")
	settings.AccountID = "bot-default"
	err := UpsertCredentials(settings, "shop", "门店号", "token-shop", "user-shop", "bot-default", "", "")
	if err == nil {
		t.Fatal("duplicate bot id was accepted")
	}
	err = UpsertCredentials(settings, "shop", "门店号", "token-default", "user-shop", "bot-shop", "", "")
	if err == nil {
		t.Fatal("duplicate token was accepted")
	}
}

func TestEnsureAccountAssignsNextKey(t *testing.T) {
	settings := &config.WeixinSettings{}
	first, err := EnsureAccount(settings, "门店号")
	if err != nil {
		t.Fatalf("EnsureAccount() error = %v", err)
	}
	if first != "bot-2" {
		t.Fatalf("first key = %q, want bot-2", first)
	}
	second, err := EnsureAccount(settings, "客服号")
	if err != nil {
		t.Fatalf("EnsureAccount() second error = %v", err)
	}
	if second != "bot-3" {
		t.Fatalf("second key = %q, want bot-3", second)
	}
	if _, err := EnsureAccount(settings, ""); err == nil {
		t.Fatal("empty name was accepted")
	}
	if _, err := canonicalAccountKey("门店"); err == nil {
		t.Fatal("non-ascii account key was accepted")
	}
}

func TestEnsureAccountStopsAtSharedPollerCap(t *testing.T) {
	settings := &config.WeixinSettings{}
	for i := 0; i < MaxAccounts-1; i++ {
		if _, err := EnsureAccount(settings, fmt.Sprintf("bot %d", i)); err != nil {
			t.Fatalf("account %d: %v", i, err)
		}
	}
	if _, err := EnsureAccount(settings, "one more"); err == nil {
		t.Fatal("expected the account cap to reject another bot")
	}
}

func TestRemoveAccountClearsDefaultWithoutDeletingSlot(t *testing.T) {
	settings := &config.WeixinSettings{}
	settings.SetToken("token-default")
	settings.AccountID = "bot-default"
	if _, err := EnsureAccount(settings, "门店号"); err != nil {
		t.Fatalf("EnsureAccount() error = %v", err)
	}
	if err := UpsertCredentials(settings, "bot-2", "", "token-shop", "user-shop", "bot-shop", "", ""); err != nil {
		t.Fatalf("UpsertCredentials() error = %v", err)
	}
	if err := RemoveAccount(settings, "bot-2"); err != nil {
		t.Fatalf("RemoveAccount(bot-2) error = %v", err)
	}
	if _, ok := settings.Accounts["bot-2"]; ok {
		t.Fatal("bot-2 still exists")
	}
	if err := RemoveAccount(settings, DefaultAccountKey); err != nil {
		t.Fatalf("RemoveAccount(default) error = %v", err)
	}
	specs, err := ListAccounts(settings)
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(specs) != 1 || specs[0].Key != DefaultAccountKey {
		t.Fatalf("specs = %+v, want only default", specs)
	}
	if strings.TrimSpace(specs[0].Settings.Token.String()) != "" || specs[0].Settings.AccountID != "" {
		t.Fatalf("default credentials were not cleared: token %q id %q", specs[0].Settings.Token.String(), specs[0].Settings.AccountID)
	}
}

func TestWeixinAccountTokensRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.DefaultConfig()
	_, settings, err := EnsureWeixinChannel(cfg, config.ChannelWeixin)
	if err != nil {
		t.Fatalf("EnsureWeixinChannel() error = %v", err)
	}
	if err := UpsertCredentials(settings, DefaultAccountKey, "默认", "token-default", "user-default", "bot-default", "", "http://127.0.0.1:7890"); err != nil {
		t.Fatalf("upsert default: %v", err)
	}
	if err := UpsertCredentials(settings, "shop", "门店号", "token-shop", "user-shop", "bot-shop", "https://region.ilinkai.weixin.qq.com/", ""); err != nil {
		t.Fatalf("upsert shop: %v", err)
	}
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	decoded, err := loaded.Channels[config.ChannelWeixin].GetDecoded()
	if err != nil {
		t.Fatalf("GetDecoded() error = %v", err)
	}
	wx := decoded.(*config.WeixinSettings)
	specs, err := ListAccounts(wx)
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("len(specs) = %d, want 2", len(specs))
	}
	if specs[0].Settings.Token.String() != "token-default" || specs[0].Settings.AccountID != "bot-default" {
		t.Fatalf("default = token %q id %q", specs[0].Settings.Token.String(), specs[0].Settings.AccountID)
	}
	if specs[1].Key != "shop" || specs[1].Name != "门店号" || specs[1].Settings.Token.String() != "token-shop" {
		t.Fatalf("shop = key %q name %q token %q", specs[1].Key, specs[1].Name, specs[1].Settings.Token.String())
	}
	if specs[1].Settings.BaseURL != "https://region.ilinkai.weixin.qq.com/" {
		t.Fatalf("shop base url = %q", specs[1].Settings.BaseURL)
	}
}
