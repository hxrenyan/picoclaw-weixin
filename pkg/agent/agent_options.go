package agent

import (
	"github.com/sipeed/picoclaw/pkg/config"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
)

// AgentLoopOption configures an AgentLoop at construction time.
type AgentLoopOption func(*AgentLoop)

// WithPrimaryModelConfig 传递启动 provider 对应的配置，保留同名别名的候选身份。
func WithPrimaryModelConfig(modelCfg *config.ModelConfig) AgentLoopOption {
	return func(al *AgentLoop) {
		al.primaryModelConfig = modelCfg
	}
}

// WithRuntimeEvents injects the runtime event bus used for new observation APIs.
//
// The injected bus is treated as externally owned and will not be closed by
// AgentLoop.Close. Passing nil leaves the default owned runtime bus enabled.
func WithRuntimeEvents(bus runtimeevents.Bus) AgentLoopOption {
	return func(al *AgentLoop) {
		if bus == nil {
			return
		}
		al.runtimeEvents = bus
		al.ownsRuntimeEvents = false
	}
}
