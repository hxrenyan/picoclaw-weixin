package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolationTestConfig(t *testing.T) (*Config, *WeixinSettings) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Agents.Defaults.Workspace = filepath.Join(t.TempDir(), "workspace")
	bc := &Channel{Enabled: true, Type: ChannelWeixin}
	settings := &WeixinSettings{}
	if err := bc.Decode(settings); err != nil {
		t.Fatal(err)
	}
	settings.IsolateAccounts = true
	settings.SetToken("default-token")
	settings.AccountID = "default-bot"
	settings.Accounts = map[string]WeixinAccountSettings{
		"alice": {Token: *NewSecureString("alice-token"), AccountID: "alice-bot"},
		"bob":   {Token: *NewSecureString("bob-token"), AccountID: "bob-bot"},
	}
	cfg.Channels = ChannelsConfig{"weixin": bc}
	return cfg, settings
}

func TestWeixinIsolationNormalizesWorkspace(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	for _, workspace := range []string{filepath.Join(base, "shared") + string(filepath.Separator), ".", "shared/../shared/"} {
		t.Run(workspace, func(t *testing.T) {
			cfg, _ := isolationTestConfig(t)
			cfg.Agents.Defaults.Workspace = workspace
			agent, required := cfg.ResolveWeixinIsolation("weixin", "alice")
			if !required || agent == nil || !filepath.IsAbs(agent.Workspace) {
				t.Fatalf("必须创建使用绝对目录的隔离 Agent: %+v", agent)
			}
			shared, err := filepath.Abs(workspace)
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(filepath.Dir(shared), "weixin-users", agent.ID)
			if agent.Workspace != want {
				t.Fatalf("隔离目录 = %q，期望 %q", agent.Workspace, want)
			}
			rel, err := filepath.Rel(shared, agent.Workspace)
			if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				t.Fatal("隔离目录不能嵌套在共享 workspace 内")
			}
		})
	}
	for _, workspace := range []string{string(filepath.Separator), filepath.Join(base, "weixin-users")} {
		cfg, _ := isolationTestConfig(t)
		cfg.Agents.Defaults.Workspace = workspace
		if agent, required := cfg.ResolveWeixinIsolation("weixin", "alice"); agent != nil || !required {
			t.Fatalf("无法派生独立目录时必须拒绝共享回退: %s", workspace)
		}
		if len(cfg.IsolatedWeixinAgents()) != 0 {
			t.Fatal("目录重叠时不能创建隔离 Agent")
		}
	}
}

func TestWeixinIsolationRejectsSharedAgentWorkspaceOverlap(t *testing.T) {
	base := t.TempDir()
	cfg, _ := isolationTestConfig(t)
	cfg.Agents.Defaults.Workspace = filepath.Join(base, "workspace")
	cfg.Agents.List = []AgentConfig{{
		ID:        "main",
		Default:   true,
		Workspace: base,
	}}

	if agent, required := cfg.ResolveWeixinIsolation("weixin", "alice"); agent != nil || !required {
		t.Fatalf("共享 Agent 覆盖到隔离目录祖先时必须拒绝隔离: %+v, %v", agent, required)
	}
	if len(cfg.IsolatedWeixinAgents()) != 0 {
		t.Fatal("共享 Agent 的实际 workspace 覆盖隔离目录时不能注册隔离实例")
	}
}

