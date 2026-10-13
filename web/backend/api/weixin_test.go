package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

func TestSaveWeixinBindingReturnsSuccessWhenRestartFails(t *testing.T) {
	resetGatewayTestState(t)

	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.DefaultConfig()
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	originalHealthGet := gatewayHealthGet
	gatewayHealthGet = func(url string, timeout time.Duration) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"status":"ok","uptime":"1s","pid":` + strconv.Itoa(os.Getpid()) + `}`,
			)),
		}, nil
	}
	t.Cleanup(func() {
		gatewayHealthGet = originalHealthGet
	})

	h := NewHandler(configPath)
	if err := h.saveWeixinBinding("default", "bot-token", "bot-account", "user-1", ""); err != nil {
		t.Fatalf("saveWeixinBinding() error = %v, want nil after config save succeeds", err)
	}

	savedCfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	bc := savedCfg.Channels["weixin"]
	decoded, err := bc.GetDecoded()
	if err != nil {
		t.Fatalf("GetDecoded() error = %v", err)
	}
	wxCfg := decoded.(*config.WeixinSettings)
	if got := wxCfg.Token.String(); got != "bot-token" {
		t.Fatalf("Weixin.Token() = %q, want %q", got, "bot-token")
	}
	if got := wxCfg.AccountID; got != "bot-account" {
		t.Fatalf("Weixin.AccountID = %q, want %q", got, "bot-account")
	}
	if !bc.Enabled {
		t.Fatalf("Weixin.Enabled = false, want true")
	}
	if got := wxCfg.UserID; got != "user-1" {
		t.Fatalf("Weixin.UserID = %q, want user-1", got)
	}
}

func TestSaveWeixinBindingKeepsBotsSeparate(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.DefaultConfig()
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	h := NewHandler(configPath)
	if err := h.saveWeixinBinding("default", "token-default", "bot-default", "user-default", ""); err != nil {
		t.Fatalf("save default bot: %v", err)
	}
	if err := h.saveWeixinBinding("shop", "token-shop", "bot-shop", "user-shop", "https://region.ilinkai.weixin.qq.com/"); err != nil {
		t.Fatalf("save shop bot: %v", err)
	}
	if err := h.saveWeixinBinding("other", "token-other", "bot-default", "user-other", ""); err == nil {
		t.Fatal("duplicate bot id was accepted")
	}

	savedCfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	decoded, err := savedCfg.Channels["weixin"].GetDecoded()
	if err != nil {
		t.Fatalf("GetDecoded() error = %v", err)
	}
	wxCfg := decoded.(*config.WeixinSettings)
	if wxCfg.Token.String() != "token-default" || wxCfg.AccountID != "bot-default" {
		t.Fatalf("default bot = token %q id %q", wxCfg.Token.String(), wxCfg.AccountID)
	}
	shop := wxCfg.Accounts["shop"]
	if shop.Token.String() != "token-shop" || shop.AccountID != "bot-shop" {
		t.Fatalf("shop bot = token %q id %q", shop.Token.String(), shop.AccountID)
	}
	if shop.BaseURL != "https://region.ilinkai.weixin.qq.com/" {
		t.Fatalf("shop base url = %q", shop.BaseURL)
	}
}
