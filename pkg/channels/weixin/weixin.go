// Package weixin talks to WeChat personal accounts through the Tencent iLink API.
// One channel can host several bots. Each bot keeps its own QR login, token,
// long-poll cursor, and conversation. The first bot still uses the original
// top-level token fields, so a single-bot config does not need to change.
package weixin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// WeixinChannel is the Weixin channel implementation over Tencent iLink REST API.
type WeixinChannel struct {
	*channels.BaseChannel
	api    *ApiClient
	config *config.WeixinSettings
	ctx    context.Context
	cancel context.CancelFunc
	bus    *bus.MessageBus
	// accountKey identifies this WeChat bot inside the channel. Empty means the
	// historical single-bot channel; "default" is the first bot.
	accountKey string
	// contextTokens stores the last context_token per user (from_user_id → context_token).
	// This is required by the iLink API to associate replies with the right chat session.
	contextTokens     sync.Map
	typingMu          sync.Mutex
	typingCache       map[string]typingTicketCacheEntry
	pauseMu           sync.Mutex
	pauseUntil        time.Time
	syncBufPath       string
	contextTokensPath string
	pollInit          sync.Once
	cursor            pollCursor
}

func init() {
	channels.RegisterFactory(
		config.ChannelWeixin,
		func(channelName, channelType string, cfg *config.Config, bus *bus.MessageBus) (channels.Channel, error) {
			bc := cfg.Channels[channelName]
			decoded, err := bc.GetDecoded()
			if err != nil {
				return nil, err
			}
			weixinCfg, ok := decoded.(*config.WeixinSettings)
			if !ok {
				return nil, channels.ErrSendFailed
			}
			return newWeixinChannelFromConfig(channelName, bc, weixinCfg, bus)
		},
	)
}

// newWeixinChannelFromConfig starts one poll loop per linked WeChat bot.
// A single bot keeps the original channel type. Two or more bots share one
// channel name and are separated by the inbound account id.
func newWeixinChannelFromConfig(
	channelName string,
	bc *config.Channel,
	weixinCfg *config.WeixinSettings,
	messageBus *bus.MessageBus,
) (channels.Channel, error) {
	if channelName == "" {
		channelName = bc.Name()
	}
	if channelName == "" {
		channelName = config.ChannelWeixin
	}
	specs, err := RuntimeAccounts(weixinCfg)
	if err != nil {
		return nil, err
	}
	if len(specs) <= 1 {
		spec := specs[0]
		childBC := channelForBot(bc, channelName, spec)
		ch, err := NewWeixinChannel(&childBC, spec.FlatSettings(), messageBus)
		if err != nil {
			return nil, err
		}
		ch.accountKey = spec.Key
		ch.SetName(channelName)
		return ch, nil
	}
	multi, err := newMultiWeixinChannel(channelName, bc, weixinCfg, messageBus, specs)
	if err != nil {
		return nil, err
	}
	multi.SetName(channelName)
	return multi, nil
}

// channelForBot copies the channel and applies one bot's allow list.
func channelForBot(bc *config.Channel, channelName string, spec AccountSpec) config.Channel {
	child := *bc
	child.SetName(channelName)
	if len(spec.AllowFrom) > 0 {
		child.AllowFrom = spec.AllowFrom
	}
	return child
}

// NewWeixinChannel creates a new WeixinChannel from config.
func NewWeixinChannel(
	bc *config.Channel,
	cfg *config.WeixinSettings,
	messageBus *bus.MessageBus,
) (*WeixinChannel, error) {
	api, err := NewApiClient(cfg.BaseURL, cfg.Token.String(), cfg.Proxy)
	if err != nil {
		return nil, fmt.Errorf("weixin: failed to create API client: %w", err)
	}

	base := channels.NewBaseChannel(
		bc.Name(),
		cfg,
		messageBus,
		bc.AllowFrom,
		channels.WithMaxMessageLength(4000),
		channels.WithReasoningChannelID(bc.ReasoningChannelID),
	)

	return &WeixinChannel{
		BaseChannel:       base,
		api:               api,
		config:            cfg,
		bus:               messageBus,
		typingCache:       make(map[string]typingTicketCacheEntry),
		syncBufPath:       buildWeixinSyncBufPath(cfg),
		contextTokensPath: buildWeixinContextTokensPath(cfg),
	}, nil
}

