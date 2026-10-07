package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/rooms"
)

// room_tools.go — B32.36: AGENTS IN ROOMS OVER MCP.
//
// The room_* tools let an agent's OWN key take part in rooms: list the open ones, join one its owner is a member of,
// read and post messages, propose and fork contributions, vote, and run a contribution or a listing. As with the wallet
// and market tools, the agent is the key's, never an argument, and it acts as its owner's member through the same store
// calls the room routes make. What it posts carries its id and name. A run paid by the room needs its owner's
// membership to be allowed to spend the room's budget; a run it pays itself is its own purchase, judged by its own rules.
// Every call is logged in agent_tool_calls, refused ones too, and counts against LENS_ROOM_MESSAGES_PER_MINUTE.

// RoomDeps gives the room tools what a run needs beside the store: for each payer, a runner that calls the models as it
// (a room's wallet key, or the credential the tool call came in with), its bill and its judge.
type RoomDeps interface {
	RunDeps(ctx context.Context) rooms.DepsFor
}

// SetRooms enables the room tools; they need the agent tools too (SetAgentBank).
func (s *Server) SetRooms(store *rooms.Store, deps RoomDeps) {
	s.rooms, s.roomDeps = store, deps
}

func isRoomTool(name string) bool { return strings.HasPrefix(name, "room_") }

func roomToolDefinitions() []map[string]any {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	num := func(d string) map[string]any { return map[string]any{"type": "integer", "description": d} }
	tool := func(name, desc string, props map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"name": name, "description": desc,
			"inputSchema": map[string]any{"type": "object", "properties": props, "required": required}}
	}
	room := str("the room's id, from room_list")
	contribution := str("a contribution's id: the contribution_id of a contribution message from room_messages")
	price := num("its price per use in µUSD (1 USD = 1,000,000); absent: the room's default price")
	key := str("1 to 128 characters, unique to this contribution: send it again to retry without publishing twice")
	return []map[string]any{
		tool("room_list", "Rooms on Talyvor: the open public rooms, latest activity first, and the rooms your owner is a member of.",
			map[string]any{"topic": str("only open rooms of this topic")}),
		tool("room_join", "Join a room as your owner's agent. Your owner must be a member of it: an agent is in a room only as its owner's.",
			map[string]any{"room_id": room}, "room_id"),
		tool("room_messages", "A room's messages, oldest first: the latest ones, or those before or after a cursor.",
			map[string]any{"room_id": room, "before": num("only messages before this cursor"), "after": num("only messages after this cursor"),
				"limit": num("how many, at most 200; default 50")}, "room_id"),
		tool("room_post", "Post a message to a room you have joined. It shows your name; in a public room a message carrying a secret or personal data is refused.",
			map[string]any{"room_id": room, "body": str("the message, at most 8,000 characters")}, "room_id", "body"),
		tool("room_propose", "Propose a contribution to a room you have joined: a prompt, skill, agent, evaluation or pipeline its members can see, vote on and run.",
			map[string]any{"room_id": room, "kind": str("prompt, skill, agent, evaluation or pipeline"), "title": str("its title"),
				"description": str("what it does"), "artifact": map[string]any{"type": "object", "description": "the contribution itself, as a listing of its kind carries it"},
				"price_usd_micros": price, "idempotency_key": key},
			"room_id", "kind", "title", "artifact"),
		tool("room_fork", "Fork one of a room's contributions as your owner's own contribution, crediting the original at the room's remix share.",
			map[string]any{"room_id": room, "contribution_id": contribution, "title": str("its title; absent: the original's"),
				"description":      str("what it does; absent: the original's"),
				"artifact":         map[string]any{"type": "object", "description": "the changed contribution; absent: the original as it is"},
				"price_usd_micros": price, "idempotency_key": key},
			"room_id", "contribution_id"),
		tool("room_vote", "Vote on one of a room's contributions, +1 or -1, for your owner: it replaces your owner's earlier vote.",
			map[string]any{"room_id": room, "contribution_id": contribution, "vote": num("1 or -1")}, "room_id", "contribution_id", "vote"),
		tool("room_run", "Run one of a room's contributions, or any listing, in the room. pay \"self\" is your own purchase, within your spending rules; "+
			"pay \"room\" spends the room's budget, only when your owner's membership may spend it. The room is told what ran, its charge and who paid.",
			map[string]any{"room_id": room, "target": str("a contribution's id, or a listing's id"),
				"version": num("the version to run; 0 or absent: the latest"), "input": str("your message, for an agent or a skill"),
				"variables": map[string]any{"type": "object", "description": "a prompt's {{variables}}", "additionalProperties": map[string]any{"type": "string"}},
				"model":     str("the model to run it on, when it names none"), "pay": str("self or room"),
				"max_price_usd_micros": num("the most this run may cost, in µUSD; above it nothing runs and nothing is charged")},
			"room_id", "target", "pay"),
	}
}

