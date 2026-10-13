package weixin

import (
	"context"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

type pollResult struct {
	bot   *WeixinChannel
	delay time.Duration
}

// runSharedPoller waits for every bot's long-poll on one scheduler.
// Each bot still has its own in-flight getupdates, because iLink scopes that
// call to one token. The waits share the process poller and the HTTP pool
// from sharedTransport, which is the same shape as a single NIO selector.
func runSharedPoller(ctx context.Context, bots []*WeixinChannel) {
	if len(bots) == 0 {
		return
	}
	results := make(chan pollResult, len(bots))
	for _, bot := range bots {
		armWeixinPoll(ctx, bot, 0, results)
	}

	inFlight := len(bots)
	for inFlight > 0 {
		select {
		case <-ctx.Done():
			for inFlight > 0 {
				<-results
				inFlight--
			}
			logger.InfoC("weixin", "Shared Weixin poller stopped")
			return
		case res := <-results:
			inFlight--
			if ctx.Err() != nil || res.delay < 0 {
				continue
			}
			armWeixinPoll(ctx, res.bot, res.delay, results)
			inFlight++
		}
	}
}

func armWeixinPoll(ctx context.Context, bot *WeixinChannel, delay time.Duration, results chan<- pollResult) {
	go func() {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				results <- pollResult{bot: bot, delay: pollStop}
				return
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			results <- pollResult{bot: bot, delay: pollStop}
			return
		}
		results <- pollResult{bot: bot, delay: bot.pollOnce(ctx)}
	}()
}
