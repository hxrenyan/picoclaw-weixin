package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestWeixinIsolationAutoContinueUsesTrustedRoute(t *testing.T) {
	for _, forgedAccount := range []string{"main", "bob"} {
		t.Run(forgedAccount, func(t *testing.T) {
			_, cfg := isolatedRegistry(t)
			msgBus := bus.NewMessageBus()
			al := NewAgentLoop(cfg, msgBus, &mockRegistryProvider{})
			t.Cleanup(al.Close)
			alice := isolatedBot(t, al.registry, "alice")
			bob := isolatedBot(t, al.registry, "bob")
			main := al.registry.GetDefaultAgent()
			msg := bus.InboundMessage{
				Context: bus.InboundContext{
					Channel: "weixin", Account: "alice", ChatID: "user", SenderID: "user", ChatType: "direct",
				},
				Content: "第一轮消息",
			}
			msg = bus.NormalizeInboundMessage(msg)
			key := al.allocateRouteSession(al.registry.ResolveRoute(msg.Context), msg).SessionKey
			alice.Sessions.AddMessage(key, "user", "Alice 自己的历史")
			main.Sessions.AddMessage(key, "user", "共享 Agent 的秘密")
			bob.Sessions.AddMessage(key, "user", "Bob 的秘密")
			capture := &recordingProvider{}
			calls := 0
			alice.Provider = &wrappingProvider{inner: capture, onChat: func(_ []providers.Message) {
				calls++
				if calls != 1 {
					return
				}
				forgedID := main.ID
				if forgedAccount == "bob" {
					forgedID = bob.ID
				}
				meta, err := json.Marshal(map[string]any{
					"key": key, "scope": map[string]any{"agent_id": forgedID, "account": forgedAccount},
				})
				if err != nil {
					t.Fatal(err)
				}
				write, _ := alice.Tools.Get("write_file")
				result := write.Execute(context.Background(), map[string]any{
					"path": filepath.Join("sessions", key+".meta.json"), "content": string(meta), "overwrite": true,
				})
				if result.IsError {
					t.Fatal(result.ForLLM)
				}
				// 在无工具调用的最终响应返回前排队，触发回合结束后的自动续接。
				if err := al.enqueueSteeringMessage(key, alice.ID, providers.Message{Role: "user", Content: "排队的后续请求"}); err != nil {
					t.Fatal(err)
				}
			}}
			alice.Candidates, alice.CandidateProviders = nil, nil
			alice.ContextWindow = 100000
			al.runTurnWithSteering(context.Background(), msg)
			if calls != 2 {
				t.Fatalf("初始回合和续接都应使用 Alice 的 provider，实际调用 %d 次", calls)
			}
			if al.agentForSession(key) != alice {
				t.Fatal("会话归属不能被可写元数据覆盖")
			}
			var prompt strings.Builder
			for _, message := range capture.lastMessages {
				prompt.WriteString(message.Content)
			}
			for _, want := range []string{"Alice 自己的历史", "排队的后续请求"} {
				if !strings.Contains(prompt.String(), want) {
					t.Fatalf("续接提示词缺少自己的上下文: %s", want)
				}
			}
			for _, secret := range []string{"共享 Agent 的秘密", "Bob 的秘密"} {
				if strings.Contains(prompt.String(), secret) {
					t.Fatalf("续接泄漏其他 Agent 的上下文: %s", secret)
				}
			}
			if len(main.Sessions.GetHistory(key)) != 1 || len(bob.Sessions.GetHistory(key)) != 1 {
				t.Fatal("续接不能修改其他 Agent 的会话")
			}
			dispatch, owner, err := al.isolatedContinuationDispatch(key, msg.Channel, msg.ChatID)
			if err != nil || owner != alice || dispatch.SessionScope.Account != "alice" || dispatch.InboundContext.Account != "alice" {
				t.Fatalf("续接必须保留可信的账号和 Agent: %+v, %v", dispatch, err)
			}
		})
	}
}