type roomArgs struct {
	Topic             string             `json:"topic"`
	RoomID            string             `json:"room_id"`
	Before            int64              `json:"before"`
	After             int64              `json:"after"`
	Limit             int                `json:"limit"`
	Body              string             `json:"body"`
	Kind              string             `json:"kind"`
	Title             string             `json:"title"`
	Description       string             `json:"description"`
	Artifact          json.RawMessage    `json:"artifact"`
	PriceUSDMicros    *int64             `json:"price_usd_micros"`
	Parents           []market.ParentRef `json:"parents"`
	IdempotencyKey    string             `json:"idempotency_key"`
	ContributionID    string             `json:"contribution_id"`
	Vote              int                `json:"vote"`
	Target            string             `json:"target"`
	Version           int                `json:"version"`
	Input             string             `json:"input"`
	Variables         map[string]string  `json:"variables"`
	Model             string             `json:"model"`
	Pay               string             `json:"pay"`
	MaxPriceUSDMicros *int64             `json:"max_price_usd_micros"`
}

// roomRefusals are the rooms' answers that say no to what the agent asked, rather than that Lens failed.
var roomRefusals = []error{rooms.ErrNotFound, rooms.ErrInvalid, rooms.ErrForbidden, rooms.ErrConflict, rooms.ErrPlanLimit}

func (s *Server) runRoomTool(ctx context.Context, name, ws, agent string, raw json.RawMessage) (any, error) {
	if s.rooms == nil || s.roomDeps == nil {
		return nil, errors.New("the room tools are not configured")
	}
	var a roomArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, &toolRefusal{"invalid arguments: " + err.Error()}
		}
	}
	var rate *rooms.RateError
	if err := s.rooms.AgentCallRate(ctx, ws, agent); errors.As(err, &rate) {
		return nil, &toolRefusal{fmt.Sprintf("an agent makes at most %d room calls a minute (%s); try again in %ds",
			rate.PerMinute, rooms.MessagesPerMinuteSetting, int(rate.RetryAfter.Seconds()))}
	} else if err != nil {
		return nil, err
	}
	res, err := s.roomTool(rooms.WithAgent(ctx, agent), name, ws, agent, a)
	var need *economy.ApprovalNeededError
	var scan *rooms.ScanRefusal
	switch {
	case errors.As(err, &need):
		return nil, &toolRefusal{err.Error()}
	case errors.As(err, &scan):
		return nil, &toolRefusal{scan.Reason}
	case errors.As(err, &rate):
		return nil, &toolRefusal{err.Error()}
	}
	for _, e := range append(roomRefusals, marketRefusals...) {
		if errors.Is(err, e) {
			return nil, &toolRefusal{err.Error()}
		}
	}
	return res, err
}

func (s *Server) roomTool(ctx context.Context, name, ws, agent string, a roomArgs) (any, error) {
	switch name {
	case "room_list":
		open, err := s.rooms.OpenPublic(ctx, a.Topic)
		if err != nil {
			return nil, err
		}
		joined, err := s.rooms.Joined(ctx, ws)
		return map[string]any{"open": open, "joined": joined}, err
	case "room_join":
		joined, _, err := s.rooms.AddAgent(ctx, ws, a.RoomID, agent)
		return joined, err
	case "room_messages":
		return s.rooms.Messages(ctx, ws, a.RoomID, a.Before, a.After, a.Limit)
	}
	// Everything else acts in the room: the agent must be in it, as its owner's.
	if err := s.rooms.AgentIn(ctx, ws, a.RoomID, agent); err != nil {
		return nil, err
	}
	draft := rooms.ContributionDraft{Kind: a.Kind, Title: a.Title, Description: a.Description, Artifact: a.Artifact,
		PriceUSDMicros: a.PriceUSDMicros, Parents: a.Parents}
	switch name {
	case "room_post":
		return s.rooms.Post(ctx, ws, "", a.RoomID, a.Body)
	case "room_propose":
		c, _, err := s.rooms.Contribute(ctx, ws, "", a.RoomID, a.IdempotencyKey, draft)
		return c, err
	case "room_fork":
		c, _, err := s.rooms.Fork(ctx, ws, "", a.RoomID, a.ContributionID, a.IdempotencyKey, draft)
		return c, err
	case "room_vote":
		return s.rooms.Vote(ctx, ws, a.RoomID, a.ContributionID, a.Vote)
	case "room_run":
		return s.rooms.Run(ctx, s.roomDeps.RunDeps(ctx), ws, "", a.RoomID, rooms.RunRequest{Target: a.Target, Version: a.Version,
			Input: a.Input, Variables: a.Variables, Model: a.Model, Pay: a.Pay, MaxPriceUSDMicros: a.MaxPriceUSDMicros})
	}
	return nil, &toolRefusal{"unknown room tool: " + name}
}
