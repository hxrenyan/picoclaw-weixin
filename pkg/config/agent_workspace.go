package config

import (
	"path/filepath"
	"strings"
)

// ResolveAgentWorkspace 供实例创建和隔离校验共用，保证实际工作目录与安全边界一致。
func ResolveAgentWorkspace(agentCfg *AgentConfig, defaults *AgentDefaults) string {
	if agentCfg != nil && strings.TrimSpace(agentCfg.Workspace) != "" {
		return expandHome(strings.TrimSpace(agentCfg.Workspace))
	}
	if agentCfg == nil || agentCfg.Default || agentCfg.ID == "" ||
		NormalizeAgentID(agentCfg.ID) == DefaultAgentID {
		return expandHome(defaults.Workspace)
	}
	return filepath.Join(expandHome(defaults.Workspace), "..", "workspace-"+NormalizeAgentID(agentCfg.ID))
}
