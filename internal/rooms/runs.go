package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// runs.go — B32.33: RUNS IN A ROOM.
//
// A member runs something in the room — one of the room's contributions, or any listing it may use — or asks the room's
// AI a question with the room's latest messages as its context, and says who pays. Paying room, the room's owner is the
// buyer and the room's wallet the agent: a listing's price is judged against the wallet's rules and goes on the owner's
// marketplace bill with the room and the member recorded (the owner's own contribution charged own, another member's
// billed at its price and earning its author their share, a marketplace listing billed per use or covered by the owner's
// licence), and every model call is made with the wallet's key, so it is judged and spent on the wallet. Paying self, the
// member's own workspace is the buyer and its own credential calls the models. Each run posts a run message to the room
// naming the use, the charge and who paid.

// Who pays for a run.
const (
	PayRoom = "room"
	PaySelf = "self"
)

const (
	// ContextMessagesSetting names the setting that says how many of the room's latest messages an ask sends.
	ContextMessagesSetting = "LENS_ROOM_CONTEXT_MESSAGES"
	// defaultContextMessages is LENS_ROOM_CONTEXT_MESSAGES's default.
	defaultContextMessages = 30
)

// SetContextMessages sets LENS_ROOM_CONTEXT_MESSAGES; below 1 it is the default.
func (s *Store) SetContextMessages(n int) {
	if n < 1 {
		n = defaultContextMessages
	}
	s.contextMessages = n
}

func (s *Store) contextMessageCount() int {
	if s.contextMessages < 1 {
		return defaultContextMessages
	}
	return s.contextMessages
}

// Payer is who pays for a run: the buyer, and — paying room — the room's wallet and the key its model calls are made with.
type Payer struct {
	Pay         string
	WorkspaceID string
	AgentID     string   // the room's wallet; "" paying self
	KeyID       string   // the wallet's key; "" paying self, when the member's own credential calls the models
	KeyScopes   []string // the wallet key's scopes, as it was issued
	Spender     *Spender // paying room: the charge's room and member
}

// DepsFor answers what the marketplace needs to run something paid by p: a runner that calls the models as p, the
// bill p's uses go on, and the judge of p's agent.
type DepsFor func(p Payer) market.UseDeps

// RunRequest is what a member runs in a room.
type RunRequest struct {
	Target            string            `json:"target"` // one of the room's contributions, or any listing
	Version           int               `json:"version"`
	Input             string            `json:"input"`
	Variables         map[string]string `json:"variables"`
	Model             string            `json:"model"`
	Pay               string            `json:"pay"` // room or self
	MaxPriceUSDMicros *int64            `json:"max_price_usd_micros,omitempty"`
}

// AskRequest is a member's question to the room's AI.
type AskRequest struct {
	Question string `json:"question"`
	Model    string `json:"model"`
	Pay      string `json:"pay"` // room or self
}

// RunResult is a run as the member who ran it sees it: the use, or the AI's answer, who paid, and the message the room
// got. MessageError says why the room got none: the run itself ran and stands.
type RunResult struct {
	Use              *market.Use `json:"use,omitempty"`
	Answer           string      `json:"answer,omitempty"`
	Pay              string      `json:"pay"`
	PayerWorkspaceID string      `json:"payer_workspace_id"`
	Message          *Message    `json:"message,omitempty"`
	MessageError     string      `json:"message_error,omitempty"`
}

// runner answers the room ws runs in: ws must be a live member that is not a viewer, and the room not closed. A private
// room answers ErrNotFound to a workspace that is not a member.
func runner(ctx context.Context, tx pgx.Tx, ws, roomID string) (Room, error) {
	if err := readable(ctx, tx, ws, roomID); err != nil {
		return Room{}, err
	}
	r, err := scanRoom(tx.QueryRow(ctx, `SELECT `+roomCols+` FROM rooms r WHERE r.id = $1`, roomID))
	if err != nil {
		return r, err
	}
	me, ok, err := member(ctx, tx, roomID, ws)
	switch {
	case err != nil:
		return r, err
	case !ok:
		return r, forbidden("join the room to run things in it")
	case me.Role == RoleViewer:
		return r, forbidden("a viewer reads the room and does not run things in it")
	case r.Status == Closed:
		return r, fmt.Errorf("%w: the room is closed", ErrConflict)
	}
	return r, nil
}

