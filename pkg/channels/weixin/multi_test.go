package weixin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/routing"
)

func TestWeixinIsolationInboundRetainsBindingIdentity(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		name := "单 Bot"
		if multiple {
			name = "多 Bot"
		}
		t.Run(name, func(t *testing.T) {
			settings := &config.WeixinSettings{IsolateAccounts: true, AccountID: "bot-default"}
			settings.SetToken("token-default")
			if multiple {
				settings.Accounts = map[string]config.WeixinAccountSettings{
					"shop": {Token: *config.NewSecureString("token-shop"), AccountID: "bot-shop"},
				}
			}
			bc := &config.Channel{Type: config.ChannelWeixin, Enabled: true, AllowFrom: []string{"*"}}
			if err := bc.Decode(settings); err != nil {
				t.Fatal(err)
			}
			const channel = "weixin-business"
			cfg := config.DefaultConfig()
			cfg.Channels = config.ChannelsConfig{channel: bc}
			decoded, err := bc.GetDecoded()
			if err != nil {
				t.Fatal(err)
			}
			current := decoded.(*config.WeixinSettings)
			messageBus := bus.NewMessageBus()
			t.Cleanup(messageBus.Close)
			ch, err := newWeixinChannelFromConfig(channel, bc, current, messageBus)
			if err != nil {
				t.Fatal(err)
			}
			var bots []*WeixinChannel
			switch runtime := ch.(type) {
			case *WeixinChannel:
				bots = append(bots, runtime)
			case *multiWeixinChannel:
				for _, bot := range runtime.bots {
					bots = append(bots, bot)
				}
			}
			resolver := routing.NewRouteResolver(cfg)
			var received []bus.InboundContext
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			for _, bot := range bots {
				bot.handleInboundMessage(ctx, WeixinMessage{
					FromUserID: "user-1", ClientID: "message-1",
					ItemList: []MessageItem{{Type: MessageItemTypeText, TextItem: &TextItem{Text: "我的私有消息"}}},
				})
				select {
				case msg := <-messageBus.InboundChan():
					if msg.Context.Raw["weixin_isolated"] != "true" {
						t.Fatal("运行时 Bot 丢失隔离标记")
					}
					want, _ := cfg.ResolveWeixinIsolation(channel, bot.accountKey)
					route := resolver.ResolveRoute(msg.Context)
					if want == nil || !route.Isolated || route.AgentID != want.ID || msg.Context.Raw["weixin_bot_identity"] != want.ID {
						t.Fatalf("接收时身份与隔离路由不一致: route=%+v, want=%+v, channel=%s, identity=%s", route, want, bot.Name(), msg.Context.Raw["weixin_bot_identity"])
					}
					received = append(received, msg.Context)
				case <-ctx.Done():
					t.Fatal("未收到入站消息")
				}
			}
			current.IsolateAccounts = false
			for _, inbound := range received {
				if route := resolver.ResolveRoute(inbound); !route.Isolated || route.AgentID != "" {
					t.Fatal("关闭隔离后，已排队的隔离消息不能进入共享 Agent")
				}
			}
		})
	}
}

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

func TestBotForOutboundUsesScopeAccountBeforeDefaultFallback(t *testing.T) {
	defaultBot := &WeixinChannel{}
	shopBot := &WeixinChannel{}
	multi := &multiWeixinChannel{bots: map[string]*WeixinChannel{
		DefaultAccountKey: defaultBot,
		"shop":            shopBot,
	}}
	if got := multi.botForOutbound("", &bus.OutboundScope{Account: "shop"}); got != shopBot {
		t.Fatal("scope account did not select shop bot")
	}
	if got := multi.botForOutbound("missing", &bus.OutboundScope{Account: "shop"}); got != shopBot {
		t.Fatal("scope account did not recover from an unknown context account")
	}
	if got := multi.botForOutbound("", nil); got != defaultBot {
		t.Fatal("missing account did not fall back to default bot")
	}
	if got := multi.botForOutbound("missing", nil); got != nil {
		t.Fatal("unknown context account fell back to another bot")
	}
	if got := multi.botForOutbound("", &bus.OutboundScope{Account: "missing"}); got != nil {
		t.Fatal("unknown scope account fell back to another bot")
	}
}
