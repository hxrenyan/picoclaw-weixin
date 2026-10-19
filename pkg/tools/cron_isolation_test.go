package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
	"github.com/sipeed/picoclaw/pkg/session"
)

type rebindingCronExecutor struct {
	stubJobExecutor
	rebind func()
}

func (e *rebindingCronExecutor) ProcessDirectWithIsolation(ctx context.Context, content, key, channel, chatID, account, identity string) (string, error) {
	e.rebind()
	return "旧身份私密回复", nil
}

func TestCronTool_IsolatedRebindDuringExecution(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = filepath.Join(t.TempDir(), "shared")
	cfg.Channels = config.ChannelsConfig{"weixin": {Enabled: true, Type: config.ChannelWeixin, Settings: config.RawNode(`{"isolate_accounts":true,"accounts":{"alice":{"token":"a","account_id":"bot-a"}}}`)}}
	ctx := isolatedCronContext(t, cfg, "alice")
	executor := &rebindingCronExecutor{rebind: func() { cfg.Channels = config.ChannelsConfig{} }}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, cfg)
	if result := tool.Execute(ctx, map[string]any{"action": "add", "message": "提醒", "at_seconds": float64(600)}); result.IsError {
		t.Fatal(result.ForLLM)
	}
	job := tool.cronService.ListJobs(false)[0]
	if result := tool.ExecuteJob(context.Background(), &job); !strings.HasPrefix(result, "Error:") {
		t.Fatalf("执行中换绑应拒绝响应: %s", result)
	}
	if executor.publishedResp != "" {
		t.Fatal("旧身份响应被投递")
	}
}

func isolatedCronContext(t *testing.T, cfg *config.Config, account string) context.Context {
	t.Helper()
	agent, _ := cfg.ResolveWeixinIsolation("weixin", account)
	if agent == nil {
		t.Fatal("未生成隔离身份")
	}
	ctx := WithToolContext(context.Background(), "weixin", "same-chat")
	return WithToolSessionContext(ctx, agent.ID, "test", &session.SessionScope{AgentID: agent.ID, Channel: "weixin", Account: account})
}

func TestCronTool_IsolatedOwnershipAndCommands(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = filepath.Join(t.TempDir(), "shared")
	cfg.Tools.Cron.CommandAllowedRemotes = []string{"*"}
	cfg.Channels = config.ChannelsConfig{"weixin": {Enabled: true, Type: config.ChannelWeixin, Settings: config.RawNode(`{"isolate_accounts":true,"accounts":{"alice":{"token":"a","account_id":"bot-a"},"bob":{"token":"b","account_id":"bot-b"}}}`)}}
	tool := newTestCronToolWithConfig(t, cfg)
	storePath := filepath.Join(t.TempDir(), "jobs.json")
	tool.cronService = cron.NewCronService(storePath, nil)
	alice, bob := isolatedCronContext(t, cfg, "alice"), isolatedCronContext(t, cfg, "bob")
	if r := tool.Execute(alice, map[string]any{"action": "add", "message": "私密提醒", "at_seconds": float64(600)}); r.IsError {
		t.Fatal(r.ForLLM)
	}
	jobs := tool.cronService.ListJobs(false)
	if len(jobs) != 1 {
		t.Fatalf("任务数 = %d", len(jobs))
	}
	job := jobs[0]
	if job.Payload.Account != "alice" || job.Payload.IsolationAgentID != ToolAgentID(alice) {
		t.Fatal("任务缺少可信身份")
	}
	restored := cron.NewCronService(storePath, nil)
	saved, ok := restored.GetJob(job.ID)
	if !ok || saved.Payload != job.Payload {
		t.Fatal("身份未持久化")
	}
	if r := tool.Execute(bob, map[string]any{"action": "list"}); strings.Contains(r.ForLLM, "私密提醒") {
		t.Fatal("跨账号泄漏任务")
	}
	for _, action := range []string{"get", "update", "remove", "enable", "disable"} {
		if r := tool.Execute(bob, map[string]any{"action": action, "job_id": job.ID, "message": "篡改"}); !r.IsError {
			t.Fatalf("跨账号操作成功: %s", action)
		}
	}
	for _, action := range []string{"add", "update"} {
		r := tool.Execute(alice, map[string]any{"action": action, "job_id": job.ID, "message": "命令", "at_seconds": float64(600), "command": "echo unsafe", "command_confirm": true})
		if !r.IsError {
			t.Fatalf("隔离账号绕过命令限制: %s", action)
		}
	}
	if r := tool.Execute(alice, map[string]any{"action": "update", "job_id": job.ID, "message": "新提醒"}); r.IsError {
		t.Fatal(r.ForLLM)
	}
	job.Payload.Command = "echo unsafe"
	if !strings.HasPrefix(tool.ExecuteJob(context.Background(), &job), "Error:") {
		t.Fatal("不应执行隔离命令任务")
	}
	job.Payload.Command = ""
	originalChannels := cfg.Channels
	for _, settings := range []string{
		`{"isolate_accounts":true,"accounts":{}}`,
		`{"isolate_accounts":true,"accounts":{"alice":{"enabled":false,"token":"a","account_id":"bot-a"}}}`,
	} {
		cfg.Channels = config.ChannelsConfig{"weixin": {Enabled: true, Type: config.ChannelWeixin, Settings: config.RawNode(settings)}}
		if !strings.HasPrefix(tool.ExecuteJob(context.Background(), &job), "Error:") {
			t.Fatal("账号删除或禁用后仍执行任务")
		}
	}
	cfg.Channels = originalChannels
	legacy := job
	legacy.Payload.IsolationAgentID = ""
	if !strings.HasPrefix(tool.ExecuteJob(context.Background(), &legacy), "Error:") {
		t.Fatal("未绑定旧任务不应迁入隔离身份")
	}
	tool.executor = &stubJobExecutor{response: "不应回落共享执行器"}
	if !strings.Contains(tool.ExecuteJob(context.Background(), &job), "does not support isolated") {
		t.Fatal("隔离任务不应回落普通执行器")
	}
	cfg.Channels = config.ChannelsConfig{"weixin": {Enabled: true, Type: config.ChannelWeixin, Settings: config.RawNode(`{"isolate_accounts":true,"accounts":{"alice":{"token":"new","account_id":"new-bot"}}}`)}}
	if !tool.Execute(alice, map[string]any{"action": "list"}).IsError {
		t.Fatal("旧回合仍可访问任务")
	}
	current := isolatedCronContext(t, cfg, "alice")
	if !tool.Execute(current, map[string]any{"action": "get", "job_id": job.ID}).IsError {
		t.Fatal("换绑后可访问旧任务")
	}
	if !strings.HasPrefix(tool.ExecuteJob(context.Background(), &job), "Error:") {
		t.Fatal("换绑后执行了旧任务")
	}
	cfg.Channels = config.ChannelsConfig{"weixin": {Enabled: true, Type: config.ChannelWeixin, Settings: config.RawNode(`{"isolate_accounts":false}`)}}
	if !strings.HasPrefix(tool.ExecuteJob(context.Background(), &job), "Error:") {
		t.Fatal("关闭隔离后旧任务回落共享")
	}
}
