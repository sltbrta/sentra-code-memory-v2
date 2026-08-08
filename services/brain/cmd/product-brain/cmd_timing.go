// CLI action timers: wall-clock duration for every product-brain command.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// actionTimer measures one top-level CLI verb (create, ask, gardener, …).
type actionTimer struct {
	command string
	t0      time.Time
	once    sync.Once
}

var (
	currentTimerMu sync.Mutex
	currentTimer   *actionTimer
)

// startAction begins timing for a CLI command. Call Finish (via defer) always.
func startAction(command string) *actionTimer {
	a := &actionTimer{command: command, t0: time.Now()}
	currentTimerMu.Lock()
	currentTimer = a
	currentTimerMu.Unlock()
	return a
}

// MS returns elapsed milliseconds since start.
func (a *actionTimer) MS() int64 {
	if a == nil {
		return 0
	}
	return time.Since(a.t0).Milliseconds()
}

// Finish emits a machine-readable timing line on stderr (once).
// Format: {"event":"cli_timing","command":"ask","duration_ms":1234,"product_owned":true}
// Disable with OUROBOROS_CLI_TIMING=0.
func (a *actionTimer) Finish() {
	if a == nil {
		return
	}
	a.once.Do(func() {
		if os.Getenv("OUROBOROS_CLI_TIMING") == "0" {
			return
		}
		ms := a.MS()
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{
			"event":         "cli_timing",
			"command":       a.command,
			"duration_ms":   ms,
			"product_owned": true,
		})
		// Human one-liner on stderr as well (tools can ignore non-JSON lines).
		fmt.Fprintf(os.Stderr, "timing  %-16s %dms\n", a.command, ms)
	})
}

// withTiming merges duration_ms / cli_action into a JSON object map when absent.
func withTiming(m map[string]any) map[string]any {
	if m == nil {
		m = map[string]any{}
	}
	currentTimerMu.Lock()
	a := currentTimer
	currentTimerMu.Unlock()
	if a == nil {
		return m
	}
	if _, ok := m["duration_ms"]; !ok {
		m["duration_ms"] = a.MS()
	}
	if _, ok := m["cli_action"]; !ok {
		m["cli_action"] = a.command
	}
	return m
}

// emitJSON encodes v to stdout. If v is map[string]any, injects timing fields.
func emitJSON(v any) {
	switch m := v.(type) {
	case map[string]any:
		_ = json.NewEncoder(os.Stdout).Encode(withTiming(m))
	default:
		_ = json.NewEncoder(os.Stdout).Encode(v)
	}
}
