package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

func isolatedRegistry(t *testing.T) (*AgentRegistry, *config.Config) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = filepath.Join(t.TempDir(), "shared")
	// 故意放宽全局配置，验证隔离 Agent 不会继承这些权限。
	cfg.Agents.Defaults.RestrictToWorkspace = false
	cfg.Agents.Defaults.AllowReadOutsideWorkspace = true
	cfg.Tools.AllowReadPaths = []string{".*"}
	cfg.Tools.AllowWritePaths = []string{".*"}
	cfg.Channels = config.ChannelsConfig{"weixin": {
		Enabled: true, Type: config.ChannelWeixin,
		Settings: config.RawNode(`{"isolate_accounts":true,"accounts":{"alice":{"token":"alice-token","account_id":"alice-bot"},"bob":{"token":"bob-token","account_id":"bob-bot"}}}`),
	}}
	r := NewAgentRegistry(cfg, &mockRegistryProvider{})
	t.Cleanup(r.Close)
	return r, cfg
}

func isolatedBot(t *testing.T, r *AgentRegistry, account string) *AgentInstance {
	t.Helper()
	route := r.ResolveRoute(bus.InboundContext{Channel: "weixin", Account: account})
	agent, ok := r.GetAgent(route.AgentID)
	if !ok || !agent.Isolated {
		t.Fatalf("未创建 %s 的隔离 Agent", account)
	}
	return agent
}

