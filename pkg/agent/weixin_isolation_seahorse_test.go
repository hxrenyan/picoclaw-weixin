//go:build !mipsle && !netbsd && !(freebsd && arm)

package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/seahorse"
)

func TestWeixinIsolationSeahorseDoesNotIngestRejectedMessage(t *testing.T) {
	_, cfg := isolatedRegistry(t)
	cfg.Agents.Defaults.ContextManager = "seahorse"
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockRegistryProvider{})
	t.Cleanup(al.Close)
	cm, ok := al.contextManager.(*seahorseContextManager)
	if !ok {
		t.Fatalf("应实际启动 Seahorse 后端，得到 %T", al.contextManager)
	}
	t.Cleanup(func() { _ = cm.engine.Close() })
	ctx := context.Background()
	msg := bus.InboundMessage{
		Context: bus.InboundContext{Channel: "weixin", Account: "alice", ChatID: "user", SenderID: "user", ChatType: "direct"},
		Content: "isolationleakprobe",
	}
	assertRejected := func() {
		t.Helper()
		if _, err := al.processMessage(ctx, msg); err == nil {
			t.Fatal("共享 Seahorse 后端必须拒绝隔离消息")
		}
		got, err := cm.engine.GetRetrieval().Grep(ctx, seahorse.GrepInput{
			Pattern: "isolationleakprobe", Scope: "message", AllConversations: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Messages) != 0 || len(al.registry.GetDefaultAgent().Sessions.ListSessions()) != 0 {
			t.Fatal("被拒绝的隔离消息不能进入共享数据库或会话文件")
		}
	}
	assertRejected()
	// 更改配置并重载后，原共享引擎仍在运行，必须继续拒绝。
	cfg.Agents.Defaults.ContextManager = "legacy"
	if err := al.ReloadProviderAndConfig(ctx, &mockRegistryProvider{}, cfg); err != nil {
		t.Fatal(err)
	}
	assertRejected()
}
