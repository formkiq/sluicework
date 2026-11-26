package worker

import (
	"encoding/json"
	"os"
	"runtime"

	"sluicework/internal/state"
)

func (w WorkstationConfig) settings() state.WorkstationSettings {
	return state.WorkstationSettings{MaxLoadPerCPU: w.MaxLoadPerCPU, MinAvailableMemoryMB: w.MinAvailableMemoryMB, MinIdleSeconds: *w.MinIdleSeconds, RequireACPower: *w.RequireACPower, BuildThreads: w.BuildThreads, EnableMinutes: w.EnableMinutes}
}

func (w WorkstationConfig) withSettings(s state.WorkstationSettings) WorkstationConfig {
	w.MaxLoadPerCPU, w.MinAvailableMemoryMB = s.MaxLoadPerCPU, s.MinAvailableMemoryMB
	w.MinIdleSeconds, w.RequireACPower = &s.MinIdleSeconds, &s.RequireACPower
	w.BuildThreads, w.EnableMinutes = s.BuildThreads, s.EnableMinutes
	return w
}

func cachedSettings(path string, fallback WorkstationConfig) WorkstationConfig {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 4096 {
		return fallback
	}
	var s state.WorkstationSettings
	if json.Unmarshal(b, &s) != nil || s.Validate(runtime.GOOS) != nil {
		return fallback
	}
	return fallback.withSettings(s)
}
