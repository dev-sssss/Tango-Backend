package rpc

import (
	"github.com/dev-sssss/tango_backend/server/internal/game"
)

// Friends use the database-backed social service in the engine: every
// player gets an 8-character friend code; searching a code (or username)
// sends a request that shows up in the other player's list; accepting adds
// both. Online friends can be invited into a private room.

func (s *Service) social(c *Context) (game.Social, bool) {
	soc, ok := game.SocialFrom(s.NK)
	return soc, ok && c.UserID != ""
}

func socialResult(v map[string]any, err error) (string, error) {
	if err != nil {
		return "", fail(err.Error())
	}
	return enc(game.Normalize(v)), nil
}

func (s *Service) friendsList(c *Context, payload string) (string, error) {
	soc, ok := s.social(c)
	if !ok {
		return refuse("UNAVAILABLE")
	}
	return socialResult(soc.FriendsList(c.context(), c.UserID))
}

func (s *Service) friendsAdd(c *Context, payload string) (string, error) {
	soc, ok := s.social(c)
	if !ok {
		return refuse("UNAVAILABLE")
	}
	q := str(parse(payload), "query", 64)
	if q == "" {
		return refuse("NOT_FOUND")
	}
	return socialResult(soc.FriendAdd(c.context(), c.UserID, q))
}

func (s *Service) friendsFind(c *Context, payload string) (string, error) {
	soc, ok := s.social(c)
	if !ok {
		return refuse("UNAVAILABLE")
	}
	q := str(parse(payload), "query", 64)
	if q == "" {
		return refuse("NOT_FOUND")
	}
	return socialResult(soc.FriendFind(c.context(), c.UserID, q))
}

var friendActions = map[string]bool{"accept": true, "decline": true, "cancel": true, "remove": true, "block": true, "unblock": true}

func (s *Service) friendsAction(c *Context, payload string) (string, error) {
	soc, ok := s.social(c)
	if !ok {
		return refuse("UNAVAILABLE")
	}
	data := parse(payload)
	other, action := str(data, "user_id", 64), str(data, "action", 16)
	if !game.ValidUUID(other) || !friendActions[action] || other == c.UserID {
		return refuse("INVALID_REQUEST")
	}
	return socialResult(soc.FriendAction(c.context(), c.UserID, other, action))
}

func (s *Service) friendsInvite(c *Context, payload string) (string, error) {
	soc, ok := s.social(c)
	if !ok {
		return refuse("UNAVAILABLE")
	}
	data := parse(payload)
	friend, room := str(data, "user_id", 64), str(data, "room_code", 16)
	if !game.ValidUUID(friend) || room == "" || friend == c.UserID {
		return refuse("INVALID_REQUEST")
	}
	return socialResult(soc.RoomInvite(c.context(), c.UserID, friend, room, str(data, "mode", 32)))
}
