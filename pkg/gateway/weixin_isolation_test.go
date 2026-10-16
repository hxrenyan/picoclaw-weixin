package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestWeixinIsolationPreservesModelAliasAcrossReload(t *testing.T) {
	for _, protocol := range []string{"anthropic", "anthropic-messages"} {
		t.Run(protocol, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("解析模型请求失败: %v", err)
				}
				if request.Model != "claude-sonnet-4-6" {
					t.Errorf("应发送原始模型 ID，实际为 %q", request.Model)
				}
				w.Header().Set("Content-Type", "application/json")
				if protocol == "anthropic-messages" {
					if r.URL.Path != "/v1/messages" {
						t.Errorf("模型协议丢失，实际请求路径 %q", r.URL.Path)
					}
					fmt.Fprint(w, `{"id":"test","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"隔离 Bot 正常回复"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
					return
				}
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"隔离 Bot 正常回复"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			cfg := config.DefaultConfig()
			cfg.Agents.Defaults.Workspace = filepath.Join(t.TempDir(), "shared")
			cfg.Agents.Defaults.Provider = ""
			cfg.Agents.Defaults.ModelName = "chat"
			cfg.Heartbeat.Enabled = false
			cfg.Devices.Enabled = false
			cfg.Tools.MediaCleanup.Enabled = false
			cfg.ModelList = []*config.ModelConfig{{
				ModelName: "chat", Model: protocol + "/claude-sonnet-4-6",
				APIBase: server.URL + "/v1", APIKeys: config.SimpleSecureStrings("test-key"),
			}}
			cfg.Channels = config.ChannelsConfig{"weixin": {
				Enabled: true, Type: config.ChannelWeixin,
				Settings: config.RawNode(`{"isolate_accounts":true,"token":"test-token","account_id":"test-bot"}`),
			}}
			primary, selected, err := createStartupProvider(cfg, false)
			if err != nil {
				t.Fatal(err)
			}
			msgBus := bus.NewMessageBus()
			defer msgBus.Close()
			al := agent.NewAgentLoop(cfg, msgBus, primary, agent.WithPrimaryModelConfig(selected))
			defer al.Close()
			cm, err := channels.NewManager(cfg, msgBus, nil)
			if err != nil {
				t.Fatal(err)
			}
			running := &services{ChannelManager: cm}
			defer stopAndCleanupServices(running, serviceShutdownTimeout, false)
			assertReply := func() *agent.AgentInstance {
				t.Helper()
				ac, _ := cfg.ResolveWeixinIsolation("weixin", "default")
				isolated, ok := al.GetRegistry().GetAgent(ac.ID)
				if !ok || !isolated.Isolated || isolated.Provider == primary {
					t.Fatal("应按原始配置创建独立的隔离 provider")
				}
				response, err := isolated.Provider.Chat(context.Background(), []providers.Message{
					{Role: "user", Content: "你好"},
				}, nil, isolated.Candidates[0].Model, map[string]any{"max_tokens": 32})
				if err != nil {
					t.Fatalf("非 OpenAI 别名应正常响应: %v", err)
				}
				if response.Content != "隔离 Bot 正常回复" || isolated.Candidates[0].Provider != protocol {
					t.Fatalf("隔离实例的协议或响应错误: %+v, %+v", isolated.Candidates, response)
				}
				return isolated
			}
			before := assertReply()
			if err := handleConfigReload(context.Background(), al, cfg, &primary, running, msgBus, false, true); err != nil {
				t.Fatal(err)
			}
			after := assertReply()
			if before.Provider == after.Provider || cfg.Agents.Defaults.ModelName != "chat" {
				t.Fatal("重载应保留模型别名并重新创建隔离 provider")
			}
		})
	}
}