func TestWeixinIsolationMemoryFilesAndHistory(t *testing.T) {
	r, cfg := isolatedRegistry(t)
	alice, bob := isolatedBot(t, r, "alice"), isolatedBot(t, r, "bob")
	if err := alice.ContextBuilder.memory.WriteLongTerm("只有 Alice 能看到的记忆"); err != nil {
		t.Fatal(err)
	}
	alice.Sessions.AddMessage("same-key", "user", "Alice 的聊天")
	alice.Sessions.SetSummary("same-key", "Alice 的摘要")
	if err := alice.Sessions.Save("same-key"); err != nil {
		t.Fatal(err)
	}
	if got := bob.ContextBuilder.memory.ReadLongTerm(); got != "" {
		t.Fatalf("Bob 读到了其他人的记忆: %s", got)
	}
	if history := bob.Sessions.GetHistory("same-key"); len(history) != 0 {
		t.Fatal("不同 Bot 的相同会话键不能共享聊天")
	}
	if bob.Sessions.GetSummary("same-key") != "" {
		t.Fatal("不同 Bot 不能共享会话摘要")
	}
	write, ok := alice.Tools.Get("write_file")
	if !ok {
		t.Fatal("隔离用户应能保存自己的文件")
	}
	if result := write.Execute(context.Background(), map[string]any{"path": "private.txt", "content": "Alice 的文件"}); result.IsError {
		t.Fatal(result.ForLLM)
	}
	read, _ := bob.Tools.Get("read_file")
	list, _ := bob.Tools.Get("list_dir")
	for _, path := range []string{filepath.Join(alice.Workspace, "private.txt"), filepath.Join(cfg.WorkspacePath(), "secret.txt"), "../outside.txt"} {
		if result := read.Execute(context.Background(), map[string]any{"path": path}); !result.IsError {
			t.Fatalf("不应允许读取其他目录: %s", path)
		}
	}
	if result := write.Execute(context.Background(), map[string]any{"path": filepath.Join(bob.Workspace, "stolen.txt"), "content": "越权"}); !result.IsError {
		t.Fatal("不应允许越界写入其他用户目录")
	}
	if result := list.Execute(context.Background(), map[string]any{"path": alice.Workspace}); !result.IsError {
		t.Fatal("不应允许列出其他用户的目录")
	}
	if err := os.Symlink(alice.Workspace, filepath.Join(bob.Workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	if result := read.Execute(context.Background(), map[string]any{"path": "escape/private.txt"}); !result.IsError {
		t.Fatal("不能通过符号链接访问其他用户")
	}
	r.Close()
	restarted := NewAgentRegistry(cfg, &mockRegistryProvider{})
	t.Cleanup(restarted.Close)
	if got := isolatedBot(t, restarted, "alice").ContextBuilder.memory.ReadLongTerm(); !strings.Contains(got, "Alice") {
		t.Fatal("重启后丢失了用户自己的长期记忆")
	}
	if got := isolatedBot(t, restarted, "bob").ContextBuilder.memory.ReadLongTerm(); got != "" {
		t.Fatal("重启后用户记忆串用")
	}
	if history := isolatedBot(t, restarted, "alice").Sessions.GetHistory("same-key"); len(history) != 1 {
		t.Fatal("重启后丢失了用户自己的聊天历史")
	}
	if summary := isolatedBot(t, restarted, "alice").Sessions.GetSummary("same-key"); summary != "Alice 的摘要" {
		t.Fatal("重启后会话摘要丢失")
	}
}

func TestWeixinIsolationCannotExpandToolsOrSpawn(t *testing.T) {
	r, cfg := isolatedRegistry(t)
	alice, bob := isolatedBot(t, r, "alice"), isolatedBot(t, r, "bob")
	for _, name := range []string{"exec", "message", "spawn", "delegate", "subagent", "search_tools", "mcp_secret", "send_file", "load_image"} {
		alice.Tools.Register(&allowlistTestTool{name: name})
		alice.Tools.RegisterHidden(&allowlistTestTool{name: name})
		if alice.Tools.HasRegistered(name) {
			t.Fatalf("不应注册隔离白名单外的工具: %s", name)
		}
	}
	if alice.AllowsMCPServer("secret") {
		t.Fatal("隔离 Agent 不能调用共享 MCP")
	}
	main := r.GetDefaultAgent()
	main.Subagents = &config.SubagentsConfig{AllowAgents: []string{"*"}}
	if r.CanSpawnSubagent(main.ID, alice.ID) || r.CanSpawnSubagent(alice.ID, bob.ID) {
		t.Fatal("不能通过跨 Agent 调用访问隔离用户")
	}
	main.Tools.Register(&allowlistTestTool{name: "spawn"})
	if peers := r.ListSpawnableAgents(main.ID); len(peers) != 0 {
		t.Fatalf("共享 Agent 的提示词不能发现隔离用户的名称或描述: %+v", peers)
	}
	if err := os.WriteFile(filepath.Join(alice.Workspace, "AGENT.md"), []byte("---\ntools: [read_file, exec, message]\nmodel: codex-cli/codex\nmcpServers: [secret]\n---\n用户提示词"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Close()
	restarted := NewAgentRegistry(cfg, &mockRegistryProvider{})
	t.Cleanup(restarted.Close)
	changed := isolatedBot(t, restarted, "alice")
	if changed.Tools.HasRegistered("exec") || changed.AllowsMCPServer("secret") || changed.Model != cfg.Agents.Defaults.GetModelName() {
		t.Fatal("用户可写的 AGENT.md 不能提升工具或模型权限")
	}
}

func TestWeixinIsolationRejectsSharedFallbackAndAdminCommands(t *testing.T) {
	r, cfg := isolatedRegistry(t)
	al := &AgentLoop{registry: r, cfg: cfg}
	alice := isolatedBot(t, r, "alice")
	msg := bus.InboundMessage{Context: bus.InboundContext{Channel: "weixin", Account: "missing"}}
	if _, _, err := al.resolveMessageRoute(msg); err == nil {
		t.Fatal("未知隔离账号不能回退到默认 Agent")
	}
	for _, command := range []string{"/reload", "/switch model local", "/list agents", "/show mcp secret", "/subagents"} {
		_, handled := al.handleCommand(context.Background(), bus.InboundMessage{Content: command}, alice, &processOptions{})
		if !handled {
			t.Fatalf("隔离用户不应执行管理命令: %s", command)
		}
	}
	if _, err := al.ProcessHeartbeatWithAccount(context.Background(), "共享工作目录内的任务", "weixin", "user", "alice"); err == nil {
		t.Fatal("共享 heartbeat 不能进入独立用户会话")
	}
	cfg.Agents.Dispatch = &config.DispatchConfig{Rules: []config.DispatchRule{{
		Agent: alice.ID, When: config.DispatchSelector{Channel: "telegram"},
	}}}
	// 显式列表让普通 dispatch 能解析到该 ID，实际处理仍必须拒绝跨渠道访问。
	cfg.Agents.List = append(cfg.Agents.List, config.AgentConfig{ID: alice.ID})
	if _, _, err := al.resolveMessageRoute(bus.InboundMessage{Context: bus.InboundContext{Channel: "telegram"}}); err == nil {
		t.Fatal("其他渠道不能通过指定 Agent ID 进入隔离用户目录")
	}
	delete(r.agents, alice.ID)
	msg.Context.Account = "alice"
	if _, _, err := al.resolveMessageRoute(msg); err == nil {
		t.Fatal("隔离 Agent 缺失时不能回退到默认 Agent")
	}
}

func TestWeixinIsolationSharedAgentWorkspaces(t *testing.T) {
	for _, tc := range []struct {
		name      string
		id        string
		isDefault bool
		workspace string
		symlink   bool
		allowed   bool
	}{
		{name: "主 Agent 覆盖祖先", id: "main", isDefault: true, workspace: "."},
		{name: "其他共享 Agent 覆盖祖先", id: "writer", workspace: "."},
		{name: "其他共享 Agent 位于隔离根内", id: "writer", workspace: "weixin-users/shared"},
		{name: "其他共享 Agent 链接到隔离根", id: "writer", workspace: "alias", symlink: true},
		{name: "命名 Agent 自动派生独立目录", id: " Research Bot! ", allowed: true},
		{name: "默认 Agent 沿用默认目录", id: "writer", isDefault: true, allowed: true},
		{name: "默认 Agent 显式覆盖独立目录", id: "writer", isDefault: true, workspace: "separate", allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			cfg := config.DefaultConfig()
			cfg.Agents.Defaults.Workspace = filepath.Join(base, "workspace")
			cfg.Agents.Defaults.RestrictToWorkspace = true
			cfg.Channels = config.ChannelsConfig{"weixin": {
				Enabled: true, Type: config.ChannelWeixin,
				Settings: config.RawNode(`{"isolate_accounts":true,"accounts":{"alice":{"token":"alice-token","account_id":"alice-bot"},"bob":{"token":"bob-token","account_id":"bob-bot"}}}`),
			}}
			shared := config.AgentConfig{ID: tc.id, Default: tc.isDefault}
			if tc.workspace != "" {
				shared.Workspace = filepath.Join(base, tc.workspace)
			}
			if tc.symlink {
				derived := filepath.Join(base, "weixin-users")
				if err := os.MkdirAll(derived, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(derived, shared.Workspace); err != nil {
					t.Fatal(err)
				}
			}
			cfg.Agents.List = []config.AgentConfig{shared}
			if !tc.isDefault {
				cfg.Agents.List = append([]config.AgentConfig{{ID: "main", Default: true}}, shared)
			}
			r := NewAgentRegistry(cfg, &mockRegistryProvider{})
			t.Cleanup(r.Close)
			al := &AgentLoop{cfg: cfg, registry: r}
			al.contextManager = &legacyContextManager{al: al}
			if !tc.allowed {
				for _, id := range r.ListAgentIDs() {
					instance, _ := r.GetAgent(id)
					if instance.Isolated {
						t.Fatal("任一共享 Agent 的实际目录重叠时不能注册隔离实例")
					}
				}
			}
			for _, account := range []string{"alice", "bob"} {
				_, instance, err := al.resolveMessageRoute(bus.InboundMessage{Context: bus.InboundContext{
					Channel: "weixin", Account: account, ChatID: "user", SenderID: "user",
				}})
				if tc.allowed {
					if err != nil || instance == nil || !instance.Isolated {
						t.Fatalf("共享目录独立时 %s 应正常进入隔离实例: %v", account, err)
					}
				} else if err == nil {
					t.Fatalf("共享目录重叠时 %s 不能进入隔离实例或回退到共享 Agent", account)
				}
			}
			if _, instance, err := al.resolveMessageRoute(bus.InboundMessage{Context: bus.InboundContext{
				Channel: "telegram", ChatID: "shared", SenderID: "shared",
			}}); err != nil || instance == nil || instance.Isolated {
				t.Fatalf("其他渠道应仍能使用共享 Agent: %v", err)
			}
		})
	}
}

func TestWeixinIsolationRejectsWorkspaceSymlinkOverlap(t *testing.T) {
	for _, timing := range []string{"初始化前", "初始化后"} {
		t.Run(timing, func(t *testing.T) {
			cfg := config.DefaultConfig()
			base := t.TempDir()
			shared := filepath.Join(base, "shared")
			cfg.Agents.Defaults.Workspace = shared
			cfg.Agents.Defaults.RestrictToWorkspace = true
			cfg.Channels = config.ChannelsConfig{"weixin": {
				Enabled: true, Type: config.ChannelWeixin,
				Settings: config.RawNode(`{
					"isolate_accounts": true,
					"accounts": {
						"alice": {"token": "alice-token", "account_id": "alice-bot"},
						"bob": {"token": "bob-token", "account_id": "bob-bot"}
					}
				}`),
			}}
			if timing == "初始化前" {
				if err := os.Symlink(".", shared); err != nil {
					t.Fatal(err)
				}
			}
			r := NewAgentRegistry(cfg, &mockRegistryProvider{})
			t.Cleanup(r.Close)
			al := &AgentLoop{cfg: cfg, registry: r}
			al.contextManager = &legacyContextManager{al: al}
			if timing == "初始化前" {
				if len(r.ListAgentIDs()) != 1 || r.GetDefaultAgent() == nil {
					t.Fatal("真实目录重叠时只能保留共享 Agent，不能创建隔离实例")
				}
			} else {
				isolatedBot(t, r, "alice")
				for _, account := range []string{"alice", "bob"} {
					if _, _, err := al.resolveMessageRoute(bus.InboundMessage{Context: bus.InboundContext{
						Channel: "weixin", Account: account, ChatID: "user", SenderID: "user",
					}}); err != nil {
						t.Fatalf("替换链接前应能路由到 %s 的隔离实例: %v", account, err)
					}
				}
				if err := os.Rename(shared, filepath.Join(base, "original-shared")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(".", shared); err != nil {
					t.Fatal(err)
				}
			}
			for _, account := range []string{"alice", "bob"} {
				if _, _, err := al.resolveMessageRoute(bus.InboundMessage{Context: bus.InboundContext{
					Channel: "weixin", Account: account, ChatID: "user", SenderID: "user",
				}}); err == nil {
					t.Fatalf("%s 目录重叠时，%s 不能进入隔离实例或回退到共享 Agent", timing, account)
				}
			}
		})
	}
}

func TestWeixinIsolationRejectsLocalModelProviders(t *testing.T) {
	for _, position := range []string{"primary", "fallback", "image", "light"} {
		t.Run(position, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.ModelList = append(cfg.ModelList, &config.ModelConfig{ModelName: "local", Model: "codex-cli/codex"})
			switch position {
			case "primary":
				cfg.Agents.Defaults.ModelName = "local"
			case "fallback":
				cfg.Agents.Defaults.ModelFallbacks = []string{"local"}
			case "image":
				cfg.Agents.Defaults.ImageModel = "local"
			case "light":
				cfg.Agents.Defaults.Routing = &config.RoutingConfig{Enabled: true, LightModel: "local"}
			}
			if err := validateIsolatedModels(cfg); err == nil {
				t.Fatal("不能通过主模型或其他模型分支执行本机命令")
			}
		})
	}
}

func TestWeixinIsolationTwoTurnsUseOwnHistory(t *testing.T) {
	_, cfg := isolatedRegistry(t)
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockRegistryProvider{})
	t.Cleanup(al.Close)
	alice := isolatedBot(t, al.registry, "alice")
	bob := isolatedBot(t, al.registry, "bob")
	capture := &recordingProvider{}
	alice.Provider = capture
	alice.CandidateProviders = nil
	alice.ContextWindow = 100000
	msg := bus.InboundMessage{
		Context: bus.InboundContext{Channel: "weixin", Account: "alice", ChatID: "user", SenderID: "user", ChatType: "direct"},
		Content: "第一轮只有 Alice 的私密内容",
	}
	if _, err := al.processMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	route := al.registry.ResolveRoute(msg.Context)
	key := al.allocateRouteSession(route, msg).SessionKey
	main := al.registry.GetDefaultAgent()
	main.Sessions.AddMessage(key, "user", "共享 Agent 的私密内容")
	bob.Sessions.AddMessage(key, "user", "Bob 的私密内容")
	alice.Sessions.SetSummary(key, "Alice 自己的摘要")
	msg.Content = "第二轮继续对话"
	if _, err := al.processMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	var prompt strings.Builder
	for _, message := range capture.lastMessages {
		prompt.WriteString(message.Content)
	}
	for _, want := range []string{"第一轮只有 Alice 的私密内容", "Mock response", "Alice 自己的摘要", msg.Content} {
		if !strings.Contains(prompt.String(), want) {
			t.Fatalf("第二轮提示词缺少自己的上下文: %s", want)
		}
	}
	for _, secret := range []string{"共享 Agent 的私密内容", "Bob 的私密内容"} {
		if strings.Contains(prompt.String(), secret) {
			t.Fatalf("第二轮提示词泄漏其他 Agent 的上下文: %s", secret)
		}
	}
}

func TestWeixinIsolationContextCompressionAndSummary(t *testing.T) {
	for _, reason := range []ContextCompressReason{ContextCompressReasonProactive, ContextCompressReasonRetry, ContextCompressReasonSummarize} {
		t.Run(string(reason), func(t *testing.T) {
			_, cfg := isolatedRegistry(t)
			al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockRegistryProvider{})
			t.Cleanup(al.Close)
			r := al.registry
			alice := isolatedBot(t, r, "alice")
			bob := isolatedBot(t, r, "bob")
			if alice.ID < bob.ID {
				alice, bob = bob, alice
			}
			cm := &legacyContextManager{al: al}
			key := session.BuildOpaqueSessionKey("alice-context")
			alice.Sessions.(session.MetadataAwareSessionStore).EnsureSessionMetadata(key, &session.SessionScope{AgentID: alice.ID}, nil)
			// 排序更靠前的用户伪造相同会话键，不能影响目标用户的压缩和摘要。
			bob.Sessions.(session.MetadataAwareSessionStore).EnsureSessionMetadata(key, &session.SessionScope{AgentID: bob.ID}, nil)
			alice.Provider = &simpleMockProvider{response: "Alice 的新摘要"}
			alice.SummarizeMessageThreshold = 4
			main := r.GetDefaultAgent()
			for _, agent := range []*AgentInstance{alice, bob, main} {
				for i := 0; i < 4; i++ {
					agent.Sessions.AddMessage(key, "user", "用户内容")
					agent.Sessions.AddMessage(key, "assistant", "回复内容")
				}
				agent.Sessions.SetSummary(key, "原摘要")
			}
			if err := cm.Compact(withContextAgent(context.Background(), alice), &CompactRequest{SessionKey: key, Reason: reason}); err != nil {
				t.Fatal(err)
			}
			if reason == ContextCompressReasonSummarize {
				deadline := time.Now().Add(3 * time.Second)
				for {
					_, running := cm.summarizing.Load(alice.ID + ":" + key)
					if !running {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("等待隔离会话摘要超时")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if got := len(alice.Sessions.GetHistory(key)); got != 4 {
				t.Fatalf("隔离会话应压缩为 4 条消息，实际 %d", got)
			}
			if got := alice.Sessions.GetSummary(key); got == "原摘要" {
				t.Fatal("隔离会话摘要未更新")
			}
			for _, agent := range []*AgentInstance{main, bob} {
				if len(agent.Sessions.GetHistory(key)) != 8 || agent.Sessions.GetSummary(key) != "原摘要" {
					t.Fatal("摘要或压缩修改了其他 Agent 的存储")
				}
			}
		})
	}
}

func TestWeixinIsolationRejectsEveryAliasCandidate(t *testing.T) {
	for _, position := range []string{"primary", "fallback", "image", "image-fallback", "light"} {
		for _, protocol := range []string{"codex-cli", "claude-cli", "github-copilot"} {
			t.Run(position+"/"+protocol, func(t *testing.T) {
				cfg := config.DefaultConfig()
				cfg.ModelList = []*config.ModelConfig{
					{ModelName: "mixed", Model: "openai/test", APIBase: "http://127.0.0.1:1"},
					{ModelName: "mixed", Provider: protocol, Model: "test"},
				}
				switch position {
				case "primary":
					cfg.Agents.Defaults.ModelName = "mixed"
				case "fallback":
					cfg.Agents.Defaults.ModelFallbacks = []string{"mixed"}
				case "image":
					cfg.Agents.Defaults.ImageModel = "mixed"
				case "image-fallback":
					cfg.Agents.Defaults.ImageModelFallbacks = []string{"mixed"}
				case "light":
					cfg.Agents.Defaults.Routing = &config.RoutingConfig{Enabled: true, LightModel: "mixed"}
				}
				for i := 0; i < 8; i++ {
					if err := validateIsolatedModels(cfg); err == nil {
						t.Fatal("同别名任一候选具有本机权限时必须始终拒绝")
					}
				}
			})
		}
	}
}

func TestWeixinIsolationCreatesOwnProvider(t *testing.T) {
	_, cfg := isolatedRegistry(t)
	cfg.Agents.Defaults.ModelName = "safe-http"
	cfg.ModelList = []*config.ModelConfig{{ModelName: "safe-http", Model: "openai/test", APIBase: "http://127.0.0.1:1"}}
	injected := &providers.CodexCliProvider{}
	r := NewAgentRegistry(cfg, injected)
	t.Cleanup(r.Close)
	if r.GetDefaultAgent().Provider != injected {
		t.Fatal("共享 Agent 应保留注入的 provider")
	}
	for _, account := range []string{"alice", "bob"} {
		agent := isolatedBot(t, r, account)
		if _, ok := agent.Provider.(*providers.HTTPProvider); !ok {
			t.Fatalf("隔离 Agent 应创建配置指定的 HTTP provider，实际 %T", agent.Provider)
		}
	}
}

func TestWeixinIsolationSkipsEvolution(t *testing.T) {
	for _, mode := range []string{"observe", "draft", "apply"} {
		for _, trigger := range []string{"after_turn", "scheduled"} {
			t.Run(mode+"/"+trigger, func(t *testing.T) {
				r, cfg := isolatedRegistry(t)
				cfg.Evolution.Enabled = true
				cfg.Evolution.Mode = mode
				cfg.Evolution.ColdPathTrigger = trigger
				cfg.Evolution.ColdPathTimes = []string{"03:00"}
				bridge, err := newEvolutionBridge(r, cfg, &mockRegistryProvider{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = bridge.Close() })
				for _, account := range []string{"alice", "bob"} {
					agent := isolatedBot(t, r, account)
					if accepted := bridge.handleTurnEndAsync(EventMeta{AgentID: agent.ID, TurnID: "isolated-turn"}, TurnEndPayload{
						Status: TurnEndStatusCompleted, Workspace: agent.Workspace, UserMessage: "隔离用户任务", FinalContent: "成功",
					}); accepted {
						t.Fatal("隔离回合不能进入后台技能演化")
					}
					for _, workspace := range bridge.scheduledColdPathWorkspaces() {
						if workspace == agent.Workspace {
							t.Fatal("定时后台扫描不能包含隔离目录")
						}
					}
					assertNotExists(t, filepath.Join(agent.Workspace, "state", "evolution", "task-records.jsonl"))
				}
				workspaces := registryWorkspaces(r)
				if len(workspaces) != 1 || workspaces[0] != r.GetDefaultAgent().Workspace {
					t.Fatalf("后台扫描应保留共享 Agent，实际 %v", workspaces)
				}
			})
		}
	}
}

func TestWeixinIsolationSkipsEvolutionAfterConfigReload(t *testing.T) {
	r, cfg := isolatedRegistry(t)
	cfg.Evolution.Enabled = true
	cfg.Evolution.Mode = "observe"
	bridge, err := newEvolutionBridge(r, cfg, &mockRegistryProvider{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	agent := isolatedBot(t, r, "alice")
	// 模拟配置重载后旧 Agent 从新 registry 消失；过滤不能再依赖当前查找结果。
	delete(r.agents, agent.ID)
	if accepted := bridge.handleTurnEndAsync(EventMeta{AgentID: agent.ID, TurnID: "reloaded-isolated-turn"}, TurnEndPayload{
		Isolated:     true,
		Status:       TurnEndStatusCompleted,
		Workspace:    agent.Workspace,
		UserMessage:  "隔离用户任务",
		FinalContent: "成功",
	}); accepted {
		t.Fatal("重载后旧隔离回合不能进入后台技能演化")
	}
	assertNotExists(t, filepath.Join(agent.Workspace, "state", "evolution", "task-records.jsonl"))
}

func TestWeixinIsolationRejectsSharedContextBackend(t *testing.T) {
	for _, backend := range []string{"seahorse", "custom-shared-context"} {
		t.Run(backend, func(t *testing.T) {
			_, cfg := isolatedRegistry(t)
			cfg.Agents.Defaults.ContextManager = backend
			r := NewAgentRegistry(cfg, &mockRegistryProvider{})
			t.Cleanup(r.Close)
			if len(r.ListAgentIDs()) != 1 || r.GetDefaultAgent() == nil {
				t.Fatal("共享上下文后端只能保留共享 Agent")
			}
			al := &AgentLoop{cfg: cfg, registry: r}
			if _, _, err := al.resolveMessageRoute(bus.InboundMessage{Context: bus.InboundContext{
				Channel: "weixin", Account: "alice",
			}}); err == nil {
				t.Fatal("隔离账号不能回退共享 Agent 或共享上下文")
			}
		})
	}
	// 重载只更新配置而保留共享后端时，也必须拒绝隔离消息。
	r, cfg := isolatedRegistry(t)
	al := &AgentLoop{cfg: cfg, registry: r, contextManager: &noopContextManager{}}
	msg := bus.InboundMessage{Context: bus.InboundContext{Channel: "weixin", Account: "alice"}}
	if _, _, err := al.resolveMessageRoute(msg); err == nil {
		t.Fatal("不能只依据重载后的配置放行实际仍使用共享后端的实例")
	}
	al.contextManager = &legacyContextManager{al: al}
	if _, _, err := al.resolveMessageRoute(msg); err != nil {
		t.Fatalf("独立会话后端应正常处理隔离消息: %v", err)
	}
}

func TestWeixinIsolationReadsOwnInboundAttachment(t *testing.T) {
	r, cfg := isolatedRegistry(t)
	alice, bob := isolatedBot(t, r, "alice"), isolatedBot(t, r, "bob")
	store := media.NewFileMediaStore()
	src := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(src, []byte("Alice 本人的附件内容"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := store.Store(src, media.MediaMeta{Filename: "report.txt", ContentType: "text/plain", OwnerAgentID: alice.ID}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	messages := []providers.Message{{Role: "user", Content: "[file: report.txt]", Media: []string{ref}}}
	resolved := resolveMediaRefsForAgent(messages, store, cfg.Agents.Defaults.GetMaxMediaSize(), 0, alice)
	content := resolved[0].Content
	if !strings.HasPrefix(content, "[file:") || !strings.HasSuffix(content, "]") {
		t.Fatalf("缺少可读取的附件路径: %s", content)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(content, "[file:"), "]")
	if filepath.Dir(path) != filepath.Join(alice.Workspace, ".inbound-media") {
		t.Fatalf("附件必须保存在自己的目录内: %s", path)
	}
	read, _ := alice.Tools.Get("read_file")
	if result := read.Execute(context.Background(), map[string]any{"path": path}); result.IsError || !strings.Contains(result.ForLLM, "Alice 本人的附件内容") {
		t.Fatalf("用户无法读取本人的附件: %s", result.ForLLM)
	}
	for _, other := range []string{src, filepath.Join(bob.Workspace, "private.txt")} {
		if result := read.Execute(context.Background(), map[string]any{"path": other}); !result.IsError {
			t.Fatalf("附件读取不能开放外部路径: %s", other)
		}
	}
	bobRead, _ := bob.Tools.Get("read_file")
	if result := bobRead.Execute(context.Background(), map[string]any{"path": path}); !result.IsError {
		t.Fatal("其他 Bot 不能读取附件副本")
	}
	for _, owner := range []string{bob.ID, "", "previous-binding"} {
		foreign, err := store.Store(src, media.MediaMeta{OwnerAgentID: owner}, "foreign")
		if err != nil {
			t.Fatal(err)
		}
		got := resolveMediaRefsForAgent([]providers.Message{{Role: "user", Content: "[file]", Media: []string{foreign}}}, store, 1024, 0, alice)
		if got[0].Content != "[file]" || len(got[0].Media) != 0 {
			t.Fatal("无归属、其他账号或旧绑定的附件不能被读取")
		}
	}
	again := resolveMediaRefsForAgent(messages, store, cfg.Agents.Defaults.GetMaxMediaSize(), 0, alice)
	if again[0].Content != content || len(messages[0].Media) != 1 || messages[0].Content != "[file: report.txt]" {
		t.Fatal("提示词重建应复用附件路径且不能修改原始消息")
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatalf("重复解析不应重复创建附件: %v, %v", files, err)
	}
}

func TestWeixinIsolationAttachmentCopyRejectsEscapeAndOversize(t *testing.T) {
	for _, scenario := range []string{"directory-symlink", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			r, _ := isolatedRegistry(t)
			alice, bob := isolatedBot(t, r, "alice"), isolatedBot(t, r, "bob")
			before, err := os.ReadDir(bob.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(src, []byte("附件内容"), 0o600); err != nil {
				t.Fatal(err)
			}
			maxSize := 1024
			if scenario == "directory-symlink" {
				if err := os.Symlink(bob.Workspace, filepath.Join(alice.Workspace, ".inbound-media")); err != nil {
					t.Fatal(err)
				}
			} else {
				maxSize = 1
			}
			if _, err := copyMediaIntoWorkspace(src, "media://test", alice.Workspace, maxSize); err == nil {
				t.Fatal("必须拒绝通过符号链接越界复制和超大附件")
			}
			files, err := os.ReadDir(bob.Workspace)
			if err != nil || len(files) != len(before) {
				t.Fatalf("不能写入其他用户目录: %v, %v", files, err)
			}
			for i := range files {
				if files[i].Name() != before[i].Name() {
					t.Fatal("附件复制修改了其他用户目录")
				}
			}
		})
	}
}

func TestWeixinIsolationAttachmentInTurnPrompt(t *testing.T) {
	_, cfg := isolatedRegistry(t)
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockRegistryProvider{})
	t.Cleanup(al.Close)
	alice := isolatedBot(t, al.registry, "alice")
	capture := &recordingProvider{}
	alice.Provider, alice.CandidateProviders = capture, nil
	alice.ContextWindow = 100000
	store := media.NewFileMediaStore()
	al.SetMediaStore(store)
	src := filepath.Join(t.TempDir(), "upload.txt")
	if err := os.WriteFile(src, []byte("上传文件"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := store.Store(src, media.MediaMeta{ContentType: "text/plain", OwnerAgentID: alice.ID}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := al.processMessage(context.Background(), bus.InboundMessage{
		Context: bus.InboundContext{Channel: "weixin", Account: "alice", ChatID: "user", SenderID: "user", ChatType: "direct"},
		Content: "读取附件 [file]", Media: []string{ref},
	}); err != nil {
		t.Fatal(err)
	}
	last := capture.lastMessages[len(capture.lastMessages)-1]
	if !strings.Contains(last.Content, filepath.Join(alice.Workspace, ".inbound-media")) || strings.Contains(last.Content, src) {
		t.Fatalf("回合提示词应使用隔离目录内的附件路径: %s", last.Content)
	}
}

func TestWeixinIsolationAudioChecksOwnerBeforeTranscription(t *testing.T) {
	r, cfg := isolatedRegistry(t)
	alice, bob := isolatedBot(t, r, "alice"), isolatedBot(t, r, "bob")
	al := &AgentLoop{cfg: cfg, registry: r, transcriber: &fixedTranscriber{text: "允许转写的内容"}}
	store := media.NewFileMediaStore()
	al.mediaStore = store
	src := filepath.Join(t.TempDir(), "voice.wav")
	if err := os.WriteFile(src, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{alice.ID, bob.ID, "", "previous-binding"} {
		ref, err := store.Store(src, media.MediaMeta{Filename: "voice.wav", ContentType: "audio/wav", OwnerAgentID: owner}, "audio")
		if err != nil {
			t.Fatal(err)
		}
		msg := bus.NormalizeInboundMessage(bus.InboundMessage{
			Context: bus.InboundContext{Channel: "weixin", Account: "alice"},
			Content: "[audio]", Media: []string{ref},
		})
		got, transcribed := al.transcribeAudioInMessage(context.Background(), msg)
		if transcribed != (owner == alice.ID) || strings.Contains(got.Content, "允许转写的内容") != transcribed {
			t.Fatalf("语音转写必须先匹配账号归属: owner=%q, got=%s", owner, got.Content)
		}
		msg.Context.Raw = map[string]string{"weixin_isolated": "true", "weixin_bot_identity": "previous-binding"}
		if _, transcribed := al.transcribeAudioInMessage(context.Background(), msg); transcribed {
			t.Fatal("换绑前的消息不能读取或转写当前附件")
		}
	}
}
