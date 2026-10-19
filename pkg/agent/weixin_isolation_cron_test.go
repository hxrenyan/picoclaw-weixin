package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
)

type isolatedCronFeedbackProvider struct {
	*sequenceProvider
	beforeResponse func()
}

func (p *isolatedCronFeedbackProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	if p.beforeResponse != nil {
		beforeResponse := p.beforeResponse
		p.beforeResponse = nil
		beforeResponse()
	}
	return p.sequenceProvider.Chat(ctx, messages, toolDefs, model, opts)
}

func TestWeixinIsolationCronSuppressesToolFeedback(t *testing.T) {
	for _, rebind := range []bool{false, true} {
		name := "绑定未变"
		if rebind {
			name = "执行中换绑"
		}
		t.Run(name, func(t *testing.T) {
			_, cfg := isolatedRegistry(t)
			cfg.Agents.Defaults.ToolFeedback.Enabled = true
			msgBus := bus.NewMessageBus()
			al := NewAgentLoop(cfg, msgBus, &mockRegistryProvider{})
			t.Cleanup(al.Close)
			alice := isolatedBot(t, al.registry, "alice")
			provider := &isolatedCronFeedbackProvider{sequenceProvider: &sequenceProvider{
				responses: []*providers.LLMResponse{
					{
						Content: "原用户的私有模型正文",
						ToolCalls: []providers.ToolCall{{
							ID: "write-private", Name: "write_file",
							Arguments: map[string]any{"path": "private.txt", "content": "原用户的私有工具参数"},
						}},
						FinishReason: "tool_calls",
					},
					{Content: "定时任务结果", FinishReason: "stop"},
				},
			}}
			if rebind {
				provider.beforeResponse = func() {
					// 在工具反馈发布前换绑，验证最终复核无法拦截的中间输出。
					next := *cfg
					next.Channels = config.ChannelsConfig{"weixin": {Enabled: true, Type: config.ChannelWeixin, Settings: config.RawNode(`{"isolate_accounts":true,"accounts":{"alice":{"token":"new","account_id":"replacement"}}}`)}}
					if err := al.ReloadProviderAndConfig(context.Background(), &mockRegistryProvider{}, &next); err != nil {
						t.Fatal(err)
					}
				}
			}
			alice.Provider, alice.Candidates, alice.CandidateProviders = provider, nil, nil
			alice.ContextWindow = 100000
			service := cron.NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
			tool, err := tools.NewCronTool(service, al, msgBus, cfg.WorkspacePath(), true, 0, cfg)
			if err != nil {
				t.Fatal(err)
			}
			job := &cron.CronJob{ID: "private-feedback", Payload: cron.CronPayload{
				Message: "写入自己的文件", Channel: "weixin", To: "user", Account: "alice", IsolationAgentID: alice.ID,
			}}
			result := tool.ExecuteJob(context.Background(), job)
			if rebind {
				if !strings.HasPrefix(result, "Error:") {
					t.Fatalf("换绑后应拒绝最终投递: %s", result)
				}
			} else {
				if result != "ok" {
					t.Fatal(result)
				}
				select {
				case outbound := <-msgBus.OutboundChan():
					if outbound.Content != "定时任务结果" || outbound.Context.Account != "alice" || outbound.Context.ChatID != "user" {
						t.Fatalf("应只向原账号投递最终结果: %+v", outbound)
					}
				default:
					t.Fatal("缺少任务最终回复")
				}
			}
			if content, err := os.ReadFile(filepath.Join(alice.Workspace, "private.txt")); err != nil || string(content) != "原用户的私有工具参数" {
				t.Fatalf("任务应在原隔离目录执行工具: %q, %v", content, err)
			}
			select {
			case outbound := <-msgBus.OutboundChan():
				t.Fatalf("隔离定时任务泄露了中间输出: %+v", outbound)
			default:
			}
		})
	}
}

func TestWeixinIsolationToolFeedbackForOtherTurns(t *testing.T) {
	for _, inbound := range []bool{true, false} {
		name := "普通隔离消息"
		if !inbound {
			name = "共享定时任务"
		}
		t.Run(name, func(t *testing.T) {
			_, cfg := isolatedRegistry(t)
			cfg.Agents.Defaults.ToolFeedback.Enabled = true
			msgBus := bus.NewMessageBus()
			al := NewAgentLoop(cfg, msgBus, &mockRegistryProvider{})
			t.Cleanup(al.Close)
			agent := al.registry.GetDefaultAgent()
			if inbound {
				agent = isolatedBot(t, al.registry, "alice")
			}
			if err := os.WriteFile(filepath.Join(agent.Workspace, "feedback.txt"), []byte("自己的文件"), 0o600); err != nil {
				t.Fatal(err)
			}
			agent.Provider = &toolFeedbackProvider{filePath: "feedback.txt"}
			agent.Candidates, agent.CandidateProviders = nil, nil
			agent.ContextWindow = 100000
			var response string
			var err error
			if inbound {
				response, err = al.processMessage(context.Background(), bus.InboundMessage{
					Context: bus.InboundContext{Channel: "weixin", Account: "alice", ChatID: "user", SenderID: "user", ChatType: "direct"},
					Content: "读取自己的文件",
				})
			} else {
				response, err = al.ProcessDirectWithChannelAccount(context.Background(), "读取文件", "agent:cron-shared-feedback", "telegram", "user", "default")
			}
			if err != nil || response != "HEARTBEAT_OK" {
				t.Fatalf("回合执行结果错误: %q, %v", response, err)
			}
			select {
			case outbound := <-msgBus.OutboundChan():
				if !strings.Contains(outbound.Content, "`read_file`") || !strings.Contains(outbound.Content, "feedback.txt") {
					t.Fatalf("缺少正常工具反馈: %+v", outbound)
				}
			default:
				t.Fatal("其他回合仍应遵循全局工具反馈配置")
			}
		})
	}
}

