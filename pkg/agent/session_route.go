package agent

import (
	"fmt"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/routing"
	"github.com/sipeed/picoclaw/pkg/session"
)

func (al *AgentLoop) rememberIsolatedSessionRoute(
	agent *AgentInstance,
	sessionKey string,
	inbound bus.InboundContext,
	route routing.ResolvedRoute,
	scope *session.SessionScope,
) {
	if !agent.Isolated {
		return
	}
	al.isolatedSessionRoutes.Store(sessionKey, DispatchRequest{
		SessionKey:     sessionKey,
		InboundContext: cloneInboundContext(&inbound),
		RouteResult:    cloneResolvedRoute(&route),
		SessionScope:   session.CloneScope(scope),
	})
}

// isolatedContinuationDispatch 重新校验绑定及运行时实例，保留原始会话归属。
func (al *AgentLoop) isolatedContinuationDispatch(
	sessionKey, channel, chatID string,
) (DispatchRequest, *AgentInstance, error) {
	cached, ok := al.isolatedSessionRoutes.Load(sessionKey)
	if !ok {
		return DispatchRequest{}, nil, fmt.Errorf("隔离会话缺少可信路由，拒绝续接")
	}
	snapshot := cached.(DispatchRequest)
	inbound := cloneInboundContext(snapshot.InboundContext)
	if (channel != "" && channel != inbound.Channel) || (chatID != "" && chatID != inbound.ChatID) {
		return DispatchRequest{}, nil, fmt.Errorf("隔离会话的续接目标与原路由不符")
	}
	route, agent, err := al.resolveMessageRoute(bus.InboundMessage{Context: *inbound})
	if err != nil {
		return DispatchRequest{}, nil, err
	}
	if !agent.Isolated || agent.ID != snapshot.SessionScope.AgentID {
		return DispatchRequest{}, nil, fmt.Errorf("隔离会话的绑定已变更，拒绝续接")
	}
	return DispatchRequest{
		SessionKey:     sessionKey,
		InboundContext: inbound,
		RouteResult:    cloneResolvedRoute(&route),
		SessionScope:   session.CloneScope(snapshot.SessionScope),
	}, agent, nil
}
