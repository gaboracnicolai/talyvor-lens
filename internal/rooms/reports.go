package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/operatoraudit"
)

// reports.go — B32.52: REPORTS, MUTES AND BANS, AND THE OPERATOR'S LOCK AND CLOSE (migration 0220).
//
// A workspace that can read a room reports it, or one of its messages, for one of the marketplace's report reasons or
// harassment or spam; while its report of one thing is open, reporting it again changes nothing. A public room with
// LENS_ROOM_REPORTS_HIDE open reports from different workspaces leaves the public list of rooms (OpenPublic) until the
// operator reviews it. The operator keeps the room, locks it — read-only: no messages, contributions, votes or runs —,
// unlocks it, or closes it: a closed room takes nothing and its wallet is spent no more (whyNotSpend). Keeping, locking
// and closing resolve every open report of the room and its messages. Each operator action writes its operator_audit row
// in the same transaction, and locking, unlocking and closing tell the room with a system message.
//
// The room's owner or an editor mutes a member — it reads the room and posts nothing — or bans it: it is removed, and
// does not join again until it is unbanned (ChangeMember).

// Report reasons (room_reports.reason): the marketplace's listing report reasons, plus harassment and spam.
var reportReasons = map[string]bool{"malicious": true, "injection": true, "secret": true, "personal_data": true,
	"infringing": true, "misleading": true, "harassment": true, "spam": true, "other": true}

const (
	// ReportsHideSetting names the setting that says how many workspaces' open reports take a public room off the list.
	ReportsHideSetting = "LENS_ROOM_REPORTS_HIDE"
	// defaultReportsHide is LENS_ROOM_REPORTS_HIDE's default — a proposal for Nicolai.
	defaultReportsHide = 3

	maxReportDetails = 2000
	maxReviewReason  = 500
)

// The operator's actions on a room.
const (
	ActionKeep   = "keep"
	ActionLock   = "lock"
	ActionUnlock = "unlock"
	ActionClose  = "close"
)

// Report is one report of a room or of a message in it.
type Report struct {
	ID          string    `json:"id"`
	RoomID      string    `json:"room_id"`
	MessageID   string    `json:"message_id,omitempty"` // empty: the room itself
	Reason      string    `json:"reason"`
	Details     string    `json:"details,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	AlreadyMade bool      `json:"already_reported,omitempty"` // this reporter's earlier report of it is still open
}

// ReportDraft is POST /v1/rooms/{id}/reports and /v1/rooms/{id}/messages/{mid}/reports.
type ReportDraft struct {
	Reason  string `json:"reason"`
	Details string `json:"details"`
}

// SetReportsHide sets LENS_ROOM_REPORTS_HIDE; below 1 it is the default.
func (s *Store) SetReportsHide(n int) {
	if n < 1 {
		n = defaultReportsHide
	}
	s.reportsHideAt = n
}

// ReportsHide is LENS_ROOM_REPORTS_HIDE as this store holds it.
func (s *Store) ReportsHide() int { return s.reportsHide() }

func (s *Store) reportsHide() int {
	if s.reportsHideAt < 1 {
		return defaultReportsHide
	}
	return s.reportsHideAt
}

// openReporters counts the workspaces with an open report of room r or of one of its messages.
const openReporters = `(SELECT count(DISTINCT rr.reporter_workspace_id) FROM room_reports rr WHERE rr.room_id = r.id AND rr.resolved_at IS NULL)`

// Report records ws's report of the room, or of its message msgID when that is set. ws must be able to read the room:
// anyone a public room, a member a private one; anyone else gets ErrNotFound.
func (s *Store) Report(ctx context.Context, ws, roomID, msgID string, d ReportDraft) (Report, error) {
	d.Reason, d.Details = strings.TrimSpace(d.Reason), strings.TrimSpace(d.Details)
	if !reportReasons[d.Reason] {
		return Report{}, invalid("reason must be malicious, injection, secret, personal_data, infringing, misleading, harassment, spam or other")
	}
	if len(d.Details) > maxReportDetails {
		return Report{}, invalid("details must be at most %d characters", maxReportDetails)
	}
	out := Report{ID: "rrpt_" + strings.ReplaceAll(uuid.NewString(), "-", ""), RoomID: roomID, MessageID: msgID, Reason: d.Reason, Details: d.Details}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := readable(ctx, tx, ws, roomID); err != nil {
			return err
		}
		if msgID != "" {
			var found bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM room_messages WHERE id = $1 AND room_id = $2)`, msgID, roomID).Scan(&found); err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: no such message in this room", ErrNotFound)
			}
		}
		err := tx.QueryRow(ctx, `INSERT INTO room_reports (id, room_id, message_id, reporter_workspace_id, reason, details)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (room_id, message_id, reporter_workspace_id) WHERE resolved_at IS NULL DO NOTHING
			RETURNING created_at`, out.ID, roomID, msgID, ws, d.Reason, d.Details).Scan(&out.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			out.AlreadyMade = true
			return tx.QueryRow(ctx, `SELECT id, reason, details, created_at FROM room_reports
				WHERE room_id = $1 AND message_id = $2 AND reporter_workspace_id = $3 AND resolved_at IS NULL`, roomID, msgID, ws).
				Scan(&out.ID, &out.Reason, &out.Details, &out.CreatedAt)
		}
		return err
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		err = fmt.Errorf("rooms: report: %w", err)
	}
	return out, err
}

