package routing

import "github.com/sipeed/picoclaw/pkg/config"

const (
	DefaultAgentID   = config.DefaultAgentID
	DefaultMainKey   = "main"
	DefaultAccountID = config.DefaultAccountID
	MaxAgentIDLength = config.MaxAgentIDLength
)

// NormalizeAgentID sanitizes an agent ID to [a-z0-9][a-z0-9_-]{0,63}.
// Invalid characters are collapsed to "-". Leading/trailing dashes stripped.
// Empty input returns DefaultAgentID ("main").
func NormalizeAgentID(id string) string {
	return config.NormalizeAgentID(id)
}

// NormalizeAccountID sanitizes an account ID. Empty returns DefaultAccountID.
func NormalizeAccountID(id string) string {
	return config.NormalizeAccountID(id)
}