func TestWeixinIsolationRejectsRealWorkspaceOverlap(t *testing.T) {
	for _, scenario := range []string{
		"共享目录链接到父目录", "共享目录经多层链接指向父目录", "派生根链接到共享目录",
		"派生根链接到共享目录内", "派生根链接到共享目录祖先", "用户目录链接到共享目录",
		"派生根经大小写别名指向共享目录", "派生根经大小写别名指向共享目录内", "共享目录经大小写别名位于派生根内",
		"未创建目录仅大小写不同", "多层未创建目录仅大小写不同",
		"用户目录链接到其他用户", "共享目录含断链祖先", "派生目录含断链祖先", "共享目录链接循环",
		"派生根为普通文件",
	} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			shared := filepath.Join(base, "shared")
			derived := filepath.Join(base, "weixin-users")
			id := WeixinIsolationAgentID("weixin", "alice", "alice-bot", "", "alice-token")
			mkdir := func(path string) {
				t.Helper()
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			link := func(target, path string) {
				t.Helper()
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			requireCaseAlias := func(path, alias string) {
				t.Helper()
				original, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				other, err := os.Stat(alias)
				if os.IsNotExist(err) {
					t.Skip("当前文件系统区分大小写")
				}
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(original, other) {
					t.Skip("当前文件系统的大小写路径不是同一目录")
				}
			}
			switch scenario {
			case "共享目录链接到父目录":
				link(".", shared)
			case "共享目录经多层链接指向父目录":
				link(".", filepath.Join(base, "alias"))
				link("alias", shared)
			case "派生根链接到共享目录":
				mkdir(shared)
				link("shared", derived)
			case "派生根链接到共享目录内":
				mkdir(filepath.Join(shared, "users"))
				link(filepath.Join("shared", "users"), derived)
			case "派生根链接到共享目录祖先":
				mkdir(shared)
				link(".", derived)
			case "派生根经大小写别名指向共享目录":
				mkdir(shared)
				requireCaseAlias(shared, filepath.Join(base, "SHARED"))
				link("SHARED", derived)
			case "派生根经大小写别名指向共享目录内":
				mkdir(filepath.Join(shared, "users"))
				requireCaseAlias(shared, filepath.Join(base, "SHARED"))
				link(filepath.Join("SHARED", "users"), derived)
			case "共享目录经大小写别名位于派生根内":
				mkdir(filepath.Join(derived, "shared"))
				requireCaseAlias(derived, filepath.Join(base, "WEIXIN-USERS"))
				link(filepath.Join("WEIXIN-USERS", "shared"), shared)
			case "未创建目录仅大小写不同":
				shared = filepath.Join(base, "WEIXIN-USERS")
			case "多层未创建目录仅大小写不同":
				shared = filepath.Join(base, "missing", "nested", "WEIXIN-USERS")
			case "用户目录链接到共享目录":
				mkdir(shared)
				mkdir(derived)
				link(shared, filepath.Join(derived, id))
			case "用户目录链接到其他用户":
				mkdir(shared)
				otherID := WeixinIsolationAgentID("weixin", "bob", "bob-bot", "", "bob-token")
				mkdir(filepath.Join(derived, otherID))
				link(otherID, filepath.Join(derived, id))
			case "共享目录含断链祖先":
				link("missing", filepath.Join(base, "broken"))
				shared = filepath.Join(base, "broken", "nested", "shared")
			case "派生目录含断链祖先":
				mkdir(shared)
				link("missing", derived)
			case "共享目录链接循环":
				link("shared", shared)
			case "派生根为普通文件":
				mkdir(shared)
				if err := os.WriteFile(derived, []byte("不是目录"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, _ := isolationTestConfig(t)
			cfg.Agents.Defaults.Workspace = shared
			if agent, required := cfg.ResolveWeixinIsolation("weixin", "alice"); agent != nil || !required {
				t.Fatalf("真实目录无法安全隔离时必须拒绝共享回退: %+v, %v", agent, required)
			}
			for _, agent := range cfg.IsolatedWeixinAgents() {
				if agent.ID == id {
					t.Fatal("不能注册目录边界不安全的隔离 Agent")
				}
			}
		})
	}
}

func TestWeixinIsolationAllowsSeparateRealWorkspaces(t *testing.T) {
	for _, scenario := range []string{
		"尚未创建多层目录", "共享目录链接到独立目录", "派生根链接到独立目录", "祖先目录为符号链接", "已有大小写不同的独立目录",
	} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			shared := filepath.Join(base, "shared")
			realShared := shared
			realDerived := filepath.Join(base, "weixin-users")
			if scenario != "尚未创建多层目录" && scenario != "已有大小写不同的独立目录" {
				if err := os.MkdirAll(shared, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "尚未创建多层目录":
				shared = filepath.Join(base, "missing", "nested", "shared")
				realShared, realDerived = shared, filepath.Join(filepath.Dir(shared), "weixin-users")
			case "共享目录链接到独立目录":
				realShared, shared = shared, filepath.Join(base, "alias")
				if err := os.Symlink("shared", shared); err != nil {
					t.Fatal(err)
				}
			case "派生根链接到独立目录":
				realDerived = filepath.Join(base, "private-users")
				if err := os.MkdirAll(realDerived, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("private-users", filepath.Join(base, "weixin-users")); err != nil {
					t.Fatal(err)
				}
			case "祖先目录为符号链接":
				if err := os.Symlink(".", filepath.Join(base, "alias")); err != nil {
					t.Fatal(err)
				}
				shared = filepath.Join(base, "alias", "shared")
			case "已有大小写不同的独立目录":
				shared = filepath.Join(base, "WEIXIN-USERS")
				realShared = shared
				for _, path := range []string{shared, realDerived} {
					if err := os.MkdirAll(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				sharedInfo, err := os.Stat(shared)
				if err != nil {
					t.Fatal(err)
				}
				derivedInfo, err := os.Stat(realDerived)
				if err != nil {
					t.Fatal(err)
				}
				if os.SameFile(sharedInfo, derivedInfo) {
					t.Skip("当前文件系统不支持仅大小写不同的独立目录")
				}
			}
			cfg, _ := isolationTestConfig(t)
			cfg.Agents.Defaults.Workspace = shared
			agent, required := cfg.ResolveWeixinIsolation("weixin", "alice")
			if agent == nil || !required {
				t.Fatal("真实路径互不重叠时应保留隔离配置")
			}
			want := filepath.Join(filepath.Dir(shared), "weixin-users", agent.ID)
			if agent.Workspace != want {
				t.Fatalf("不应迁移现有隔离目录: %q，期望 %q", agent.Workspace, want)
			}
			if err := os.MkdirAll(realShared, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(agent.Workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			secret := filepath.Join(agent.Workspace, "MEMORY.md")
			if err := os.WriteFile(secret, []byte("Alice 的私密记忆"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(shared)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			rel, err := filepath.Rel(realShared, filepath.Join(realDerived, agent.ID, "MEMORY.md"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := root.ReadFile(rel); err == nil {
				t.Fatal("共享目录的文件沙箱不能读取隔离用户的记忆")
			}
			again, required := cfg.ResolveWeixinIsolation("weixin", "alice")
			if !required || again == nil || again.Workspace != agent.Workspace {
				t.Fatal("目录创建后应继续解析到原隔离目录")
			}
		})
	}
}

func TestWeixinIsolationPreservesChannelKeys(t *testing.T) {
	cfg, _ := isolationTestConfig(t)
	cfg.Channels["WeixinBusiness"] = cfg.Channels["weixin"]
	cfg.Channels["weixinbusiness"] = cfg.Channels["weixin"]
	upper, required := cfg.ResolveWeixinIsolation("WeixinBusiness", "alice")
	lower, _ := cfg.ResolveWeixinIsolation("weixinbusiness", "alice")
	if !required || upper == nil || lower == nil || upper.ID == lower.ID || upper.Workspace == lower.Workspace {
		t.Fatal("大小写不同的配置键必须分配不同的隔离 Agent")
	}
	if len(cfg.IsolatedWeixinAgents()) != 9 {
		t.Fatal("必须为全部原始渠道键创建隔离 Agent")
	}
	delete(cfg.Channels, "weixinbusiness")
	if agent, required := cfg.ResolveWeixinIsolation("weixinbusiness", "alice"); agent != nil || required {
		t.Fatal("渠道查询必须精确匹配配置键")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got, required := loaded.ResolveWeixinIsolation("WeixinBusiness", "alice")
	if !required || got == nil || got.ID != upper.ID {
		t.Fatal("大写渠道保存并加载后必须保留隔离 Agent")
	}
}

func TestWeixinIsolationSeparatesBotsAndChannels(t *testing.T) {
	cfg, settings := isolationTestConfig(t)
	alice, required := cfg.ResolveWeixinIsolation("weixin", "alice")
	bob, _ := cfg.ResolveWeixinIsolation("weixin", "bob")
	if !required || alice == nil || bob == nil || !alice.Isolated || alice.ID == bob.ID || alice.Workspace == bob.Workspace {
		t.Fatal("不同 Bot 必须分配独立 Agent 和目录")
	}
	if filepath.Dir(alice.Workspace) == cfg.WorkspacePath() {
		t.Fatal("独立目录不应放进共享 Agent 的工作目录")
	}
	cfg.Channels["other"] = cfg.Channels["weixin"]
	other, _ := cfg.ResolveWeixinIsolation("other", "alice")
	if alice.ID == other.ID || alice.Workspace == other.Workspace {
		t.Fatal("不同渠道的相同账号不能共用 Agent")
	}
	account := settings.Accounts["alice"]
	account.Token = *NewSecureString("refreshed-token")
	settings.Accounts["alice"] = account
	refreshed, _ := cfg.ResolveWeixinIsolation("weixin", "alice")
	if refreshed.ID != alice.ID || refreshed.Workspace != alice.Workspace {
		t.Fatal("同一个微信 Bot 刷新令牌应保留记忆目录")
	}
	account.AccountID = "another-person-bot"
	settings.Accounts["alice"] = account
	rebound, _ := cfg.ResolveWeixinIsolation("weixin", "alice")
	if rebound.ID == alice.ID || rebound.Workspace == alice.Workspace {
		t.Fatal("换绑其他微信身份不能继承旧记忆")
	}
}

func TestWeixinIsolationRejectsUnavailableAccounts(t *testing.T) {
	cfg, settings := isolationTestConfig(t)
	no := false
	settings.Accounts["disabled"] = WeixinAccountSettings{Token: *NewSecureString("disabled-token"), Enabled: &no}
	settings.Accounts["unbound"] = WeixinAccountSettings{}
	for _, account := range []string{"missing", "disabled", "unbound"} {
		agent, required := cfg.ResolveWeixinIsolation("weixin", account)
		if agent != nil || !required {
			t.Fatalf("账号 %s 必须拒绝共享回退", account)
		}
	}
	agents := cfg.IsolatedWeixinAgents()
	if len(agents) != 3 {
		t.Fatalf("只应创建默认和两个已绑定 Bot，得到 %d 个", len(agents))
	}
	defaultAgent, _ := cfg.ResolveWeixinIsolation("weixin", "")
	explicitDefault, _ := cfg.ResolveWeixinIsolation("weixin", "default")
	if defaultAgent.ID != explicitDefault.ID {
		t.Fatal("默认账号归一化不一致")
	}
	cfg.Channels["weixin"].Enabled = false
	if agent, required := cfg.ResolveWeixinIsolation("weixin", "alice"); agent != nil || !required {
		t.Fatal("禁用渠道必须拒绝共享回退")
	}
	settings.IsolateAccounts = false
	if agent, required := cfg.ResolveWeixinIsolation("weixin", "alice"); agent != nil || required {
		t.Fatal("旧配置应保持原路由")
	}
}

func TestWeixinIsolationSurvivesConfigRoundTrip(t *testing.T) {
	cfg, _ := isolationTestConfig(t)
	want, _ := cfg.ResolveWeixinIsolation("weixin", "alice")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got, required := loaded.ResolveWeixinIsolation("weixin", "alice")
	if !required || got == nil || got.ID != want.ID || got.Workspace != want.Workspace {
		t.Fatal("重启后隔离配置或目录发生变化")
	}
}
