package routing

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

func TestWeixinIsolationTakesPrecedenceOverSharedDispatch(t *testing.T) {
	cfg := testConfig([]config.AgentConfig{{ID: "main", Default: true}})
	cfg.Channels = config.ChannelsConfig{"weixin": {
		Enabled: true, Type: config.ChannelWeixin,
		Settings: config.RawNode(`{"isolate_accounts":true,"token":"token","account_id":"bot"}`),
	}}
	cfg.Agents.Dispatch = &config.DispatchConfig{Rules: []config.DispatchRule{{
		Agent: "main", When: config.DispatchSelector{Channel: "weixin"},
	}}}
	cfg.Session.IdentityLinks = map[string][]string{"shared": {"alice", "bob"}}
	r := NewRouteResolver(cfg)
	route := r.ResolveRoute(bus.InboundContext{Channel: "weixin", ChatID: "alice", SenderID: "alice"})
	if !route.Isolated || route.AgentID == "main" || route.AgentID == "" || route.SessionPolicy.IdentityLinks != nil {
		t.Fatalf("隔离路由不能进入共享 Agent 或身份映射: %+v", route)
	}
	missing := r.ResolveRoute(bus.InboundContext{Channel: "weixin", Account: "missing"})
	if !missing.Isolated || missing.AgentID != "" {
		t.Fatalf("未知账号必须保持隔离并拒绝默认回退: %+v", missing)
	}
	other := r.ResolveRoute(bus.InboundContext{Channel: "telegram", SenderID: "alice"})
	if other.Isolated || other.AgentID != "main" {
		t.Fatalf("其他渠道路由受到影响: %+v", other)
	}
	stale := bus.InboundContext{Channel: "weixin", Raw: map[string]string{
		"weixin_bot_identity": "previous-bot-identity", "weixin_isolated": "true",
	}}
	if old := r.ResolveRoute(stale); !old.Isolated || old.AgentID != "" {
		t.Fatal("换绑前排队的旧消息不能进入新用户的 Agent")
	}
	delete(cfg.Channels, "weixin")
	if removed := r.ResolveRoute(stale); !removed.Isolated || removed.AgentID != "" {
		t.Fatal("渠道删除后，旧的隔离消息不能进入共享 Agent")
	}
}

func TestWeixinIsolationRoutesOriginalChannelKey(t *testing.T) {
	cfg := testConfig(nil)
	cfg.Channels = config.ChannelsConfig{}
	for _, channel := range []string{"WeixinBusiness", "weixinbusiness"} {
		cfg.Channels[channel] = &config.Channel{
			Enabled: true, Type: config.ChannelWeixin,
			Settings: config.RawNode(`{"isolate_accounts":true,"token":"token","account_id":"bot"}`),
		}
	}
	r := NewRouteResolver(cfg)
	for _, channel := range []string{"WeixinBusiness", "weixinbusiness"} {
		agent, _ := cfg.ResolveWeixinIsolation(channel, "default")
		route := r.ResolveRoute(bus.InboundContext{Channel: channel, Raw: map[string]string{
			"weixin_isolated": "true", "weixin_bot_identity": config.WeixinIsolationAgentID(channel, "default", "bot", "", "token"),
		}})
		if !route.Isolated || route.AgentID != agent.ID {
			t.Fatalf("必须按原始渠道键路由: %+v", route)
		}
	}
}
