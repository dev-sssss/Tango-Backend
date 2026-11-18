package tango

// Bots fill empty public seats after the matchmaking wait. They play at an
// easy-to-medium level and look like normal players (ordinary names,
// avatars and trophy counts); nothing sent to clients marks them as bots.

var botNames = []string{
	"Arjun", "Priya", "Rahul", "Ananya", "Karthik", "Divya", "Vikram", "Sneha", "Rohan", "Meera",
	"Aditya", "Kavya", "Siddharth", "Pooja", "Nikhil", "Lakshmi", "Varun", "Ishita", "Harish", "Neha",
	"Suresh", "Deepa", "Manoj", "Swathi", "Ajay", "Keerthi", "Sanjay", "Nandini", "Gokul", "Riya",
	"Daniel", "Sofia", "Omar", "Lena", "Mateo", "Aisha", "Lucas", "Yuki", "Noah", "Zara",
	"Ethan", "Mia", "Liam", "Sara", "Adam", "Nora", "Ravi", "Anjali", "Vijay", "Bhavana",
}

var botSuffixes = []string{"", "", "", "_07", "99", "_k", "21", "_x", "777", "_pro", "123", "_r"}

var botAvatars = []string{"avatar_kai", "avatar_nova", "avatar_iris", "avatar_ren"}
var botFrames = []string{"frame_gold", "frame_gold", "frame_starlight", "frame_azure", "frame_sakura", "frame_ember", "frame_koi"}
var botBadges = []string{"badge_none", "badge_none", "badge_sunset", "badge_ocean", "badge_forest", "badge_star"}

// NewBotPlayer makes a bot seat with a believable profile. Trophies scale a
// little with the entry fee so high-stake tables look experienced.
func NewBotPlayer(rng RNG, id string, fee int64, taken map[string]bool) *Player {
	name := ""
	for i := 0; i < 20; i++ {
		name = botNames[rng.RandInt(0, len(botNames)-1)] + botSuffixes[rng.RandInt(0, len(botSuffixes)-1)]
		if !taken[name] {
			break
		}
	}
	base := 40 + int(fee/10)
	return &Player{UserID: id, Name: name, Bot: true,
		Avatar: botAvatars[rng.RandInt(0, len(botAvatars)-1)], Frame: botFrames[rng.RandInt(0, len(botFrames)-1)],
		Badge: botBadges[rng.RandInt(0, len(botBadges)-1)], CardBack: "back_default", Trophies: base + rng.RandInt(0, 600)}
}

// Move is a bot decision.
type Move struct {
	Kind   string // play, draw, color, keep, play_drawn, challenge, swap
	CardID int
	Target int // swap: whose hand to take
	Color  string
	Tango  bool // press TANGO! first
}

// DecideBot chooses a move for the seat whose phase it is.
func DecideBot(t *Table, seat int, rng RNG) Move {
	p := t.Players[seat]
	switch t.Phase {
	case PhaseColor:
		return Move{Kind: "color", Color: BestColor(p.Hand, rng)}
	case PhaseSwap:
		return Move{Kind: "swap", Target: t.FewestCards(seat)}
	case PhaseRoulette:
		return Move{Kind: "color", Color: Colors[rng.RandInt(0, len(Colors)-1)]}
	case PhaseKeep:
		if rng.RandInt(1, 100) <= 70 {
			return Move{Kind: "play_drawn", Color: BestColor(without(p.Hand, t.Drawn.ID), rng), Tango: armTango(p, rng)}
		}
		return Move{Kind: "keep"}
	}
	if t.Pending > 0 {
		if t.CanChallenge(seat) && rng.RandInt(1, 100) <= 15 {
			return Move{Kind: "challenge"}
		}
		var best *Card
		for i := range p.Hand {
			c := p.Hand[i]
			if !t.Legal(c) {
				continue
			}
			// Save black cards: prefer a matching coloured +2.
			if best == nil || (best.IsWild() && !c.IsWild()) {
				best = &p.Hand[i]
			}
		}
		if best != nil && rng.RandInt(1, 100) <= 85 {
			return Move{Kind: "play", CardID: best.ID, Color: BestColor(without(p.Hand, best.ID), rng), Tango: armTango(p, rng)}
		}
		return Move{Kind: "draw"}
	}
	var playable []Card
	for _, c := range p.Hand {
		if t.Legal(c) {
			playable = append(playable, c)
		}
	}
	if len(playable) == 0 {
		return Move{Kind: "draw"}
	}
	choice := playable[rng.RandInt(0, len(playable)-1)]
	if rng.RandInt(1, 100) > 10 { // 10% of the time just play anything
		choice = pickCard(t, seat, playable)
	}
	return Move{Kind: "play", CardID: choice.ID, Color: BestColor(without(p.Hand, choice.ID), rng), Tango: armTango(p, rng)}
}

// armTango: bots about to go down to one card remember TANGO! most times.
func armTango(p *Player, rng RNG) bool {
	return len(p.Hand) == 2 && rng.RandInt(1, 100) <= 85
}

func without(h []Card, id int) []Card {
	out := make([]Card, 0, len(h))
	for _, c := range h {
		if c.ID != id {
			out = append(out, c)
		}
	}
	return out
}

// pickCard scores playable cards: keep black cards for later, use action
// cards against an opponent close to winning, otherwise follow the colour
// the bot holds most of.
func pickCard(t *Table, seat int, playable []Card) Card {
	p := t.Players[seat]
	colorCount := map[string]int{}
	for _, c := range p.Hand {
		colorCount[c.Color]++
	}
	next := t.Players[t.Next(seat, 1)]
	threat := next.Team != p.Team && len(next.Hand) <= 2
	best, bestScore := playable[0], -1<<30
	for _, c := range playable {
		score := colorCount[c.Color] * 10
		switch {
		case c.IsWildPlus():
			score = -50
			if threat {
				score = 80
			}
		case c.IsWild():
			score = -40
		case c.Kind == KindDiscardAll:
			score += colorCount[c.Color] * 15 // empties a whole colour
		case c.Kind != KindNumber:
			score += 5
			if threat {
				score += 60
			}
		default:
			score += c.Value // dump high numbers first
		}
		if score > bestScore {
			best, bestScore = c, score
		}
	}
	return best
}
