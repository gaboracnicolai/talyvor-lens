package rooms

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// agents.go — B32.36: AGENTS IN ROOMS.
//
// An agent acts in a room over MCP with its own key, always as its owner's member: it joins a room its owner is a
// member of (AddAgent), and then posts, proposes, forks, votes and runs there as its owner would, with two
// differences. What it posts carries its id, so every member reads which agent wrote it and its name. And a run it pays
// itself is the agent's own purchase, judged by its own rules — paying room is still for an owner whose membership may
// spend. Each of its room calls counts against LENS_ROOM_MESSAGES_PER_MINUTE.

type agentKey struct{}

// WithAgent marks ctx as agentID acting in a room for its workspace: a message it posts carries the agent's id, and a
// run it pays itself is judged by the agent's own rules. Only the MCP room tools set it, from the calling key.
func WithAgent(ctx context.Context, agentID string) context.Context {
	return context.WithValue(ctx, agentKey{}, agentID)
}

// agentOf is the agent acting in ctx, or "" for a person.
func agentOf(ctx context.Context) string {
	a, _ := ctx.Value(agentKey{}).(string)
	return a
}

// AgentIn answers nil when agentID is in the room as ws's agent and ws is still a live member of it; ErrForbidden
// otherwise, and ErrNotFound for a private room ws may not see.
func (s *Store) AgentIn(ctx context.Context, ws, roomID, agentID string) error {
	err := readTx(ctx, s, func(tx pgx.Tx) error {
		if err := readable(ctx, tx, ws, roomID); err != nil {
			return err
		}
		var in bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM room_member_agents a JOIN room_members m
			ON m.room_id = a.room_id AND m.workspace_id = a.workspace_id AND m.removed_at IS NULL
			WHERE a.room_id = $1 AND a.agent_id = $2 AND a.workspace_id = $3)`, roomID, agentID, ws).Scan(&in); err != nil {
			return err
		}
		if !in {
			return forbidden("this agent is not in the room: join it with room_join first")
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrForbidden) {
		return fmt.Errorf("rooms: agent in room: %w", err)
	}
	return err
}

// AgentCallRate answers a *RateError when agentID has already made LENS_ROOM_MESSAGES_PER_MINUTE room calls in the
// last minute: every call is in agent_tool_calls, refused ones too, so an agent that keeps calling stays refused.
func (s *Store) AgentCallRate(ctx context.Context, ws, agentID string) error {
	var recent int
	var oldest *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT count(*), min(created_at) FROM agent_tool_calls
		WHERE workspace_id = $1 AND agent_id = $2 AND tool LIKE 'room\_%' AND created_at > now() - interval '1 minute'`,
		ws, agentID).Scan(&recent, &oldest); err != nil {
		return fmt.Errorf("rooms: an agent's room calls: %w", err)
	}
	if limit := s.messagesPerMinute(); recent >= limit {
		wait := time.Second
		if oldest != nil {
			wait = max(time.Until(oldest.Add(time.Minute)).Round(time.Second), time.Second)
		}
		return &RateError{PerMinute: limit, RetryAfter: wait}
	}
	return nil
}
