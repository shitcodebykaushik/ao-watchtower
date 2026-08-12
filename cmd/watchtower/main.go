// Command watchtower is the future self-hosted Watchtower binary entry point.
package main

import "github.com/agent-orchestrator/ao-watchtower/internal/config"

func main() {
	// Runtime wiring is intentionally deferred until webhook intake is implemented.
	_ = config.Defaults()
}
