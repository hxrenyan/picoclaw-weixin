package auth

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAuthCommand(t *testing.T) {
	cmd := NewAuthCommand()

	require.NotNil(t, cmd)

	assert.Equal(t, "auth", cmd.Use)
	assert.Equal(t, "Manage authentication (login, logout, status)", cmd.Short)

	assert.Len(t, cmd.Aliases, 0)

	assert.Nil(t, cmd.Run)
	assert.NotNil(t, cmd.RunE)

	assert.Nil(t, cmd.PersistentPreRun)
	assert.Nil(t, cmd.PersistentPostRun)

	assert.False(t, cmd.HasFlags())
	assert.True(t, cmd.HasSubCommands())

	allowedCommands := []string{
		"login",
		"logout",
		"status",
		"models",
		"weixin",
		"wecom",
	}

	subcommands := cmd.Commands()
	assert.Len(t, subcommands, len(allowedCommands))

	for _, subcmd := range subcommands {
		found := slices.Contains(allowedCommands, subcmd.Name())
		assert.True(t, found, "unexpected subcommand %q", subcmd.Name())

		assert.Len(t, subcmd.Aliases, 0)
		assert.False(t, subcmd.Hidden)

		if subcmd.Name() == "weixin" {
			children := subcmd.Commands()
			require.Len(t, children, 1)
			assert.Equal(t, "list", children[0].Name())
			assert.False(t, children[0].HasSubCommands())
		} else {
			assert.False(t, subcmd.HasSubCommands())
		}

		assert.Nil(t, subcmd.Run)
		assert.NotNil(t, subcmd.RunE)

		assert.Nil(t, subcmd.PersistentPreRun)
		assert.Nil(t, subcmd.PersistentPostRun)
	}
}