func TestWeixinIsolationCronExecutionAndReload(t *testing.T) {
	_, cfg := isolatedRegistry(t)
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, &mockRegistryProvider{})
	t.Cleanup(al.Close)
	service := cron.NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	tool, err := tools.NewCronTool(service, al, msgBus, cfg.WorkspacePath(), true, 0, cfg)
	if err != nil {
		t.Fatal(err)
	}
	al.registry.GetDefaultAgent().Tools.SetAllowlist([]string{"read_file"})
	al.RegisterTool(tool)
	if al.registry.GetDefaultAgent().Tools.HasRegistered("cron") {
		t.Fatal("默认 Agent 不应绕过其工具白名单")
	}
	alice := isolatedBot(t, al.registry, "alice")
	ctx := tools.WithToolContext(context.Background(), "weixin", "user")
	ctx = tools.WithToolSessionContext(ctx, alice.ID, "create", &session.SessionScope{AgentID: alice.ID, Channel: "weixin", Account: "alice"})
	result := tool.Execute(ctx, map[string]any{"action": "add", "message": "定时搜索消息", "at_seconds": float64(600)})
	if result.IsError {
		t.Fatal(result.ForLLM)
	}
	jobs := service.ListJobs(false)
	if len(jobs) != 1 {
		t.Fatal("任务创建失败")
	}
	if err := al.ReloadProviderAndConfig(context.Background(), &mockRegistryProvider{}, cfg); err != nil {
		t.Fatal(err)
	}
	alice = isolatedBot(t, al.GetRegistry(), "alice")
	if registered, ok := alice.Tools.Get("cron"); !ok || registered != tool {
		t.Fatal("重载丢失定时工具")
	}
	capture := &nativeSearchCaptureProvider{}
	alice.Provider, alice.Candidates, alice.CandidateProviders = capture, nil, nil
	alice.ContextWindow = 100000
	if result := tool.ExecuteJob(context.Background(), &jobs[0]); result != "ok" {
		t.Fatal(result)
	}
	if len(capture.messages) == 0 {
		t.Fatal("隔离 Agent 未执行任务")
	}
	select {
	case outbound := <-msgBus.OutboundChan():
		if outbound.Context.Account != "alice" || outbound.Context.ChatID != "user" {
			t.Fatalf("投递目标错误: %+v", outbound)
		}
	default:
		t.Fatal("缺少任务回复")
	}
	// 新建配置对象模拟换绑重载，不能依赖工具构造时的旧配置。
	next := *cfg
	next.Channels = config.ChannelsConfig{"weixin": {Enabled: true, Type: config.ChannelWeixin, Settings: config.RawNode(`{"isolate_accounts":true,"accounts":{"alice":{"token":"new","account_id":"replacement"}}}`)}}
	if err := al.ReloadProviderAndConfig(context.Background(), &mockRegistryProvider{}, &next); err != nil {
		t.Fatal(err)
	}
	if result := tool.ExecuteJob(context.Background(), &jobs[0]); !strings.HasPrefix(result, "Error:") {
		t.Fatalf("旧任务不应进入新身份: %s", result)
	}
	if _, err := al.ProcessDirectWithIsolation(context.Background(), "旧任务", "stale", "weixin", "user", "alice", jobs[0].Payload.IsolationAgentID); err == nil {
		t.Fatal("专用入口接受了旧绑定")
	}
	var originalSession string
	al.isolatedSessionRoutes.Range(func(key, _ any) bool {
		originalSession = key.(string)
		return false
	})
	if originalSession == "" {
		t.Fatal("缺少原任务可信会话路由")
	}
	al.PublishResponseIfNeededWithAccount(context.Background(), "weixin", "user", originalSession, "不应投递", "alice")
	select {
	case outbound := <-msgBus.OutboundChan():
		t.Fatalf("换绑后仍有投递: %+v", outbound)
	default:
	}
}