func (c *WeixinChannel) Start(ctx context.Context) error {
	logger.InfoCF("weixin", "Starting Weixin channel", map[string]any{
		"account": c.accountKey,
	})
	c.prepare(ctx)
	go c.pollLoop(c.ctx)
	logger.InfoC("weixin", "Weixin channel started")
	return nil
}

// prepare loads this bot's cursor and context tokens without starting a poll loop.
// The shared poller uses it so every bot waits on one scheduler.
func (c *WeixinChannel) prepare(ctx context.Context) {
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.SetRunning(true)
	c.restoreContextTokens()
}

// restoreContextTokens loads persisted context tokens from disk into memory.
func (c *WeixinChannel) restoreContextTokens() {
	tokens, err := loadContextTokens(c.contextTokensPath)
	if err != nil {
		logger.WarnCF("weixin", "Failed to load persisted context tokens", map[string]any{
			"path":  c.contextTokensPath,
			"error": err.Error(),
		})
		return
	}
	if len(tokens) == 0 {
		return
	}
	for userID, token := range tokens {
		c.contextTokens.Store(userID, token)
	}
	logger.InfoCF("weixin", "Restored context tokens from disk", map[string]any{
		"path":  c.contextTokensPath,
		"count": len(tokens),
	})
}

// persistContextTokens saves all in-memory context tokens to disk.
func (c *WeixinChannel) persistContextTokens() {
	tokens := make(map[string]string)
	c.contextTokens.Range(func(k, v any) bool {
		if userID, ok := k.(string); ok {
			if token, ok := v.(string); ok {
				tokens[userID] = token
			}
		}
		return true
	})
	if err := saveContextTokens(c.contextTokensPath, tokens); err != nil {
		logger.WarnCF("weixin", "Failed to persist context tokens", map[string]any{
			"path":  c.contextTokensPath,
			"error": err.Error(),
		})
	}
}

func (c *WeixinChannel) Stop(ctx context.Context) error {
	logger.InfoC("weixin", "Stopping Weixin channel")
	c.SetRunning(false)
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

const (
	defaultPollTimeoutMs    = 35_000
	pollRetryDelay          = 2 * time.Second
	pollBackoffDelay        = 30 * time.Second
	pollMaxConsecutiveFails = 3
	// pollStop tells the poller not to schedule this bot again.
	pollStop = time.Duration(-1)
)

// pollCursor is the per-bot long-poll state. One bot is polled by at most one
// worker at a time, so the shared poller does not lock it.
type pollCursor struct {
	buf       string
	timeoutMs int
	fails     int
}

// pollLoop is the single-bot receive loop. Several bots use runSharedPoller instead.
func (c *WeixinChannel) pollLoop(ctx context.Context) {
	for {
		delay := c.pollOnce(ctx)
		if delay < 0 || ctx.Err() != nil {
			logger.InfoC("weixin", "Weixin poll loop stopped")
			return
		}
		if delay == 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			logger.InfoC("weixin", "Weixin poll loop stopped")
			return
		case <-timer.C:
		}
	}
}

