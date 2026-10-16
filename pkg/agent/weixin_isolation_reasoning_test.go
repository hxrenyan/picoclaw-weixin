package agent

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestWeixinIsolationDisablesSharedReasoningForwarding(t *testing.T) {
	for _, target := range []struct {
		account string
		chatID  string
	}{
		{account: "alice", chatID: "shared-reasoning-chat"},
		{account: "bob", chatID: "shared-reasoning-chat"},
		{account: "shared", chatID: "shared-reasoning-chat"},
		{account: "shared"},
	} {
		for _, reasoningField := range []string{"Reasoning", "ReasoningContent", "无推理"} {
			for _, withTool := range []bool{false, true} {
				name := target.account + "/" + reasoningField + "/普通回答"
				if withTool {
					name = target.account + "/" + reasoningField + "/工具调用"
				}
				if target.chatID == "" {
					name += "/无目标会话"
				}
				t.Run(name, func(t *testing.T) {
					_, cfg := isolatedRegistry(t)
					cfg.Channels["weixin"].ReasoningChannelID = target.chatID
					msgBus := bus.NewMessageBus()
					al := NewAgentLoop(cfg, msgBus, &mockRegistryProvider{})
					t.Cleanup(al.Close)
					agent := al.registry.GetDefaultAgent()
					if target.account != "shared" {
						agent = isolatedBot(t, al.registry, target.account)
					}
					const privateReasoning = "当前用户私有记忆中的推理内容"
					response := &providers.LLMResponse{Content: "正常回答", FinishReason: "stop"}
					wantReasoning := privateReasoning
					if reasoningField == "Reasoning" {
						response.Reasoning = privateReasoning
					} else if reasoningField == "ReasoningContent" {
						response.ReasoningContent = privateReasoning
					} else {
						wantReasoning = ""
					}
					wantForward := target.account == "shared" && target.chatID != "" && wantReasoning != ""
					wantControl := ControlBreak
					if withTool {
						response.ToolCalls = []providers.ToolCall{{
							ID: "read-own-file", Name: "read_file", Arguments: map[string]any{"path": "MEMORY.md"},
						}}
						response.FinishReason = "tool_calls"
						wantControl = ControlToolLoop
					}
					agent.Provider = &sequenceProvider{responses: []*providers.LLMResponse{response}}
					agent.CandidateProviders, agent.Candidates = nil, nil
					agent.ContextWindow = 100000
					manager, err := channels.NewManager(&config.Config{}, msgBus, nil)
					if err != nil {
						t.Fatal(err)
					}
					manager.RegisterChannel("weixin", &fakeChannel{id: cfg.Channels["weixin"].ReasoningChannelID})
					al.SetChannelManager(manager)
					opts := makeTestProcessOpts("reasoning-session")
					opts.Channel = "weixin"
					ts := newTurnState(agent, normalizeProcessOptions(opts), turnEventScope{})
					pipeline := NewPipeline(al)
					exec, err := pipeline.SetupTurn(context.Background(), ts)
					if err != nil {
						t.Fatal(err)
					}
					synctest.Test(t, func(t *testing.T) {
						// 保持回合上下文有效，并等待异步转发结束，避免因取消或调度竞争而误通过。
						control, err := pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 1)
						if err != nil || control != wantControl {
							t.Fatalf("模型调用结果 = %v, %v，预期 %v", control, err, wantControl)
						}
						if !withTool && exec.finalContent != response.Content {
							t.Fatalf("正常回答不应受推理转发限制影响: %q", exec.finalContent)
						}
						synctest.Wait()
						if got := responseReasoningContent(exec.response); got != wantReasoning {
							t.Fatalf("当前用户的推理内容应保留，实际为 %q", got)
						}
						select {
						case outbound := <-msgBus.OutboundChan():
							if !wantForward {
								t.Fatalf("本回合不应向共用会话转发推理: %+v", outbound)
							}
							if outbound.Context.Channel != "weixin" || outbound.Context.ChatID != target.chatID ||
								outbound.Content != wantReasoning {
								t.Fatalf("共享模式推理转发内容不符: %+v", outbound)
							}
						default:
							if wantForward {
								t.Fatal("共享模式应继续向配置的会话转发推理")
							}
						}
					})
				})
			}
		}
	}
}

func TestWeixinIsolationPendingReasoningStaysPrivateAfterReload(t *testing.T) {
	for _, keepIsolated := range []bool{true, false} {
		name := "保留隔离"
		if !keepIsolated {
			name = "关闭隔离"
		}
		t.Run(name, func(t *testing.T) {
			_, cfg := isolatedRegistry(t)
			msgBus := bus.NewMessageBus()
			al := NewAgentLoop(cfg, msgBus, &mockRegistryProvider{})
			t.Cleanup(al.Close)
			oldRegistry := al.GetRegistry()
			t.Cleanup(oldRegistry.Close)
			alice := isolatedBot(t, oldRegistry, "alice")
			alice.Provider = &reasoningContentProvider{response: "正常回答", reasoningContent: "重载前用户的私有推理"}
			alice.Candidates, alice.CandidateProviders = nil, nil
			alice.ContextWindow = 100000
			opts := makeTestProcessOpts("pending-reasoning-session")
			opts.Channel = "weixin"
			ts := newTurnState(alice, normalizeProcessOptions(opts), turnEventScope{})
			pipeline := NewPipeline(al)
			exec, err := pipeline.SetupTurn(context.Background(), ts)
			if err != nil {
				t.Fatal(err)
			}

			decoded, err := cfg.Channels["weixin"].GetDecoded()
			if err != nil {
				t.Fatal(err)
			}
			settings := *decoded.(*config.WeixinSettings)
			settings.IsolateAccounts = keepIsolated
			raw, err := json.Marshal(settings)
			if err != nil {
				t.Fatal(err)
			}
			reloaded := config.DefaultConfig()
			reloaded.Agents = cfg.Agents
			reloaded.Channels = config.ChannelsConfig{"weixin": {
				Enabled: true, Type: config.ChannelWeixin,
				ReasoningChannelID: "reasoning-chat-after-reload", Settings: config.RawNode(raw),
			}}
			if err := al.ReloadProviderAndConfig(context.Background(), &mockRegistryProvider{}, reloaded); err != nil {
				t.Fatal(err)
			}
			route := al.GetRegistry().ResolveRoute(bus.InboundContext{Channel: "weixin", Account: "alice"})
			if route.Isolated != keepIsolated {
				t.Fatalf("重载后的路由隔离状态错误: %+v", route)
			}
			manager, err := channels.NewManager(&config.Config{}, msgBus, nil)
			if err != nil {
				t.Fatal(err)
			}
			manager.RegisterChannel("weixin", &fakeChannel{id: reloaded.Channels["weixin"].ReasoningChannelID})
			al.SetChannelManager(manager)

			synctest.Test(t, func(t *testing.T) {
				// 已开始的回合继续使用原隔离实例，不能按重载后的渠道配置放行私有推理。
				control, err := pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 1)
				if err != nil || control != ControlBreak || exec.finalContent != "正常回答" {
					t.Fatalf("重载前的回合应正常返回: %v, %v, %q", control, err, exec.finalContent)
				}
				synctest.Wait()
				select {
				case outbound := <-msgBus.OutboundChan():
					t.Fatalf("重载不能将旧隔离回合的推理转发到共用会话: %+v", outbound)
				default:
				}
			})
		})
	}
}
