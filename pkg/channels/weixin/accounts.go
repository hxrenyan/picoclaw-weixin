package weixin

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/routing"
)

// LockConfig 串行化同一微信配置文件的读改写操作，并返回解锁函数。
func LockConfig(path string) (func(), error) {
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	lockFile, err := lockWeixinConfigFile(filepath.Clean(cleanPath) + ".weixin.lock")
	if err != nil {
		return nil, err
	}
	return func() { _ = lockFile.Close() }, nil
}

const (
	// DefaultAccountKey is the first WeChat bot. Its credentials stay on the
	// legacy WeixinSettings fields so configs written before multi-bot support
	// keep working.
	DefaultAccountKey = "default"
	// MaxAccounts is how many WeChat bots one channel will poll. The bots share
	// one poller and one HTTP connection pool, so the cap is not one OS thread
	// per bot. Each bot still keeps its own token and one in-flight getupdates.
	MaxAccounts = 64

	defaultBaseURL = "https://ilinkai.weixin.qq.com/"
	maxAccountName = 40
)

// AccountSpec is one resolved WeChat bot, ready to open its own poll loop.
type AccountSpec struct {
	Key       string
	Name      string
	Disabled  bool
	AllowFrom config.FlexibleStringSlice
	Settings  config.WeixinSettings
}

// FlatSettings returns credentials for a single poll loop, without the nested account map.
func (s AccountSpec) FlatSettings() *config.WeixinSettings {
	copied := s.Settings
	copied.Accounts = nil
	return &copied
}

// ListAccounts returns the default bot plus every extra bot.
// The default bot is always present, even when it has no token yet.
func ListAccounts(cfg *config.WeixinSettings) ([]AccountSpec, error) {
	if cfg == nil {
		cfg = &config.WeixinSettings{}
	}
	if err := rejectDuplicateNormalizedKeys(cfg); err != nil {
		return nil, err
	}

	defaultSpec := defaultAccountSpec(cfg)
	extras, err := extraAccountSpecs(cfg, defaultSpec)
	if err != nil {
		return nil, err
	}
	specs := make([]AccountSpec, 0, 1+len(extras))
	specs = append(specs, defaultSpec)
	specs = append(specs, extras...)
	if err := validateAccountSpecs(specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// RuntimeAccounts returns bots that should open a long-poll connection.
// A bot with an empty token is kept in config but not polled.
// When every token is empty, the default bot is still returned so startup
// behavior matches the original single-bot channel.
func RuntimeAccounts(cfg *config.WeixinSettings) ([]AccountSpec, error) {
	specs, err := ListAccounts(cfg)
	if err != nil {
		return nil, err
	}
	runnable := make([]AccountSpec, 0, len(specs))
	for _, spec := range specs {
		if spec.Disabled || strings.TrimSpace(spec.Settings.Token.String()) == "" {
			continue
		}
		runnable = append(runnable, spec)
	}
	if len(runnable) == 0 {
		return []AccountSpec{defaultAccountSpec(cfg)}, nil
	}
	return runnable, nil
}

// EnsureAccount reserves a new empty bot slot and returns its key.
func EnsureAccount(cfg *config.WeixinSettings, name string) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("weixin settings are nil")
	}
	name = strings.TrimSpace(name)
	if err := validateAccountName(name); err != nil {
		return "", err
	}
	specs, err := ListAccounts(cfg)
	if err != nil {
		return "", err
	}
	if len(specs) >= MaxAccounts {
		return "", fmt.Errorf("at most %d WeChat bots are supported", MaxAccounts)
	}
	key := nextAccountKey(specs)
	if key == "" {
		return "", fmt.Errorf("at most %d WeChat bots are supported", MaxAccounts)
	}
	enabled := true
	putAccount(cfg, key, config.WeixinAccountSettings{
		Name:    name,
		Enabled: &enabled,
	})
	return key, nil
}

// UpsertCredentials stores a QR-login result on the default bot or a named bot.
// An unknown non-default key is created when the account limit allows it.
func UpsertCredentials(
	cfg *config.WeixinSettings,
	accountKey, displayName, token, userID, botID, baseURL, proxy string,
) error {
	if cfg == nil {
		return fmt.Errorf("weixin settings are nil")
	}
	key, err := canonicalAccountKey(accountKey)
	if err != nil {
		return err
	}
	displayName = strings.TrimSpace(displayName)
	if displayName != "" {
		if err := validateAccountName(displayName); err != nil {
			return err
		}
	}
	if err := assertUniqueBinding(cfg, key, botID, token); err != nil {
		return err
	}

	if key == DefaultAccountKey {
		return upsertDefaultAccount(cfg, displayName, token, userID, botID, baseURL, proxy)
	}
	return upsertNamedAccount(cfg, key, displayName, token, userID, botID, baseURL, proxy)
}

