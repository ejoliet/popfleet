// Package contrib embeds the two enrollment payloads so the broker can serve
// them: agent.sh at /agent.sh (curl .../agent.sh | sh -s -- <TOKEN>) and the
// reference Python agent at /agent.py (the app-hook path — the panel tells you
// to run `python3 agent.py`, so the broker has to be able to hand you the file).
package contrib

import _ "embed"

//go:embed agent.sh
var AgentSH []byte

//go:embed agent.py
var AgentPY []byte
