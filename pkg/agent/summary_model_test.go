package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

type summaryModelProvider struct {
	models []string
	closed atomic.Bool
}

func (p *summaryModelProvider) Chat(
	_ context.Context, _ []providers.Message, _ []providers.ToolDefinition, model string, _ map[string]any,
) (*providers.LLMResponse, error) {
	if p.closed.Load() {
		return nil, fmt.Errorf("摘要使用了已关闭的 provider")
	}
	p.models = append(p.models, model)
	return &providers.LLMResponse{Content: "有效对话摘要"}, nil
}

func (p *summaryModelProvider) GetDefaultModel() string { return "" }
func (p *summaryModelProvider) Close()                  { p.closed.Store(true) }

func summaryModelConfig(t *testing.T, model string) *config.Config {
	t.Helper()
	return &config.Config{
		Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
			Workspace: t.TempDir(), ModelName: "chat", MaxTokens: 1024,
		}},
		ModelList: []*config.ModelConfig{{ModelName: "chat", Model: "anthropic/" + model}},
	}
}

func TestLegacySummaryUsesResolvedModel(t *testing.T) {
	cfg := summaryModelConfig(t, "claude-sonnet-4-6")
	p := &summaryModelProvider{}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()
	al := NewAgentLoop(cfg, msgBus, p)
	defer al.Close()
	agent := al.GetRegistry().GetDefaultAgent()
	var history []providers.Message
	for i := 0; i < 6; i++ {
		history = append(history,
			providers.Message{Role: "user", Content: "请保留这轮对话信息"},
			providers.Message{Role: "assistant", Content: "已记录"},
		)
	}
	agent.Sessions.SetHistory("summary-alias", history)
	al.contextManager.(*legacyContextManager).summarizeSession(agent, "summary-alias")
	if len(p.models) != 1 || p.models[0] != "claude-sonnet-4-6" {
		t.Fatalf("摘要应使用实际 ID 且无需重试: %v", p.models)
	}
	if got := agent.Sessions.GetSummary("summary-alias"); got != "有效对话摘要" {
		t.Fatalf("摘要不应降级为截取原文: %q", got)
	}
	if got := len(agent.Sessions.GetHistory("summary-alias")); got != 4 {
		t.Fatalf("应保留最近两轮对话，实际消息数 %d", got)
	}
}

func TestSelectedModelConfigPreservesThinkingIdentity(t *testing.T) {
	cfg := summaryModelConfig(t, "claude-sonnet-4-6")
	for _, level := range []string{"low", "medium", "high"} {
		mc := *cfg.ModelList[0]
		mc.ThinkingLevel = level
		cfg.ModelList = append(cfg.ModelList, &mc)
	}
	cfg.ModelList = cfg.ModelList[1:]
	selected := *cfg.ModelList[2]
	selected.Provider, selected.Model = providers.ExtractProtocol(&selected)
	p := &summaryModelProvider{}
	agent := NewAgentInstance(&config.AgentConfig{ID: "main"}, &cfg.Agents.Defaults, cfg, p, &selected)
	defer agent.Close()
	resolved, err := resolvedCandidateModelConfig(cfg, agent.Candidates[0], agent.Workspace)
	if err != nil || resolved.ThinkingLevel != "high" || agent.ThinkingLevel != ThinkingHigh {
		t.Fatalf("相同模型和端点的候选仍须保留完整配置身份: %+v, %v", resolved, err)
	}
	if agent.Provider != p || agent.Model != "chat" || cfg.ModelList[2].Model != "anthropic/claude-sonnet-4-6" {
		t.Fatal("应沿用注入的 provider，并保留原始模型配置")
	}
}