// ReportedRoom is one room in the operator's queue: the room, how many workspaces have an open report of it or its
// messages, and those reports, newest first.
type ReportedRoom struct {
	Room      Room            `json:"room"`
	Reporters int             `json:"reporters"`
	Hidden    bool            `json:"hidden"` // off the public list until it is reviewed
	Reports   []ReportedEntry `json:"reports"`
}

// ReportedEntry is an open report as the operator reads it, with the reported message's text.
type ReportedEntry struct {
	Report
	ReporterWorkspaceID string `json:"reporter_workspace_id"`
	MessageBody         string `json:"message_body,omitempty"`
}

// ReportQueue is the rooms with an open report, those with the most reporters first.
func (s *Store) ReportQueue(ctx context.Context) ([]ReportedRoom, error) {
	out := []ReportedRoom{}
	err := readTx(ctx, s, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+roomCols+`, `+openReporters+` AS reporters FROM rooms r
			WHERE EXISTS (SELECT 1 FROM room_reports rr WHERE rr.room_id = r.id AND rr.resolved_at IS NULL)
			ORDER BY reporters DESC, r.last_activity_at DESC, r.id LIMIT $1`, listLimit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var q ReportedRoom
			if q.Room, err = scanRoom(rows, &q.Reporters); err != nil {
				rows.Close()
				return err
			}
			q.Hidden = q.Room.Visibility == Public && q.Reporters >= s.reportsHide()
			out = append(out, q)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			rows, err := tx.Query(ctx, `SELECT rr.id, rr.room_id, rr.message_id, rr.reason, rr.details, rr.created_at, rr.reporter_workspace_id,
				COALESCE(m.body, '') FROM room_reports rr LEFT JOIN room_messages m ON m.id = rr.message_id AND rr.message_id <> ''
				WHERE rr.room_id = $1 AND rr.resolved_at IS NULL ORDER BY rr.created_at DESC, rr.id`, out[i].Room.ID)
			if err != nil {
				return err
			}
			out[i].Reports = []ReportedEntry{}
			for rows.Next() {
				var e ReportedEntry
				if err := rows.Scan(&e.ID, &e.RoomID, &e.MessageID, &e.Reason, &e.Details, &e.CreatedAt, &e.ReporterWorkspaceID, &e.MessageBody); err != nil {
					rows.Close()
					return err
				}
				out[i].Reports = append(out[i].Reports, e)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("rooms: report queue: %w", err)
	}
	return out, nil
}

// Moderation is the operator's action on a room.
type Moderation struct {
	Action string `json:"action"` // keep, lock, unlock or close
	Reason string `json:"reason"` // required to lock or close
}

// Moderated is what an operator's action did: the room as it now is, the open reports it resolved and its audit row.
type Moderated struct {
	Room            Room                `json:"room"`
	ResolvedReports int                 `json:"resolved_reports"`
	Audit           operatoraudit.Entry `json:"audit"`
}

// Moderate applies the operator actor's action to the room: keep resolves its open reports as kept; lock makes an open
// room read-only and close closes an open or locked one, each resolving its open reports; unlock opens a locked room
// again. A closed room stays closed. The action and its operator_audit row commit together.
func (s *Store) Moderate(ctx context.Context, actor, roomID string, m Moderation) (Moderated, error) {
	actor, m.Action, m.Reason = strings.TrimSpace(actor), strings.TrimSpace(m.Action), strings.TrimSpace(m.Reason)
	switch {
	case actor == "":
		return Moderated{}, invalid("say which operator acts: name them in X-Talyvor-Operator or as actor")
	case m.Action != ActionKeep && m.Action != ActionLock && m.Action != ActionUnlock && m.Action != ActionClose:
		return Moderated{}, invalid("action must be keep, lock, unlock or close")
	case (m.Action == ActionLock || m.Action == ActionClose) && m.Reason == "":
		return Moderated{}, invalid("say why the room is %sd: reason", m.Action)
	case len(m.Reason) > maxReviewReason:
		return Moderated{}, invalid("the reason must be at most %d characters", maxReviewReason)
	}
	var out Moderated
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockRoomAny(ctx, tx, roomID)
		if err != nil {
			return err
		}
		status, resolution, notice := r.Status, "", ""
		switch m.Action {
		case ActionKeep:
			resolution = "kept"
		case ActionLock:
			if r.Status != Open {
				return fmt.Errorf("%w: the room is %s; only an open room is locked", ErrConflict, r.Status)
			}
			status, resolution, notice = Locked, "locked", "Talyvor has locked this room: it is read-only."
		case ActionUnlock:
			if r.Status != Locked {
				return fmt.Errorf("%w: the room is %s, not locked", ErrConflict, r.Status)
			}
			status, notice = Open, "Talyvor has unlocked this room."
		case ActionClose:
			if r.Status == Closed {
				return fmt.Errorf("%w: the room is already closed", ErrConflict)
			}
			status, resolution, notice = Closed, "closed", "Talyvor has closed this room: it takes no more messages, and its budget is spent no more."
		}
		if status != r.Status {
			if _, err := tx.Exec(ctx, `UPDATE rooms SET status = $2 WHERE id = $1`, roomID, status); err != nil {
				return err
			}
		}
		if resolution != "" {
			tag, err := tx.Exec(ctx, `UPDATE room_reports SET resolved_at = now(), resolution = $2, resolved_by = $3
				WHERE room_id = $1 AND resolved_at IS NULL`, roomID, resolution, actor)
			if err != nil {
				return err
			}
			out.ResolvedReports = int(tag.RowsAffected())
		}
		if notice != "" {
			if err := postSystem(ctx, tx, r, notice, map[string]any{"room_status": status}); err != nil {
				return err
			}
		}
		detail := m.Reason
		if out.ResolvedReports > 0 {
			detail = strings.TrimSpace(fmt.Sprintf("%s (resolved %d open reports)", detail, out.ResolvedReports))
		}
		if out.Audit, err = operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: actor, Action: "room." + m.Action,
			Target: roomID, Detail: detail}); err != nil {
			return err
		}
		out.Room, err = scanRoom(tx.QueryRow(ctx, `SELECT `+roomCols+` FROM rooms r WHERE r.id = $1`, roomID))
		return err
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrInvalid) {
		err = fmt.Errorf("rooms: moderate: %w", err)
	}
	return out, err
}

