package weixin

import (
	"context"
	"fmt"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/routing"
)

// multiWeixinChannel routes replies back to the bot that received the message.
// Long-polls share one scheduler instead of one blocked loop per bot.
type multiWeixinChannel struct {
	*channels.BaseChannel
	bots   map[string]*WeixinChannel
	order  []string
	cancel context.CancelFunc
}

func newMultiWeixinChannel(
	channelName string,
	bc *config.Channel,
	settings *config.WeixinSettings,
	messageBus *bus.MessageBus,
	specs []AccountSpec,
) (*multiWeixinChannel, error) {
	bots := make(map[string]*WeixinChannel, len(specs))
	order := make([]string, 0, len(specs))
	for _, spec := range specs {
		childBC := channelForBot(bc, channelName, spec)
		bot, err := NewWeixinChannel(&childBC, spec.FlatSettings(), messageBus)
		if err != nil {
			return nil, fmt.Errorf("weixin account %q: %w", spec.Key, err)
		}
		bot.accountKey = spec.Key
		bot.SetName(channelName)
		bot.SetOwner(bot)
		bots[spec.Key] = bot
		order = append(order, spec.Key)
	}

	base := channels.NewBaseChannel(
		channelName,
		settings,
		messageBus,
		bc.AllowFrom,
		channels.WithMaxMessageLength(4000),
		channels.WithReasoningChannelID(bc.ReasoningChannelID),
	)
	return &multiWeixinChannel{
		BaseChannel: base,
		bots:        bots,
		order:       order,
	}, nil
}

func (m *multiWeixinChannel) SetName(name string) {
	m.BaseChannel.SetName(name)
	for _, bot := range m.bots {
		bot.SetName(name)
	}
}

func (m *multiWeixinChannel) SetMediaStore(store media.MediaStore) {
	m.BaseChannel.SetMediaStore(store)
	for _, bot := range m.bots {
		bot.SetMediaStore(store)
	}
}

func (m *multiWeixinChannel) SetPlaceholderRecorder(recorder channels.PlaceholderRecorder) {
	m.BaseChannel.SetPlaceholderRecorder(recorder)
	for _, bot := range m.bots {
		bot.SetPlaceholderRecorder(recorder)
	}
}

func (m *multiWeixinChannel) Start(ctx context.Context) error {
	m.SetRunning(true)
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	bots := make([]*WeixinChannel, 0, len(m.order))
	for _, key := range m.order {
		bot := m.bots[key]
		bot.prepare(ctx)
		bots = append(bots, bot)
	}
	logger.InfoCF("weixin", "Starting shared Weixin poller", map[string]any{
		"bots": len(bots),
	})
	go runSharedPoller(ctx, bots)
	return nil
}

func (m *multiWeixinChannel) Stop(ctx context.Context) error {
	m.SetRunning(false)
	if m.cancel != nil {
		m.cancel()
	}
	var first error
	for _, key := range m.order {
		if err := m.bots[key].Stop(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (m *multiWeixinChannel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	bot := m.botForOutbound(msg.Context.Account, msg.Scope)
	if bot == nil {
		return nil, fmt.Errorf("weixin send: %w: no bot for account %q", channels.ErrSendFailed, msg.Context.Account)
	}
	return bot.Send(ctx, msg)
}

func (m *multiWeixinChannel) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) ([]string, error) {
	bot := m.botForOutbound(msg.Context.Account, msg.Scope)
	if bot == nil {
		return nil, fmt.Errorf("weixin send media: %w: no bot for account %q", channels.ErrSendFailed, msg.Context.Account)
	}
	return bot.SendMedia(ctx, msg)
}

func (m *multiWeixinChannel) VoiceCapabilities() channels.VoiceCapabilities {
	return channels.VoiceCapabilities{ASR: true, TTS: true}
}

func (m *multiWeixinChannel) botForOutbound(account string, scope *bus.OutboundScope) *WeixinChannel {
	bot := m.botFor(account)
	if bot == nil && scope != nil {
		bot = m.botFor(scope.Account)
	}
	return bot
}

func (m *multiWeixinChannel) botFor(account string) *WeixinChannel {
	key := routing.NormalizeAccountID(account)
	if bot, ok := m.bots[key]; ok {
		return bot
	}
	if account == "" {
		if bot, ok := m.bots[DefaultAccountKey]; ok {
			return bot
		}
	}
	return nil
}
