package agent

import (
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// isolatedToolAllowlist 在工作目录提示词声明之外再收紧权限。
// 开放工作目录内的文件操作、客户端搜索和受身份约束的定时任务，仍拒绝命令、MCP、消息和跨 Agent 工具。
func isolatedToolAllowlist(declared []string) []string {
	safe := []string{"read_file", "write_file", "edit_file", "append_file", "list_dir", "web_search", "cron"}
	if declared == nil {
		return safe
	}
	allowed := make(map[string]bool, len(declared))
	for _, name := range declared {
		allowed[name] = true
	}
	result := make([]string, 0, len(safe))
	for _, name := range safe {
		if allowed[name] {
			result = append(result, name)
		}
	}
	return result
}

// validateWeixinIsolationConfig 只接受按 Agent 分离会话存储的上下文后端。
func validateWeixinIsolationConfig(cfg *config.Config) error {
	if name := cfg.Agents.Defaults.ContextManager; name != "" && name != "legacy" {
		return fmt.Errorf("微信用户隔离仅支持 legacy 上下文后端，不支持 %q", name)
	}
	return validateIsolatedModels(cfg)
}

// validateIsolatedModels 阻止通过内置 CLI/SDK Agent 绕过本进程的工具白名单。
func validateIsolatedModels(cfg *config.Config) error {
	unsafe := func(protocol string) bool {
		switch providers.NormalizeProvider(protocol) {
		case "claude-cli", "codex-cli", "github-copilot":
			return true
		}
		return false
	}
	defaults := cfg.Agents.Defaults
	if unsafe(defaults.Provider) {
		return fmt.Errorf("微信用户隔离不支持可执行本机操作的 provider %q", defaults.Provider)
	}
	models := []string{defaults.GetModelName(), defaults.ImageModel}
	models = append(models, defaults.ModelFallbacks...)
	models = append(models, defaults.ImageModelFallbacks...)
	if defaults.Routing != nil && defaults.Routing.Enabled {
		models = append(models, defaults.Routing.LightModel)
	}
	for _, name := range models {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if unsafe(strings.SplitN(strings.TrimSpace(name), "/", 2)[0]) {
			return fmt.Errorf("微信用户隔离不支持本机模型 %q", name)
		}
		// GetModelConfig 会轮询同名条目，不能只检查它本次抽到的一个配置。
		for _, modelCfg := range cfg.ModelList {
			if modelCfg == nil || strings.TrimSpace(modelCfg.ModelName) != name {
				continue
			}
			protocol, _ := providers.ExtractProtocol(modelCfg)
			if unsafe(protocol) {
				return fmt.Errorf("微信用户隔离不支持本机模型 %q", name)
			}
		}
		for _, candidate := range resolveModelCandidates(cfg, defaults.Provider, name, nil) {
			protocol := candidate.Provider
			if mc, err := resolvedCandidateModelConfig(cfg, candidate, ""); err == nil {
				protocol, _ = providers.ExtractProtocol(mc)
			}
			if unsafe(protocol) {
				return fmt.Errorf("微信用户隔离不支持本机模型 %q", name)
			}
		}
	}
	return nil
}