// postSystem posts Talyvor's message to the room r, whose row tx holds, and tells its members' streams.
func postSystem(ctx context.Context, tx pgx.Tx, r Room, body string, refs map[string]any) error {
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		return err
	}
	var scan []byte
	if r.Visibility == Public {
		if scan, err = scanPublic(body); err != nil {
			return err
		}
	}
	id := "rmsg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := tx.Exec(ctx, `INSERT INTO room_messages (id, room_id, author_workspace_id, kind, body, refs, scan)
		VALUES ($1, $2, '', 'system', $3, $4, $5)`, id, r.ID, body, string(refsJSON), jsonText(scan)); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, r.ID, EventPosted, id); err != nil {
		return err
	}
	return touch(ctx, tx, r.ID)
}

// shut answers why the room takes no writes, or nil while it is open: a locked room is read-only and a closed one takes
// nothing.
func shut(r Room) error {
	switch r.Status {
	case Locked:
		return fmt.Errorf("%w: Talyvor has locked the room: it is read-only", ErrConflict)
	case Closed:
		return fmt.Errorf("%w: the room is closed", ErrConflict)
	}
	return nil
}

// banned reports whether ws is banned from the room.
func banned(ctx context.Context, tx pgx.Tx, roomID, ws string) (bool, error) {
	var b bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM room_members WHERE room_id = $1 AND workspace_id = $2 AND banned_at IS NOT NULL)`,
		roomID, ws).Scan(&b)
	return b, err
}

var errBanned = forbidden("the room's owner or an editor has banned you from this room")
