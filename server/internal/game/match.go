package game

import "context"

// Presence identifies one client session inside a match. Node is the gateway
// node that holds the session's socket.
type Presence struct {
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	Username  string `json:"username"`
	Node      string `json:"node"`
}

// MatchMessage is client input delivered to the match loop.
type MatchMessage struct {
	Sender        Presence
	OpCode        int64
	Data          []byte
	HasData       bool
	Reliable      bool
	ReceiveTimeMs int64
}

// Dispatcher sends output from a match. A nil presence list means every
// presence in the match. Presences that are not in the match are ignored.
type Dispatcher interface {
	Broadcast(opCode int64, data []byte, to []Presence)
	Kick(presences []Presence)
	UpdateLabel(label string)
}

// MatchContext carries per-match context into handler calls.
type MatchContext struct {
	Ctx     context.Context
	MatchID string
	Node    string
	Env     map[string]string
}

// Match is an authoritative match implementation. Calls are never
// concurrent; the host serialises them on the match's goroutine.
//
// Returning false from Loop/Signal (keep=false) ends the match, matching the
// Lua convention of returning a nil state. A panic in any method also ends
// the match.
type Match interface {
	Init(mc *MatchContext, params map[string]any) (tickRate int, label string)
	JoinAttempt(mc *MatchContext, d Dispatcher, tick int64, p Presence, metadata map[string]string) (allow bool, reason string)
	Join(mc *MatchContext, d Dispatcher, tick int64, presences []Presence)
	Leave(mc *MatchContext, d Dispatcher, tick int64, presences []Presence)
	Loop(mc *MatchContext, d Dispatcher, tick int64, messages []MatchMessage) (keep bool)
	Signal(mc *MatchContext, d Dispatcher, tick int64, data string) (keep bool, response string)
	Terminate(mc *MatchContext, d Dispatcher, tick int64, graceSeconds int)

	// SchemaVersion and MarshalState support live handover and crash
	// restore. The bytes must be accepted by the factory's Restore on any
	// node running the same or a newer build.
	SchemaVersion() int
	MarshalState() ([]byte, error)
}

// MatchFactory creates or restores matches of one module name.
type MatchFactory interface {
	Name() string
	New(nk NK) Match
	// MaxSchemaVersion is the newest state schema this build understands.
	MaxSchemaVersion() int
	Restore(nk NK, schemaVersion int, state []byte) (Match, error)
}