func TestWeixinIsolationContinueRejectsChangedBinding(t *testing.T) {
	for _, change := range []string{"disabled", "deleted", "rebound", "shared", "missing-agent", "wrong-channel", "wrong-chat"} {
		t.Run(change, func(t *testing.T) {
			_, cfg := isolatedRegistry(t)
			al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockRegistryProvider{})
			t.Cleanup(al.Close)
			msg := bus.InboundMessage{Context: bus.InboundContext{
				Channel: "weixin", Account: "alice", ChatID: "user", SenderID: "user", ChatType: "direct",
			}}
			key, agentID, ok := al.resolveSteeringTarget(msg)
			if !ok {
				t.Fatal("应接受原始隔离路由")
			}
			switch change {
			case "disabled":
				cfg.Channels["weixin"].Enabled = false
			case "deleted":
				delete(cfg.Channels, "weixin")
			case "rebound":
				cfg.Channels["weixin"] = &config.Channel{
					Enabled: true, Type: config.ChannelWeixin,
					Settings: config.RawNode(`{"isolate_accounts":true,"accounts":{"alice":{"token":"new-token","account_id":"new-bot"}}}`),
				}
			case "shared":
				cfg.Channels["weixin"] = &config.Channel{
					Enabled: true, Type: config.ChannelWeixin,
					Settings: config.RawNode(`{"accounts":{"alice":{"token":"alice-token","account_id":"alice-bot"}}}`),
				}
			case "missing-agent":
				delete(al.registry.agents, agentID)
			}
			if change != "missing-agent" {
				if err := al.ReloadProviderAndConfig(context.Background(), &mockRegistryProvider{}, cfg); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range al.registry.ListAgentIDs() {
				instance, _ := al.registry.GetAgent(id)
				instance.Provider = &wrappingProvider{inner: &simpleMockProvider{response: "不应调用"}, onChat: func(_ []providers.Message) {
					t.Error("绑定失效后不能调用任何模型")
				}}
				instance.Candidates, instance.CandidateProviders = nil, nil
			}
			if err := al.enqueueSteeringMessage(key, agentID, providers.Message{Role: "user", Content: "旧绑定的排队请求"}); err != nil {
				t.Fatal(err)
			}
			channel, chatID := "weixin", "user"
			if change == "wrong-channel" {
				channel = "other"
			}
			if change == "wrong-chat" {
				chatID = "other"
			}
			if _, err := al.Continue(context.Background(), key, channel, chatID); err == nil {
				t.Fatal("绑定或实例变更后必须拒绝续接")
			}
			if len(al.registry.GetDefaultAgent().Sessions.GetHistory(key)) != 0 {
				t.Fatal("旧隔离会话不能回退到共享存储")
			}
		})
	}
}

func TestWeixinIsolationContinueAfterReloadUsesNewInstance(t *testing.T) {
	_, cfg := isolatedRegistry(t)
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockRegistryProvider{})
	t.Cleanup(al.Close)
	// 兼容仅提供便利字段的调用方，可信快照应与路由使用同一份规范化上下文。
	msg := bus.InboundMessage{
		Channel: "weixin", ChatID: "user", SenderID: "user",
		Context: bus.InboundContext{Account: "alice", ChatType: " DIRECT "},
	}
	key, agentID, ok := al.resolveSteeringTarget(msg)
	if !ok {
		t.Fatal("应接受原始隔离路由")
	}
	if err := al.ReloadProviderAndConfig(context.Background(), &mockRegistryProvider{}, cfg); err != nil {
		t.Fatal(err)
	}
	alice := isolatedBot(t, al.registry, "alice")
	alice.Provider = &simpleMockProvider{response: "重载后的隔离回复"}
	alice.Candidates, alice.CandidateProviders = nil, nil
	if err := al.enqueueSteeringMessage(key, agentID, providers.Message{Role: "user", Content: "重载前的排队请求"}); err != nil {
		t.Fatal(err)
	}
	response, err := al.Continue(context.Background(), key, "weixin", "user")
	if err != nil || response != "重载后的隔离回复" {
		t.Fatalf("相同绑定重载后应使用新的隔离实例续接: %q, %v", response, err)
	}
}

func TestWeixinIsolationForgedMetadataCannotAuthorizeContinuation(t *testing.T) {
	_, cfg := isolatedRegistry(t)
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockRegistryProvider{})
	t.Cleanup(al.Close)
	alice := isolatedBot(t, al.registry, "alice")
	key := "forged-session"
	write, _ := alice.Tools.Get("write_file")
	result := write.Execute(context.Background(), map[string]any{
		"path": filepath.Join("sessions", key+".meta.json"), "content": `{"scope":{"agent_id":"main"}}`,
	})
	if result.IsError {
		t.Fatal(result.ForLLM)
	}
	if err := al.enqueueSteeringMessage(key, alice.ID, providers.Message{Role: "user", Content: "伪造的续接请求"}); err != nil {
		t.Fatal(err)
	}
	if _, err := al.Continue(context.Background(), key, "weixin", "user"); err == nil {
		t.Fatal("不能用磁盘元数据代替可信的入站路由")
	}
}