// RemoveAccount deletes a named bot. The default bot is kept and its login is cleared.
func RemoveAccount(cfg *config.WeixinSettings, accountKey string) error {
	if cfg == nil {
		return fmt.Errorf("weixin settings are nil")
	}
	key, err := canonicalAccountKey(accountKey)
	if err != nil {
		return err
	}
	if key == DefaultAccountKey {
		cfg.SetToken("")
		cfg.AccountID = ""
		cfg.UserID = ""
		if raw, acct, ok := findAccount(cfg, DefaultAccountKey); ok {
			acct.Token = config.SecureString{}
			acct.AccountID = ""
			acct.UserID = ""
			cfg.Accounts[raw] = acct
		}
		return nil
	}
	raw, _, ok := findAccount(cfg, key)
	if !ok {
		return fmt.Errorf("weixin account %q not found", key)
	}
	delete(cfg.Accounts, raw)
	return nil
}

// EnsureWeixinChannel returns the named weixin channel, creating it when missing.
func EnsureWeixinChannel(cfg *config.Config, channelName string) (*config.Channel, *config.WeixinSettings, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("config is nil")
	}
	if strings.TrimSpace(channelName) == "" {
		channelName = config.ChannelWeixin
	}
	if cfg.Channels == nil {
		cfg.Channels = config.ChannelsConfig{}
	}
	bc := cfg.Channels.Get(channelName)
	if bc == nil {
		bc = &config.Channel{Type: config.ChannelWeixin}
		bc.SetName(channelName)
		cfg.Channels[channelName] = bc
	}
	if bc.Type == "" {
		bc.Type = config.ChannelWeixin
	}
	if bc.Type != config.ChannelWeixin {
		return nil, nil, fmt.Errorf("channel %q is type %q, not weixin", channelName, bc.Type)
	}
	bc.Enabled = true
	settings, err := settingsFromChannel(bc)
	if err != nil {
		return nil, nil, err
	}
	return bc, settings, nil
}

func settingsFromChannel(bc *config.Channel) (*config.WeixinSettings, error) {
	decoded, err := bc.GetDecoded()
	if err != nil {
		return nil, err
	}
	if settings, ok := decoded.(*config.WeixinSettings); ok && settings != nil {
		return settings, nil
	}
	settings := &config.WeixinSettings{}
	if err := bc.Decode(settings); err != nil {
		return nil, err
	}
	return settings, nil
}

func defaultAccountSpec(cfg *config.WeixinSettings) AccountSpec {
	spec := AccountSpec{
		Key: DefaultAccountKey,
		Settings: config.WeixinSettings{
			IsolateAccounts: cfg.IsolateAccounts,
			Token:           cfg.Token,
			AccountID:       strings.TrimSpace(cfg.AccountID),
			UserID:          strings.TrimSpace(cfg.UserID),
			BaseURL:         cfg.BaseURL,
			CDNBaseURL:      cfg.CDNBaseURL,
			Proxy:           cfg.Proxy,
		},
	}
	if _, acct, ok := findAccount(cfg, DefaultAccountKey); ok {
		if strings.TrimSpace(acct.Name) != "" {
			spec.Name = strings.TrimSpace(acct.Name)
		}
		if strings.TrimSpace(acct.Token.String()) != "" {
			spec.Settings.Token = acct.Token
		}
		if strings.TrimSpace(acct.AccountID) != "" {
			spec.Settings.AccountID = strings.TrimSpace(acct.AccountID)
		}
		if strings.TrimSpace(acct.UserID) != "" {
			spec.Settings.UserID = strings.TrimSpace(acct.UserID)
		}
		if strings.TrimSpace(acct.BaseURL) != "" {
			spec.Settings.BaseURL = acct.BaseURL
		}
		if strings.TrimSpace(acct.CDNBaseURL) != "" {
			spec.Settings.CDNBaseURL = acct.CDNBaseURL
		}
		if strings.TrimSpace(acct.Proxy) != "" {
			spec.Settings.Proxy = acct.Proxy
		}
		if len(acct.AllowFrom) > 0 {
			spec.AllowFrom = acct.AllowFrom
		}
		spec.Disabled = accountDisabled(acct.Enabled)
	}
	spec.Settings.Accounts = nil
	return spec
}

