package weixin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

func TestNewWeixinChannelFromConfigUsesOneLoopPerBot(t *testing.T) {
	settings := &config.WeixinSettings{}
	settings.SetToken("token-default")
	settings.AccountID = "bot-default"
	settings.Accounts = map[string]config.WeixinAccountSettings{
		"shop": {
			Name:      "门店号",
			Token:     *config.NewSecureString("token-shop"),
			AccountID: "bot-shop",
		},
	}
	bc := &config.Channel{Type: config.ChannelWeixin, Enabled: true}
	bc.SetName("weixin")

	ch, err := newWeixinChannelFromConfig("weixin", bc, settings, nil)
	if err != nil {
		t.Fatalf("newWeixinChannelFromConfig() error = %v", err)
	}
	multi, ok := ch.(*multiWeixinChannel)
	if !ok {
		t.Fatalf("channel type = %T, want *multiWeixinChannel", ch)
	}
	if len(multi.bots) != 2 {
		t.Fatalf("bots = %d, want 2", len(multi.bots))
	}
	if multi.bots["shop"] == nil || multi.bots["shop"].accountKey != "shop" {
		t.Fatal("shop bot was not registered")
	}
	if multi.bots["default"] == nil || multi.bots["default"].accountKey != DefaultAccountKey {
		t.Fatal("default bot was not registered")
	}
}

func TestSingleWeixinBotStaysOneChannel(t *testing.T) {
	settings := &config.WeixinSettings{}
	settings.SetToken("token-default")
	bc := &config.Channel{Type: config.ChannelWeixin, Enabled: true}
	bc.SetName("weixin")

	ch, err := newWeixinChannelFromConfig("weixin", bc, settings, nil)
	if err != nil {
		t.Fatalf("newWeixinChannelFromConfig() error = %v", err)
	}
	single, ok := ch.(*WeixinChannel)
	if !ok {
		t.Fatalf("channel type = %T, want *WeixinChannel", ch)
	}
	if single.accountKey != DefaultAccountKey {
		t.Fatalf("accountKey = %q, want %q", single.accountKey, DefaultAccountKey)
	}
}

func TestMultiWeixinSendUsesTheBotThatReceivedTheMessage(t *testing.T) {
	seen := make(chan string, 2)
	newBot := func(token string) *WeixinChannel {
		bot := &WeixinChannel{
			BaseChannel: channels.NewBaseChannel("weixin", nil, nil, nil),
			api: &ApiClient{
				BaseURL: "https://ilinkai.weixin.qq.com/",
				Token:   token,
				HttpClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					seen <- r.Header.Get("Authorization")
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"ret":0,"errcode":0}`)),
						Header:     make(http.Header),
					}, nil
				})},
			},
			config:      &config.WeixinSettings{},
			typingCache: make(map[string]typingTicketCacheEntry),
		}
		bot.SetRunning(true)
		bot.contextTokens.Store("user-1", "ctx-token")
		return bot
	}
	multi := &multiWeixinChannel{
		bots: map[string]*WeixinChannel{
			DefaultAccountKey: newBot("token-default"),
			"shop":            newBot("token-shop"),
		},
		order: []string{DefaultAccountKey, "shop"},
	}

	_, err := multi.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "user-1",
		Content: "hello shop",
		Context: bus.InboundContext{Account: "shop", ChatID: "user-1"},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if got := <-seen; got != "Bearer token-shop" {
		t.Fatalf("Authorization = %q, want shop token", got)
	}
}
