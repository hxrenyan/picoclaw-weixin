package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestWeixinIsolationRejectsForgedForeignSession(t *testing.T) {
	for _, content := range []string{"继续正常对话", "/btw 查看当前上下文", "/clear"} {
		t.Run(content, func(t *testing.T) {
			_, cfg := isolatedRegistry(t)
			al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockRegistryProvider{})
			t.Cleanup(al.Close)
			attacker, victim := isolatedBot(t, al.registry, "alice"), isolatedBot(t, al.registry, "bob")
			victimAccount := "bob"
			if attacker.ID > victim.ID {
				attacker, victim = victim, attacker
				victimAccount = "alice"
			}
			capture := &recordingProvider{}
			victim.Provider, victim.CandidateProviders, victim.Candidates = capture, nil, nil
			victim.ContextWindow = 100000
			msg := bus.InboundMessage{
				Context: bus.InboundContext{
					Channel: "weixin", Account: victimAccount, ChatID: "victim-user", SenderID: "victim-user", ChatType: "direct",
				},
				Content: "受害者自己的历史",
			}
			if _, err := al.processMessage(context.Background(), msg); err != nil {
				t.Fatal(err)
			}
			key := al.allocateRouteSession(al.registry.ResolveRoute(msg.Context), msg).SessionKey
			victim.Sessions.SetSummary(key, "受害者自己的摘要")
			meta, err := json.Marshal(map[string]any{
				"key": key, "count": 1, "summary": "攻击者伪造的摘要", "scope": map[string]any{"agent_id": attacker.ID},
			})
			if err != nil {
				t.Fatal(err)
			}
			write, _ := attacker.Tools.Get("write_file")
			for path, data := range map[string]string{
				filepath.Join("sessions", key+".meta.json"): string(meta),
				filepath.Join("sessions", key+".jsonl"):     "{\"role\":\"user\",\"content\":\"攻击者伪造的历史\"}\n",
			} {
				if result := write.Execute(context.Background(), map[string]any{"path": path, "content": data}); result.IsError {
					t.Fatal(result.ForLLM)
				}
			}
			msg.Content = content
			if _, err := al.processMessage(context.Background(), msg); err != nil {
				t.Fatal(err)
			}
			if content == "/clear" {
				if len(victim.Sessions.GetHistory(key)) != 0 || victim.Sessions.GetSummary(key) != "" {
					t.Fatal("清空命令必须操作路由指定的用户会话")
				}
				if len(attacker.Sessions.GetHistory(key)) != 1 {
					t.Fatal("清空命令不能修改其他用户的存储")
				}
				return
			}
			var prompt strings.Builder
			for _, message := range capture.lastMessages {
				prompt.WriteString(message.Content)
			}
			for _, want := range []string{"受害者自己的历史", "受害者自己的摘要"} {
				if strings.HasPrefix(content, "/btw ") && want == "受害者自己的摘要" {
					// /btw 的默认配置省略系统提示词及其中的摘要。
					continue
				}
				if !strings.Contains(prompt.String(), want) {
					t.Fatalf("提示词缺少所属用户的上下文: %s", want)
				}
			}
			if strings.Contains(prompt.String(), "攻击者伪造") {
				t.Fatal("其他用户可写的元数据不能改变当前会话的存储归属")
			}
		})
	}
}

func TestWeixinIsolationDisablesNativeSearch(t *testing.T) {
	for _, scenario := range []string{"普通调用", "钩子尝试开启", "回退模型"} {
		t.Run(scenario, func(t *testing.T) {
			_, cfg := isolatedRegistry(t)
			cfg.Tools.Web.Enabled, cfg.Tools.Web.PreferNative = true, true
			al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockRegistryProvider{})
			t.Cleanup(al.Close)
			alice := isolatedBot(t, al.registry, "alice")
			capture := &nativeSearchCaptureProvider{}
			alice.Provider, alice.CandidateProviders, alice.Candidates = capture, nil, nil
			alice.ContextWindow = 100000
			if alice.Tools.HasRegistered("web_search") {
				t.Fatal("隔离白名单应拒绝注册网络搜索工具")
			}
			if scenario == "回退模型" {
				alice.Model = "primary-model"
				alice.Provider = &errorProvider{errType: "connection_reset"}
				alice.Candidates = []providers.FallbackCandidate{
					{Provider: "openai", Model: "primary-model"},
					{Provider: "openai", Model: "fallback-model"},
				}
				alice.CandidateProviders = map[string]providers.LLMProvider{
					candidateProviderKey(alice.Candidates[1]): capture,
				}
				al.fallback = providers.NewFallbackChain(providers.NewCooldownTracker(), nil)
			}
			if scenario == "钩子尝试开启" {
				if err := al.MountHook(NamedHook("enable-native-search", turnProfileEnableNativeSearchHook{})); err != nil {
					t.Fatal(err)
				}
			}
			_, err := al.processMessage(context.Background(), bus.InboundMessage{
				Context: bus.InboundContext{Channel: "weixin", Account: "alice", ChatID: "user", SenderID: "user", ChatType: "direct"},
				Content: "搜索外网",
			})
			if err != nil {
				t.Fatal(err)
			}
			if capture.lastOpts == nil {
				t.Fatal("模型没有收到调用")
			}
			if capture.lastOpts["native_search"] == true {
				t.Fatal("隔离账号不能通过模型原生工具使用网络搜索")
			}
			if turnProfileNativeSearchCallable(cfg, config.EffectiveTurnProfile{}, alice) {
				t.Fatal("隔离提示词不能声明原生搜索可用")
			}
		})
	}
}

