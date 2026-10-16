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

func TestStartupProviderKeepsSelectedModelAcrossReload(t *testing.T) {
	for _, sameModel := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_model_%t", sameModel), func(t *testing.T) {
			type request struct {
				Model string `json:"model"`
				Path  string
				Auth  string
			}
			requests := make(chan request, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req request
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("解析模型请求失败: %v", err)
				}
				req.Path, req.Auth = r.URL.Path, r.Header.Get("Authorization")
				requests <- req
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"正常回复"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			cfg := config.DefaultConfig()
			cfg.Agents.Defaults.Workspace = filepath.Join(t.TempDir(), "shared")
			cfg.Agents.Defaults.ModelName = "chat"
			cfg.Agents.Defaults.Provider = ""
			cfg.Channels = config.ChannelsConfig{}
			cfg.ModelList = nil
			cfg.Heartbeat.Enabled = false
			cfg.Devices.Enabled = false
			cfg.Tools.MediaCleanup.Enabled = false
			for i, thinking := range []string{"low", "medium", "high"} {
				model := fmt.Sprintf("model-%d", i)
				if sameModel {
					model = "same-model"
				}
				cfg.ModelList = append(cfg.ModelList, &config.ModelConfig{
					ModelName: "chat", Model: "openai/" + model,
					APIBase: fmt.Sprintf("%s/endpoint-%d/v1", server.URL, i),
					APIKeys: config.SimpleSecureStrings(fmt.Sprintf("key-%d", i)), ThinkingLevel: thinking,
				})
			}
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
			assertReply := func() {
				t.Helper()
				active := al.GetRegistry().GetDefaultAgent()
				mc := selected
				if mc == nil {
					// 重载后根据候选身份查出唯一端点，不再轮询别名。
					for _, candidate := range cfg.ModelList {
						if candidate.ThinkingLevel == string(active.ThinkingLevel) {
							mc = candidate
						}
					}
				}
				if mc == nil {
					t.Fatal("未找到当前候选配置")
				}
				_, modelID := providers.ExtractProtocol(mc)
				if active.Provider != primary || active.Model != "chat" || active.Candidates[0].Model != modelID ||
					string(active.ThinkingLevel) != mc.ThinkingLevel {
					t.Fatalf("provider、模型与思考设置没有沿用启动选择: %+v, %+v", active.Candidates, mc)
				}
				for _, content := range []string{"你好", "/btw 你好"} {
					response, err := al.ProcessDirect(context.Background(), content, "shared-model-selection")
					if err != nil || response != "正常回复" {
						t.Fatalf("请求 %q 失败: %q, %v", content, response, err)
					}
					req := <-requests
					if req.Model != modelID || req.Auth != "Bearer "+mc.APIKey() ||
						req.Path != mc.APIBase[len(server.URL):]+"/chat/completions" {
						t.Fatalf("请求 %q 的模型 ID、端点或凭据与候选不一致: %+v, %+v", content, req, mc)
					}
				}
			}
			assertReply()
			selected = nil
			if err := handleConfigReload(context.Background(), al, cfg, &primary, running, msgBus, false, true); err != nil {
				t.Fatal(err)
			}
			assertReply()
		})
	}
}
