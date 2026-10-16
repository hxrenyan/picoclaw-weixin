package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ResolveWeixinIsolation 的第二个返回值表示该渠道是否强制隔离。
// 强制隔离时，未知、禁用或未绑定账号返回 nil，调用方必须拒绝回退到共享 Agent。
func (c *Config) ResolveWeixinIsolation(channel, account string) (*AgentConfig, bool) {
	if c == nil {
		return nil, false
	}
	channel = strings.TrimSpace(channel)
	bc := c.Channels.Get(channel)
	if bc == nil || bc.Type != ChannelWeixin {
		return nil, false
	}
	decoded, err := bc.GetDecoded()
	if err != nil {
		return nil, true
	}
	settings, ok := decoded.(*WeixinSettings)
	if !ok || !settings.IsolateAccounts {
		return nil, false
	}
	if !bc.Enabled {
		return nil, true
	}
	account = strings.ToLower(strings.TrimSpace(account))
	if account == "" {
		account = "default"
	}
	bot := WeixinAccountSettings{}
	if account == "default" {
		bot.Token = settings.Token
		bot.AccountID = settings.AccountID
		bot.UserID = settings.UserID
	}
	for key, stored := range settings.Accounts {
		if strings.ToLower(strings.TrimSpace(key)) != account {
			continue
		}
		bot.Name, bot.Enabled = stored.Name, stored.Enabled
		if account != "default" || stored.Token.String() != "" {
			bot.Token = stored.Token
		}
		if account != "default" || stored.AccountID != "" {
			bot.AccountID = stored.AccountID
		}
		if account != "default" || stored.UserID != "" {
			bot.UserID = stored.UserID
		}
	}
	if (bot.Enabled != nil && !*bot.Enabled) || strings.TrimSpace(bot.Token.String()) == "" {
		return nil, true
	}
	id := WeixinIsolationAgentID(channel, account, bot.AccountID, bot.UserID, bot.Token.String())
	workspace, ok := c.isolatedWorkspace(id)
	if !ok {
		return nil, true
	}
	return &AgentConfig{
		ID: id, Name: bot.Name, Isolated: true,
		Workspace: workspace,
	}, true
}