func TestWeixinIsolationImageInputChecksOwnerAndSize(t *testing.T) {
	r, _ := isolatedRegistry(t)
	alice, bob := isolatedBot(t, r, "alice"), isolatedBot(t, r, "bob")
	store := media.NewFileMediaStore()
	src := filepath.Join(t.TempDir(), "photo.png")
	png := []byte{137, 80, 78, 71, 13, 10, 26, 10}
	if err := os.WriteFile(src, png, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name             string
		owner            string
		maxSize          int
		currentTurnStart int
		wantImage        bool
	}{
		{name: "本人图片", owner: alice.ID, maxSize: 1024, wantImage: true},
		{name: "其他账号", owner: bob.ID, maxSize: 1024},
		{name: "无归属", maxSize: 1024},
		{name: "旧绑定", owner: "previous-binding", maxSize: 1024},
		{name: "超过大小限制", owner: alice.ID, maxSize: len(png) - 1},
		{name: "历史图片", owner: alice.ID, maxSize: 1024, currentTurnStart: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ref, err := store.Store(src, media.MediaMeta{ContentType: "image/png", OwnerAgentID: scenario.owner}, scenario.name)
			if err != nil {
				t.Fatal(err)
			}
			messages := []providers.Message{{Role: "user", Content: "[image]", Media: []string{ref}}}
			resolved := resolveMediaRefsForAgent(messages, store, scenario.maxSize, scenario.currentTurnStart, alice)
			if len(resolved) != 1 {
				t.Fatalf("入站图片不应新增消息: %d", len(resolved))
			}
			if hasMediaRefs(resolved) != scenario.wantImage {
				t.Fatalf("视觉输入存在 = %v，预期 %v", hasMediaRefs(resolved), scenario.wantImage)
			}
			if scenario.wantImage && resolved[0].Media[0] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(png) {
				t.Fatal("视觉输入必须包含原图片内容")
			}
			if messages[0].Media[0] != ref || messages[0].Content != "[image]" {
				t.Fatal("解析不能改变持久化前的原始消息")
			}
		})
	}
}

func TestWeixinIsolationImageReachesVisionModel(t *testing.T) {
	_, cfg := isolatedRegistry(t)
	cfg.Agents.Defaults.ModelName = "text-model"
	cfg.Agents.Defaults.ImageModel = "vision-model"
	cfg.ModelList = []*config.ModelConfig{
		{ModelName: "text-model", Model: "openai/text-model", APIBase: "http://127.0.0.1:1"},
		{ModelName: "vision-model", Model: "openai/vision-model", APIBase: "http://127.0.0.1:1"},
	}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockRegistryProvider{})
	t.Cleanup(al.Close)
	alice := isolatedBot(t, al.registry, "alice")
	capture := &recordingProvider{}
	alice.CandidateProviders[candidateProviderKey(alice.ImageCandidates[0])] = capture
	alice.ContextWindow = 100000
	store := media.NewFileMediaStore()
	al.SetMediaStore(store)
	src := filepath.Join(t.TempDir(), "photo.png")
	png := []byte{137, 80, 78, 71, 13, 10, 26, 10}
	if err := os.WriteFile(src, png, 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := store.Store(src, media.MediaMeta{Filename: "photo.png", ContentType: "image/png", OwnerAgentID: alice.ID}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	msg := bus.InboundMessage{
		Context: bus.InboundContext{Channel: "weixin", Account: "alice", ChatID: "user", SenderID: "user", ChatType: "direct"},
		Content: "图片里是什么 [image]", Media: []string{ref},
	}
	if _, err := al.processMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	found := false
	for _, message := range capture.lastMessages {
		for _, input := range message.Media {
			found = found || input == want
		}
	}
	if !found {
		t.Fatal("校验归属后的入站图片必须以视觉输入送入图片模型")
	}
	key := al.allocateRouteSession(al.registry.ResolveRoute(msg.Context), msg).SessionKey
	for _, message := range alice.Sessions.GetHistory(key) {
		for _, input := range message.Media {
			if strings.HasPrefix(input, "data:") {
				t.Fatal("会话历史应保存媒体引用，避免持久化编码后的图片")
			}
		}
	}
	if alice.Tools.HasRegistered("load_image") {
		t.Fatal("视觉输入不应扩大隔离工具白名单")
	}
	msg.Content, msg.Media = "只发送文字的后续回合", nil
	alice.Provider = capture
	if _, err := al.processMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if hasMediaRefs(capture.lastMessages) {
		t.Fatal("后续纯文字回合不能重新编码历史图片")
	}
}
