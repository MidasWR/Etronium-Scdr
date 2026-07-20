// example-stateful — V5 demo app. Increments a counter, persists state to
// $ETRONIUM_STATE_DUMP every PERIOD seconds. On restart (lord crashed),
// it reads state and resumes from the last persisted counter.
//
// This demonstrates *user-space fault tolerance* without kernel patches,
// CRIU, or process migration: the application itself owns its state and
// the scheduler's recovery path re-launches it from the same argv/env on
// a healthy lord, where the application reads its own state file.
//
// Usage:
//
//	ETRONIUM_STATE_DUMP=/tmp/etronium/state/pid.json ./example-stateful
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type AppState struct {
	Counter     int64     `json:"counter"`
	LastUpdated time.Time `json:"last_updated"`
	Restarts    int       `json:"restarts"`
}

const defaultStatePath = "/tmp/etronium-state.json"

func main() {
	statePath := os.Getenv("ETRONIUM_STATE_DUMP")
	if statePath == "" {
		statePath = defaultStatePath
	}

	state := AppState{}
	if data, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			fmt.Fprintln(os.Stderr, "warning: corrupt state file:", err)
		} else {
			fmt.Printf("recovered: counter=%d restarts=%d last_updated=%s\n",
				state.Counter, state.Restarts, state.LastUpdated)
			state.Restarts++
		}
	} else if os.IsNotExist(err) {
		fmt.Println("fresh start, no previous state")
	} else {
		fmt.Fprintln(os.Stderr, "state read error:", err)
	}

	if err := os.MkdirAll(parentDir(statePath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		os.Exit(1)
	}

	// Write state every 2 seconds, incrementing counter.
	tick := time.NewTicker(2 * time.Second)
	pid := os.Getpid()
	fmt.Printf("started pid=%d state_path=%s restarts=%d\n", pid, statePath, state.Restarts)
	defer fmt.Printf("exiting pid=%d last_counter=%d restarts=%d\n", pid, state.Counter, state.Restarts)

	// Track ticks: every 2s increment counter and write state.
	for {
		select {
		case <-tick.C:
			state.Counter++
			state.LastUpdated = time.Now().UTC()
			data, _ := json.MarshalIndent(state, "", "  ")
			tmp := statePath + ".tmp"
			if err := os.WriteFile(tmp, data, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, "write tmp:", err)
				continue
			}
			if err := os.Rename(tmp, statePath); err != nil {
				fmt.Fprintln(os.Stderr, "rename:", err)
			}
			if state.Counter%5 == 0 {
				fmt.Printf("pid=%d counter=%d at=%s\n",
					pid, state.Counter, state.LastUpdated.Format(time.RFC3339))
			}
		}
	}
}

func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

// (example-stateful runs forever; cmd/lord's WaitProcess is the way to
// stop it from the tenant side; on real faults, scheduler will detect
// lord disconnect and respawn.)
//
// To verify fault tolerance manually:
//   1. etronium process spawn --exec ./example-stateful --arg "" \
//        --state-dump=/tmp/etronium/state/<pid>.json --max-restarts=3
//   2. kill -9 the lord's goroutine / restart the lord container.
//   3. Wait, see process_counter resume from last value (not 0).
//
// NB: this app intentionally does NOT flush after every tick — state is
// durable on disk via os.Rename atomic. Loss window = PERIOD.