func extraAccountSpecs(cfg *config.WeixinSettings, parent AccountSpec) ([]AccountSpec, error) {
	grouped := make(map[string][]string)
	for raw := range cfg.Accounts {
		key := routing.NormalizeAccountID(raw)
		if key == DefaultAccountKey {
			continue
		}
		grouped[key] = append(grouped[key], raw)
	}
	keys := make([]string, 0, len(grouped))
	for key, raws := range grouped {
		if len(raws) > 1 {
			return nil, fmt.Errorf("weixin accounts %v normalize to the same key %q", raws, key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	specs := make([]AccountSpec, 0, len(keys))
	for _, key := range keys {
		raw := grouped[key][0]
		acct := cfg.Accounts[raw]
		settings := parent.Settings
		settings.Accounts = nil
		settings.Token = acct.Token
		settings.AccountID = strings.TrimSpace(acct.AccountID)
		settings.UserID = strings.TrimSpace(acct.UserID)
		if strings.TrimSpace(acct.BaseURL) != "" {
			settings.BaseURL = acct.BaseURL
		}
		if strings.TrimSpace(acct.CDNBaseURL) != "" {
			settings.CDNBaseURL = acct.CDNBaseURL
		}
		if strings.TrimSpace(acct.Proxy) != "" {
			settings.Proxy = acct.Proxy
		}
		spec := AccountSpec{
			Key:       key,
			Name:      strings.TrimSpace(acct.Name),
			AllowFrom: acct.AllowFrom,
			Settings:  settings,
		}
		spec.Disabled = accountDisabled(acct.Enabled)
		specs = append(specs, spec)
	}
	return specs, nil
}

func validateAccountSpecs(specs []AccountSpec) error {
	if len(specs) > MaxAccounts {
		return fmt.Errorf("at most %d WeChat bots are supported", MaxAccounts)
	}
	botOwners := make(map[string]string, len(specs))
	tokenOwners := make(map[string]string, len(specs))
	for _, spec := range specs {
		botID := strings.TrimSpace(spec.Settings.AccountID)
		if botID != "" {
			if prev, ok := botOwners[botID]; ok {
				return fmt.Errorf("weixin bot %q is linked to both %q and %q", botID, prev, spec.Key)
			}
			botOwners[botID] = spec.Key
		}
		token := strings.TrimSpace(spec.Settings.Token.String())
		if token != "" {
			if prev, ok := tokenOwners[token]; ok {
				return fmt.Errorf("weixin account %q uses the same token as %q", spec.Key, prev)
			}
			tokenOwners[token] = spec.Key
		}
	}
	return nil
}

func assertUniqueBinding(cfg *config.WeixinSettings, accountKey, botID, token string) error {
	specs, err := ListAccounts(cfg)
	if err != nil {
		return err
	}
	botID = strings.TrimSpace(botID)
	token = strings.TrimSpace(token)
	for _, spec := range specs {
		if spec.Key == accountKey {
			continue
		}
		if botID != "" && strings.TrimSpace(spec.Settings.AccountID) == botID {
			return fmt.Errorf("this WeChat bot is already linked to account %q", spec.Key)
		}
		if token != "" && strings.TrimSpace(spec.Settings.Token.String()) == token {
			return fmt.Errorf("this WeChat token is already used by account %q", spec.Key)
		}
	}
	return nil
}

func upsertDefaultAccount(cfg *config.WeixinSettings, displayName, token, userID, botID, baseURL, proxy string) error {
	if token != "" {
		cfg.SetToken(token)
	}
	if strings.TrimSpace(botID) != "" {
		cfg.AccountID = strings.TrimSpace(botID)
	}
	if strings.TrimSpace(userID) != "" {
		cfg.UserID = strings.TrimSpace(userID)
	}
	if baseURL != "" && !sameBaseURL(baseURL, defaultBaseURL) {
		cfg.BaseURL = baseURL
	}
	if proxy != "" {
		cfg.Proxy = proxy
	}
	raw, acct, ok := findAccount(cfg, DefaultAccountKey)
	if !ok && displayName == "" {
		return nil
	}
	if !ok {
		raw = DefaultAccountKey
	}
	fillStoredAccount(&acct, displayName, token, userID, botID, proxy)
	if baseURL != "" && !sameBaseURL(baseURL, defaultBaseURL) {
		acct.BaseURL = baseURL
	}
	putAccount(cfg, raw, acct)
	return nil
}

func upsertNamedAccount(
	cfg *config.WeixinSettings,
	key, displayName, token, userID, botID, baseURL, proxy string,
) error {
	raw, acct, ok := findAccount(cfg, key)
	if !ok {
		specs, err := ListAccounts(cfg)
		if err != nil {
			return err
		}
		if len(specs) >= MaxAccounts {
			return fmt.Errorf("at most %d WeChat bots are supported", MaxAccounts)
		}
		raw = key
		enabled := true
		acct.Enabled = &enabled
	}
	fillStoredAccount(&acct, displayName, token, userID, botID, proxy)
	if strings.TrimSpace(baseURL) != "" {
		acct.BaseURL = baseURL
	}
	if raw != key && cfg.Accounts != nil {
		delete(cfg.Accounts, raw)
	}
	putAccount(cfg, key, acct)
	return nil
}

func rejectDuplicateNormalizedKeys(cfg *config.WeixinSettings) error {
	if cfg == nil || len(cfg.Accounts) == 0 {
		return nil
	}
	grouped := make(map[string][]string)
	for raw := range cfg.Accounts {
		key := routing.NormalizeAccountID(raw)
		grouped[key] = append(grouped[key], raw)
	}
	for key, raws := range grouped {
		if len(raws) > 1 {
			return fmt.Errorf("weixin accounts %v normalize to the same key %q", raws, key)
		}
	}
	return nil
}

func findAccount(cfg *config.WeixinSettings, accountKey string) (string, config.WeixinAccountSettings, bool) {
	if cfg == nil || len(cfg.Accounts) == 0 {
		return "", config.WeixinAccountSettings{}, false
	}
	want := routing.NormalizeAccountID(accountKey)
	var matchedRaw string
	var matched config.WeixinAccountSettings
	found := 0
	for raw, acct := range cfg.Accounts {
		if routing.NormalizeAccountID(raw) != want {
			continue
		}
		found++
		matchedRaw = raw
		matched = acct
	}
	if found != 1 {
		return "", config.WeixinAccountSettings{}, false
	}
	return matchedRaw, matched, true
}

func nextAccountKey(specs []AccountSpec) string {
	used := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		used[spec.Key] = struct{}{}
	}
	for i := 2; i <= MaxAccounts+1; i++ {
		key := fmt.Sprintf("bot-%d", i)
		if _, ok := used[key]; !ok {
			return key
		}
	}
	return ""
}

func canonicalAccountKey(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.EqualFold(trimmed, DefaultAccountKey) {
		return DefaultAccountKey, nil
	}
	normalized := routing.NormalizeAccountID(trimmed)
	if normalized != strings.ToLower(trimmed) {
		return "", fmt.Errorf("weixin account key %q is invalid; use letters, numbers, '_' or '-'", raw)
	}
	return normalized, nil
}

func validateAccountName(name string) error {
	count := utf8.RuneCountInString(name)
	if count < 1 || count > maxAccountName {
		return fmt.Errorf("weixin bot name must be 1-%d characters", maxAccountName)
	}
	return nil
}

func sameBaseURL(left, right string) bool {
	return strings.TrimRight(strings.TrimSpace(left), "/") == strings.TrimRight(strings.TrimSpace(right), "/")
}

func accountDisabled(enabled *bool) bool {
	return enabled != nil && !*enabled
}

func putAccount(cfg *config.WeixinSettings, key string, acct config.WeixinAccountSettings) {
	if cfg.Accounts == nil {
		cfg.Accounts = map[string]config.WeixinAccountSettings{}
	}
	cfg.Accounts[key] = acct
}

func fillStoredAccount(acct *config.WeixinAccountSettings, displayName, token, userID, botID, proxy string) {
	if displayName != "" {
		acct.Name = displayName
	}
	if token != "" {
		acct.Token = *config.NewSecureString(token)
	}
	if id := strings.TrimSpace(botID); id != "" {
		acct.AccountID = id
	}
	if id := strings.TrimSpace(userID); id != "" {
		acct.UserID = id
	}
	if proxy != "" {
		acct.Proxy = proxy
	}
}