// payer answers who pays for ws's run in the room: the room's wallet, when ws may spend it, or ws itself.
func (s *Store) payer(ctx context.Context, ws, roomID, pay string) (Payer, error) {
	switch pay {
	case PayRoom:
		sp, err := s.MaySpend(ctx, ws, roomID)
		if err != nil {
			return Payer{}, err
		}
		return Payer{Pay: PayRoom, WorkspaceID: sp.OwnerWorkspaceID, AgentID: sp.WalletAgentID, KeyID: sp.WalletKeyID,
			KeyScopes: sp.WalletKeyScopes, Spender: &sp}, nil
	case PaySelf:
		return Payer{Pay: PaySelf, WorkspaceID: ws}, nil
	}
	return Payer{}, invalid(`say who pays: "pay" is "room", the room's budget, or "self"`)
}

// context carries a charge on the room's wallet to the postings it makes.
func (p Payer) context(ctx context.Context) context.Context {
	if p.Spender != nil {
		return p.Spender.Context(ctx)
	}
	return ctx
}

// Run runs in the room, for ws by user, what in names — one of the room's contributions or a listing ws may use — paid
// as in.Pay says, and posts a run message naming the use, its charge and who paid.
func (s *Store) Run(ctx context.Context, deps DepsFor, ws, user, roomID string, in RunRequest) (RunResult, error) {
	if s.market == nil {
		return RunResult{}, errors.New("rooms: runs are not wired to the marketplace")
	}
	target := strings.TrimSpace(in.Target)
	if target == "" {
		return RunResult{}, invalid("say what to run: target is one of the room's contributions, or a listing")
	}
	listingID, contributionID := target, ""
	err := readTx(ctx, s, func(tx pgx.Tx) error {
		if _, err := runner(ctx, tx, ws, roomID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT id, listing_id FROM room_contributions WHERE id = $1 AND room_id = $2`, target, roomID).
			Scan(&contributionID, &listingID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return RunResult{}, runErr("run", err)
	}
	// What ws runs it must see itself, whoever pays: a room's contribution is run in its own room only.
	l, err := s.market.Get(ctx, ws, listingID)
	if err != nil {
		return RunResult{}, err
	}
	if l.RoomID != "" && l.RoomID != roomID {
		return RunResult{}, fmt.Errorf("%w: that is another room's contribution, run in its own room", ErrNotFound)
	}
	p, err := s.payer(ctx, ws, roomID, in.Pay)
	if err != nil {
		return RunResult{}, runErr("run", err)
	}
	req := market.UseRequest{Version: in.Version, Model: in.Model, Input: in.Input, Variables: in.Variables,
		MaxPriceUSDMicros: in.MaxPriceUSDMicros, RoomID: roomID, ActorWorkspaceID: ws}
	if p.Pay == PaySelf && user != "" {
		req.Person = "user:" + user // a licence of the member's own that counts its people
	}
	u, err := s.market.Use(p.context(ctx), deps(p), p.WorkspaceID, p.AgentID, l.ID, req)
	if err != nil {
		return RunResult{}, err
	}
	res := RunResult{Use: &u, Pay: p.Pay, PayerWorkspaceID: p.WorkspaceID}
	refs := map[string]any{"run": "use", "use_id": u.ID, "listing_id": u.ListingID, "version": u.Version, "charge": u.Charge,
		"price_usd_micros": usdMicros(u.PriceULXC), "pay": p.Pay, "payer_workspace_id": p.WorkspaceID}
	if contributionID != "" {
		refs["contribution_id"] = contributionID
	}
	if p.AgentID != "" {
		refs["wallet_agent_id"] = p.AgentID
	}
	head := fmt.Sprintf("ran %s “%s” %s — %s", article(l.Kind), l.Title, paidBy(p), chargeText(u))
	m, err := s.postRun(ctx, ws, user, roomID, head, useOutput(u), refs)
	if err != nil {
		res.MessageError = err.Error()
	} else {
		res.Message = &m
	}
	return res, nil
}

// Ask sends the room's latest LENS_ROOM_CONTEXT_MESSAGES messages and ws's question to the model, paid as in.Pay says,
// and posts the question and the answer to the room as a run message.
func (s *Store) Ask(ctx context.Context, deps DepsFor, ws, user, roomID string, in AskRequest) (RunResult, error) {
	question := strings.TrimSpace(in.Question)
	switch {
	case question == "":
		return RunResult{}, invalid("ask a question")
	case utf8.RuneCountInString(question) > MaxMessage:
		return RunResult{}, invalid("a question is at most %d characters", MaxMessage)
	case strings.TrimSpace(in.Model) == "":
		return RunResult{}, invalid(`name the model to ask ("model")`)
	}
	var r Room
	var asker string
	var transcript []string
	err := readTx(ctx, s, func(tx pgx.Tx) error {
		var err error
		if r, err = runner(ctx, tx, ws, roomID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT name FROM workspaces WHERE id = $1), $1)`, ws).Scan(&asker); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT COALESCE(w.name, m.author_workspace_id), m.body FROM room_messages m
			LEFT JOIN workspaces w ON w.id = m.author_workspace_id
			WHERE m.room_id = $1 AND m.deleted_at IS NULL ORDER BY m.seq DESC LIMIT $2`, roomID, s.contextMessageCount())
		if err != nil {
			return err
		}
		lines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
			var who, body string
			err := row.Scan(&who, &body)
			return who + ": " + body, err
		})
		for i := len(lines) - 1; i >= 0; i-- {
			transcript = append(transcript, lines[i])
		}
		return err
	})
	if err != nil {
		return RunResult{}, runErr("ask", err)
	}
	// A public room's question is scanned as any message of it is, before the model or the room sees it.
	if r.Visibility == Public {
		if _, err := scanPublic(question); err != nil {
			return RunResult{}, err
		}
	}
	p, err := s.payer(ctx, ws, roomID, in.Pay)
	if err != nil {
		return RunResult{}, runErr("ask", err)
	}
	history := "(no messages yet)"
	if len(transcript) > 0 {
		history = strings.Join(transcript, "\n\n")
	}
	messages := []market.Message{
		{Role: "system", Content: fmt.Sprintf("You are the AI of the room “%s”, a chat where people and their agents build something together. "+
			"The room's latest messages follow, oldest first. Answer the question a member asks, plainly and briefly.", r.Title)},
		{Role: "user", Content: "The room's latest messages:\n\n" + history + "\n\nThe question, from " + asker + ": " + question},
	}
	d := deps(p)
	if d.Runner == nil {
		return RunResult{}, errors.New("rooms: asking is not wired to the models")
	}
	answer, err := d.Runner.Run(p.context(ctx), in.Model, messages)
	if err != nil {
		return RunResult{}, err
	}
	res := RunResult{Answer: answer, Pay: p.Pay, PayerWorkspaceID: p.WorkspaceID}
	refs := map[string]any{"run": "ask", "model": in.Model, "pay": p.Pay, "payer_workspace_id": p.WorkspaceID}
	if p.AgentID != "" {
		refs["wallet_agent_id"] = p.AgentID
	}
	head := fmt.Sprintf("asked the room's AI %s: %s", paidBy(p), question)
	m, err := s.postRun(ctx, ws, user, roomID, head, answer, refs)
	if err != nil {
		res.MessageError = err.Error()
	} else {
		res.Message = &m
	}
	return res, nil
}

// postRun posts ws's run message to the room: head, then what it answered. In a public room the message is scanned as
// any other; an answer carrying a secret or personal data is left out of it, and the member who ran it still has it.
func (s *Store) postRun(ctx context.Context, ws, user, roomID, head, output string, refs map[string]any) (Message, error) {
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		return Message{}, err
	}
	var out Message
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockRoom(ctx, tx, ws, roomID)
		if err != nil {
			return err
		}
		body := clip(strings.TrimSpace(head + "\n\n" + output))
		var scan []byte
		if r.Visibility == Public {
			scan, err = scanPublic(body)
			var refusal *ScanRefusal
			if errors.As(err, &refusal) {
				body = clip(head + "\n\nIts answer is not shown: this room is public and the answer contains " + found(refusal.Scan) +
					". The member who ran it has it.")
				scan, err = scanPublic(body)
			}
			if err != nil {
				return err
			}
		}
		id := "rmsg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if out, err = scanMessage(tx.QueryRow(ctx, `INSERT INTO room_messages AS m (id, room_id, author_workspace_id, author_user_id, kind, body, refs, scan)
			VALUES ($1, $2, $3, $4, 'run', $5, $6, $7) RETURNING `+messageCols, id, roomID, ws, user, body, string(refsJSON), jsonText(scan)), ws); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, roomID, EventPosted, id); err != nil {
			return err
		}
		return touch(ctx, tx, roomID)
	})
	return out, messageErr("run message", err)
}

func runErr(op string, err error) error {
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrForbidden) ||
		errors.Is(err, ErrConflict) || errors.Is(err, ErrPlanLimit) {
		return err
	}
	return fmt.Errorf("rooms: %s: %w", op, err)
}

// found names what a scan found: its secrets and its personal data.
func found(sc market.Scan) string {
	return strings.Join(append(append([]string{}, sc.Secrets...), sc.PersonalData...), ", ")
}

// clip keeps a body within MaxMessage characters.
func clip(body string) string {
	if utf8.RuneCountInString(body) <= MaxMessage {
		return body
	}
	return string([]rune(body)[:MaxMessage-1]) + "…"
}

// paidBy says who paid for a run.
func paidBy(p Payer) string {
	if p.Pay == PayRoom {
		return "on the room's budget"
	}
	return "on their own account"
}

// useOutput is what a use answered: its output, or how its evaluation's cases did.
func useOutput(u market.Use) string {
	if len(u.Cases) == 0 {
		return u.Output
	}
	passed := 0
	for _, c := range u.Cases {
		if c.Passed {
			passed++
		}
	}
	return fmt.Sprintf("%d of %d cases passed", passed, len(u.Cases))
}

// chargeText says what a use cost its payer.
func chargeText(u market.Use) string {
	switch u.Charge {
	case market.ChargeBilled:
		return usd(usdMicros(u.PriceULXC)) + " on the marketplace bill"
	case market.ChargeOwn:
		return "no charge: the payer's own listing"
	case market.ChargeLicensed:
		return "covered by a licence"
	case market.ChargeTrial:
		return "a free trial use"
	}
	return "no charge"
}

// usdMicros is µLXC in µUSD, at the peg.
func usdMicros(ulxc int64) int64 { return ulxc / economy.ULXCPerUSDMicro }

// usd writes µUSD as dollars, to the cent or finer.
func usd(micros int64) string {
	s := strings.TrimRight(fmt.Sprintf("%d.%06d", micros/1_000_000, micros%1_000_000), "0")
	if i := strings.IndexByte(s, '.'); len(s)-i-1 < 2 {
		s += strings.Repeat("0", 2-(len(s)-i-1))
	}
	return "$" + s
}