// pollOnce performs one getupdates call and returns how long to wait before the next.
func (c *WeixinChannel) pollOnce(ctx context.Context) time.Duration {
	c.loadPollCursor()

	select {
	case <-ctx.Done():
		return pollStop
	default:
	}

	if err := c.waitWhileSessionPaused(ctx); err != nil {
		if ctx.Err() != nil {
			return pollStop
		}
		return 0
	}

	// Timeout is slightly longer than the long-poll the server was asked to hold.
	pollCtx, pollCancel := context.WithTimeout(ctx, time.Duration(c.cursor.timeoutMs+5000)*time.Millisecond)
	resp, err := c.api.GetUpdates(pollCtx, GetUpdatesReq{
		GetUpdatesBuf: c.cursor.buf,
	})
	pollCancel()

	if err != nil {
		if ctx.Err() != nil {
			return pollStop
		}

		c.cursor.fails++
		logger.WarnCF("weixin", "getUpdates failed", map[string]any{
			"error":   err.Error(),
			"attempt": c.cursor.fails,
			"account": c.accountKey,
		})

		if c.cursor.fails >= pollMaxConsecutiveFails {
			logger.ErrorCF("weixin", "Too many consecutive failures, backing off", map[string]any{
				"duration": pollBackoffDelay,
				"account":  c.accountKey,
			})
			c.cursor.fails = 0
			return pollBackoffDelay
		}
		return pollRetryDelay
	}

	if isSessionExpiredStatus(resp.Ret, resp.Errcode) {
		remaining := c.pauseSession("getupdates", resp.Ret, resp.Errcode, resp.Errmsg)
		if remaining < 0 {
			return 0
		}
		return remaining
	}

	if resp.Errcode != 0 || resp.Ret != 0 {
		c.cursor.fails++
		logger.ErrorCF("weixin", "getUpdates API error", map[string]any{
			"ret":     resp.Ret,
			"errcode": resp.Errcode,
			"errmsg":  resp.Errmsg,
			"account": c.accountKey,
		})
		return pollRetryDelay
	}

	c.cursor.fails = 0

	if resp.LongpollingTimeoutMs > 0 {
		c.cursor.timeoutMs = resp.LongpollingTimeoutMs
	}

	if resp.GetUpdatesBuf != "" {
		c.cursor.buf = resp.GetUpdatesBuf
		if err := saveGetUpdatesBuf(c.syncBufPath, c.cursor.buf); err != nil {
			logger.WarnCF("weixin", "Failed to persist get_updates_buf", map[string]any{
				"path":  c.syncBufPath,
				"error": err.Error(),
			})
		}
	}

	for _, msg := range resp.Msgs {
		c.handleInboundMessage(ctx, msg)
	}
	return 0
}

func (c *WeixinChannel) loadPollCursor() {
	c.pollInit.Do(func() {
		c.cursor.timeoutMs = defaultPollTimeoutMs
		buf, err := loadGetUpdatesBuf(c.syncBufPath)
		if err != nil {
			logger.WarnCF("weixin", "Failed to load persisted get_updates_buf", map[string]any{
				"path":  c.syncBufPath,
				"error": err.Error(),
			})
			return
		}
		if buf == "" {
			return
		}
		c.cursor.buf = buf
		logger.InfoCF("weixin", "Resuming persisted get_updates_buf", map[string]any{
			"path":   c.syncBufPath,
			"bytes":  len(buf),
			"source": "disk",
		})
	})
}

