package game

import "context"

// Social is implemented by NKs that keep friends, invites and presence in the
// database (the production server). The in-memory and Lua-vs-Go oracle
// runtimes do not, and the friend RPCs then answer "unavailable".
type Social interface {
	// FriendsList returns the caller's friends (with presence), requests,
	// blocked players, room invites, friend code and invite progress.
	FriendsList(ctx context.Context, userID string) (map[string]any, error)
	// FriendAdd sends a request to the player with this username or friend
	// code (or accepts theirs if they already asked).
	FriendAdd(ctx context.Context, userID, query string) (map[string]any, error)
	// FriendAction applies accept, decline, cancel, remove, block or unblock
	// to the relation with otherID.
	FriendAction(ctx context.Context, userID, otherID, action string) (map[string]any, error)
	// ReferralClaim records that userID installed through code's invite.
	ReferralClaim(ctx context.Context, userID, code, deviceID, source string) (map[string]any, error)
	// RoomInvite asks a friend to join a private room.
	RoomInvite(ctx context.Context, userID, friendID, roomCode, mode string) (map[string]any, error)
}

// SocialFrom returns nk's Social implementation, if any.
func SocialFrom(nk NK) (Social, bool) {
	s, ok := nk.(Social)
	return s, ok
}