// isolatedWorkspace 保留既有目录位置，按真实路径验证所有共享 Agent 与隔离目录互不重叠。
func (c *Config) isolatedWorkspace(agentID string) (string, bool) {
	path := c.WorkspacePath()
	if path == "" {
		return "", false
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	derivedRoot := filepath.Join(filepath.Dir(path), "weixin-users")
	derivedReal, err := resolveIsolationDirectory(derivedRoot)
	if err != nil {
		return "", false
	}
	sharedPaths := []string{path}
	for i := range c.Agents.List {
		ac := &c.Agents.List[i]
		if !ac.Isolated {
			sharedPaths = append(sharedPaths, ResolveAgentWorkspace(ac, &c.Agents.Defaults))
		}
	}
	checked := make(map[string]bool, len(sharedPaths))
	for _, sharedPath := range sharedPaths {
		sharedPath, err = filepath.Abs(sharedPath)
		if err != nil {
			return "", false
		}
		if checked[sharedPath] {
			continue
		}
		checked[sharedPath] = true
		sharedReal, err := resolveIsolationDirectory(sharedPath)
		if err != nil {
			return "", false
		}
		for _, roots := range [][2]isolationDirectory{{sharedReal, derivedReal}, {derivedReal, sharedReal}} {
			contains, err := roots[0].contains(roots[1])
			if err != nil || contains {
				return "", false
			}
		}
	}
	workspace := filepath.Join(derivedRoot, agentID)
	workspaceReal, err := resolveIsolationDirectory(workspace)
	// 用户目录不能经符号链接改指共享目录或其他用户的目录。
	if err != nil || workspaceReal.path != filepath.Join(derivedReal.path, agentID) {
		return "", false
	}
	return workspace, true
}

type isolationDirectory struct {
	path     string
	ancestor string
	suffix   string
	info     os.FileInfo
}

// contains 按目录身份判断包含关系，兼容大小写及其他已有路径别名。
func (d isolationDirectory) contains(other isolationDirectory) (bool, error) {
	if d.suffix != "." {
		if !os.SameFile(d.info, other.info) {
			return false, nil
		}
		// 缺失后缀没有目录身份，保守拒绝仅大小写不同的潜在重叠。
		parts := strings.Split(d.suffix, string(filepath.Separator))
		otherParts := strings.Split(other.suffix, string(filepath.Separator))
		if len(parts) > len(otherParts) {
			return false, nil
		}
		for i, part := range parts {
			if !strings.EqualFold(part, otherParts[i]) {
				return false, nil
			}
		}
		return true, nil
	}
	current, info := other.ancestor, other.info
	for {
		if os.SameFile(d.info, info) {
			return true, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
		current = parent
		var err error
		info, err = os.Stat(current)
		if err != nil {
			return false, err
		}
	}
}

// resolveIsolationDirectory 解析已有祖先的符号链接，再补回尚未创建的目录层级。
// Lstat 确保断链不会被当作普通的缺失目录跳过；链接循环、权限错误和非目录均拒绝。
func resolveIsolationDirectory(path string) (isolationDirectory, error) {
	for current := path; ; current = filepath.Dir(current) {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return isolationDirectory{}, err
			}
			info, err := os.Stat(resolved)
			if err != nil {
				return isolationDirectory{}, err
			}
			if !info.IsDir() {
				return isolationDirectory{}, fmt.Errorf("隔离路径的已有祖先不是目录: %s", current)
			}
			suffix, err := filepath.Rel(current, path)
			if err != nil {
				return isolationDirectory{}, err
			}
			return isolationDirectory{
				path: filepath.Join(resolved, suffix), ancestor: resolved, suffix: suffix, info: info,
			}, nil
		}
		if !os.IsNotExist(err) || filepath.Dir(current) == current {
			return isolationDirectory{}, err
		}
	}
}

// WeixinIsolationAgentID 同时用于接收时记录绑定身份和路由时校验，防止换绑前的排队消息进入新用户目录。
func WeixinIsolationAgentID(channel, account, botID, userID, token string) string {
	channel = strings.TrimSpace(channel)
	account = strings.ToLower(strings.TrimSpace(account))
	if account == "" {
		account = "default"
	}
	// 微信 Bot 身份参与目录分配：令牌刷新保留数据，换绑另一个微信身份使用新目录。
	identity := strings.TrimSpace(botID)
	if identity == "" {
		identity = strings.TrimSpace(userID)
	}
	if identity == "" {
		identity = strings.TrimSpace(token)
	}
	digest := sha256.Sum256([]byte(channel + "\x00" + account + "\x00" + identity))
	return fmt.Sprintf("weixin-user-%x", digest[:16])
}

// IsolatedWeixinAgents 按固定顺序生成运行时配置，不把派生 Agent 写回配置文件。
func (c *Config) IsolatedWeixinAgents() []*AgentConfig {
	var agents []*AgentConfig
	channels := make([]string, 0, len(c.Channels))
	for name := range c.Channels {
		channels = append(channels, name)
	}
	sort.Strings(channels)
	for _, name := range channels {
		bc := c.Channels[name]
		if bc == nil || bc.Type != ChannelWeixin {
			continue
		}
		decoded, err := bc.GetDecoded()
		settings, ok := decoded.(*WeixinSettings)
		if err != nil || !ok || !settings.IsolateAccounts {
			continue
		}
		accounts := []string{"default"}
		for key := range settings.Accounts {
			if strings.ToLower(strings.TrimSpace(key)) != "default" {
				accounts = append(accounts, key)
			}
		}
		sort.Strings(accounts)
		for _, account := range accounts {
			if agent, _ := c.ResolveWeixinIsolation(name, account); agent != nil {
				agents = append(agents, agent)
			}
		}
	}
	return agents
}