// handleInboundMessage converts a WeixinMessage to a bus.InboundMessage.
func (c *WeixinChannel) handleInboundMessage(ctx context.Context, msg WeixinMessage) {
	fromUserID := msg.FromUserID
	if fromUserID == "" {
		return
	}

	messageID := msg.ClientID
	if messageID == "" {
		messageID = uuid.New().String()
	}

	// Build text content from item_list
	var parts []string
	for _, item := range msg.ItemList {
		switch item.Type {
		case MessageItemTypeText:
			if item.TextItem != nil && item.TextItem.Text != "" {
				parts = append(parts, item.TextItem.Text)
			}
		case MessageItemTypeVoice:
			if item.VoiceItem != nil && item.VoiceItem.Text != "" {
				// Use voice → text transcription from server
				parts = append(parts, item.VoiceItem.Text)
			} else {
				parts = append(parts, "[audio]")
			}
		case MessageItemTypeImage:
			parts = append(parts, "[image]")
		case MessageItemTypeFile:
			if item.FileItem != nil && item.FileItem.FileName != "" {
				parts = append(parts, fmt.Sprintf("[file: %s]", item.FileItem.FileName))
			} else {
				parts = append(parts, "[file]")
			}
		case MessageItemTypeVideo:
			parts = append(parts, "[video]")
		}
	}

	var mediaRefs []string
	if mediaItem := selectInboundMediaItem(msg); mediaItem != nil {
		ref, err := c.downloadMediaFromItem(ctx, fromUserID, messageID, mediaItem)
		if err != nil {
			logger.ErrorCF("weixin", "Failed to download inbound media", map[string]any{
				"from_user_id": fromUserID,
				"message_id":   messageID,
				"type":         mediaItem.Type,
				"error":        err.Error(),
			})
		} else if ref != "" {
			mediaRefs = append(mediaRefs, ref)
		}
	}

	content := strings.Join(parts, "\n")
	if content == "" && len(mediaRefs) == 0 {
		return
	}

	sender := bus.SenderInfo{
		Platform:    "weixin",
		PlatformID:  fromUserID,
		CanonicalID: identity.BuildCanonicalID("weixin", fromUserID),
		Username:    fromUserID,
		DisplayName: fromUserID,
	}

	if !c.IsAllowedSender(sender) {
		logger.DebugCF("weixin", "Message rejected by allowlist", map[string]any{
			"from_user_id": fromUserID,
		})
		return
	}

	metadata := map[string]string{
		"from_user_id":  fromUserID,
		"context_token": msg.ContextToken,
		"session_id":    msg.SessionID,
	}
	if c.accountKey != "" {
		metadata["weixin_account"] = c.accountKey
	}

	logger.DebugCF("weixin", "Received message", map[string]any{
		"from_user_id": fromUserID,
		"content_len":  len(content),
		"media_count":  len(mediaRefs),
	})

	// Store context_token for outbound reply association
	if msg.ContextToken != "" {
		c.contextTokens.Store(fromUserID, msg.ContextToken)
		c.persistContextTokens()
	}

	inboundCtx := bus.InboundContext{
		Channel:   "weixin",
		Account:   c.accountKey,
		ChatID:    fromUserID,
		ChatType:  "direct",
		SenderID:  fromUserID,
		MessageID: messageID,
		Raw:       metadata,
	}
	if msg.ContextToken != "" {
		inboundCtx.ReplyHandles = map[string]string{
			"context_token": msg.ContextToken,
		}
	}

	c.HandleInboundContext(ctx, fromUserID, content, mediaRefs, inboundCtx, sender)
}

// Send implements channels.Channel by sending a text message to the WeChat user.
func (c *WeixinChannel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}
	if err := c.ensureSessionActive(); err != nil {
		return nil, err
	}

	if msg.Content == "" {
		return nil, nil
	}

	// We need a context_token to send a reply. It should be stored in the conversation metadata.
	// The chat_id is the weixin user_id (from_user_id).
	toUserID := msg.ChatID

	// Retrieve context_token from our per-user map (stored on last inbound)
	contextToken := ""
	if ct, ok := c.contextTokens.Load(toUserID); ok {
		contextToken, _ = ct.(string)
	}

	// If we don't have a context token for this user, we cannot send a valid reply.
	// Treat this as a non-temporary error so the manager doesn't keep retrying.
	if contextToken == "" {
		logger.ErrorCF("weixin", "Missing context token, cannot send message", map[string]any{
			"to_user_id": toUserID,
		})
		return nil, fmt.Errorf("weixin send: %w: missing context token for chat %s", channels.ErrSendFailed, toUserID)
	}

	if err := c.sendTextMessage(ctx, toUserID, contextToken, msg.Content); err != nil {
		logger.ErrorCF("weixin", "Failed to send message", map[string]any{
			"to_user_id": toUserID,
			"error":      err.Error(),
		})
		if c.remainingPause() > 0 {
			return nil, fmt.Errorf("weixin send: %w", channels.ErrSendFailed)
		}
		return nil, fmt.Errorf("weixin send: %w", channels.ErrTemporary)
	}

	return nil, nil
}

// VoiceCapabilities returns the voice capabilities of the channel.
func (c *WeixinChannel) VoiceCapabilities() channels.VoiceCapabilities {
	return channels.VoiceCapabilities{ASR: true, TTS: true}
}
