// Package prompts implements named, versioned prompt management for
// Talyvor Lens. Teams can create, update, list, roll back, diff, and
// resolve prompts by name without redeploying code. Every change creates
// a new version row — rollback never overwrites history.
package prompts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgxDB is the subset of *pgxpool.Pool / pgxmock we depend on. Keeping
// this narrow lets the manager run against either a real pool or a
// mock without import-time coupling.
type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Prompt struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Version     int       `json:"version"`
	Content     string    `json:"content"`
	Description string    `json:"description"`
	WorkspaceID string    `json:"workspace_id"`
	IsActive    bool      `json:"is_active"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type PromptDiff struct {
	Name        string `json:"name"`
	FromVersion int    `json:"from_version"`
	ToVersion   int    `json:"to_version"`
	Added       int    `json:"lines_added"`
	Removed     int    `json:"lines_removed"`
	Diff        string `json:"diff"`
}

type Manager struct {
	pool     pgxDB
	mu       sync.RWMutex
	cache    map[string]*Prompt // active version per workspace+name
	versions map[string]*Prompt // all-versions store; populated by seedForTest only
}

// New wires a Manager to a real connection pool. Nil-safe: when the
// caller passes a nil *pgxpool.Pool (test harnesses, no-DB builds), the
// manager runs cache-only and skips every DB call.
func New(pool *pgxpool.Pool) *Manager {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newManager(db)
}

func newManager(db pgxDB) *Manager {
	return &Manager{
		pool:     db,
		cache:    make(map[string]*Prompt),
		versions: make(map[string]*Prompt),
	}
}

func versionKey(name, workspaceID string, version int) string {
	return fmt.Sprintf("%s:%s:v%d", workspaceID, name, version)
}

const (
	insertSQL = `INSERT INTO prompts
		(id, name, version, content, description, workspace_id, is_active, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	deactivateSQL = `UPDATE prompts SET is_active = false, updated_at = NOW()
		WHERE name = $1 AND workspace_id = $2 AND is_active = true`

	selectActiveSQL = `SELECT id, name, version, content, description,
		workspace_id, is_active, created_by, created_at, updated_at
		FROM prompts WHERE name = $1 AND workspace_id = $2 AND is_active = true
		LIMIT 1`

	selectVersionSQL = `SELECT id, name, version, content, description,
		workspace_id, is_active, created_by, created_at, updated_at
		FROM prompts WHERE name = $1 AND workspace_id = $2 AND version = $3
		LIMIT 1`

	selectHistorySQL = `SELECT id, name, version, content, description,
		workspace_id, is_active, created_by, created_at, updated_at
		FROM prompts WHERE name = $1 AND workspace_id = $2
		ORDER BY version DESC`

	selectListSQL = `SELECT id, name, version, content, description,
		workspace_id, is_active, created_by, created_at, updated_at
		FROM prompts WHERE workspace_id = $1 AND is_active = true
		ORDER BY name, version DESC`
)

func cacheKey(name, workspaceID string) string {
	return workspaceID + ":" + name
}

func (m *Manager) Create(ctx context.Context, p Prompt) (*Prompt, error) {
	if strings.TrimSpace(p.Name) == "" {
		return nil, errors.New("prompts: Name required")
	}
	if p.Content == "" {
		return nil, errors.New("prompts: Content required")
	}
	if p.WorkspaceID == "" {
		p.WorkspaceID = "default"
	}
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	p.Version = 1
	p.IsActive = true
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now

	if m.pool != nil {
		if _, err := m.pool.Exec(ctx, insertSQL,
			p.ID, p.Name, p.Version, p.Content, p.Description,
			p.WorkspaceID, p.IsActive, p.CreatedBy,
		); err != nil {
			return nil, fmt.Errorf("prompts: insert: %w", err)
		}
	}

	m.mu.Lock()
	stored := p
	m.cache[cacheKey(p.Name, p.WorkspaceID)] = &stored
	m.mu.Unlock()
	return &stored, nil
}

func (m *Manager) Update(ctx context.Context, name, workspaceID, content, description string) (*Prompt, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("prompts: name required")
	}
	if content == "" {
		return nil, errors.New("prompts: content required")
	}
	current, err := m.Get(ctx, name, workspaceID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, errors.New("prompts: not found")
	}
	now := time.Now().UTC()
	next := &Prompt{
		ID:          uuid.NewString(),
		Name:        name,
		Version:     current.Version + 1,
		Content:     content,
		Description: description,
		WorkspaceID: workspaceID,
		IsActive:    true,
		CreatedBy:   current.CreatedBy,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if m.pool != nil {
		if _, err := m.pool.Exec(ctx, deactivateSQL, name, workspaceID); err != nil {
			return nil, fmt.Errorf("prompts: deactivate: %w", err)
		}
		if _, err := m.pool.Exec(ctx, insertSQL,
			next.ID, next.Name, next.Version, next.Content, next.Description,
			next.WorkspaceID, next.IsActive, next.CreatedBy,
		); err != nil {
			return nil, fmt.Errorf("prompts: insert v%d: %w", next.Version, err)
		}
	}

	m.mu.Lock()
	m.cache[cacheKey(name, workspaceID)] = next
	m.mu.Unlock()
	return next, nil
}

func (m *Manager) Get(ctx context.Context, name, workspaceID string) (*Prompt, error) {
	m.mu.RLock()
	if p, ok := m.cache[cacheKey(name, workspaceID)]; ok {
		m.mu.RUnlock()
		return p, nil
	}
	m.mu.RUnlock()

	if m.pool == nil {
		return nil, errors.New("prompts: not found")
	}
	var p Prompt
	err := m.pool.QueryRow(ctx, selectActiveSQL, name, workspaceID).Scan(
		&p.ID, &p.Name, &p.Version, &p.Content, &p.Description,
		&p.WorkspaceID, &p.IsActive, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.cache[cacheKey(name, workspaceID)] = &p
	m.mu.Unlock()
	return &p, nil
}

func (m *Manager) GetVersion(ctx context.Context, name, workspaceID string, version int) (*Prompt, error) {
	if m.pool == nil {
		return nil, errors.New("prompts: GetVersion requires DB")
	}
	var p Prompt
	err := m.pool.QueryRow(ctx, selectVersionSQL, name, workspaceID, version).Scan(
		&p.ID, &p.Name, &p.Version, &p.Content, &p.Description,
		&p.WorkspaceID, &p.IsActive, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (m *Manager) List(ctx context.Context, workspaceID string) ([]Prompt, error) {
	if m.pool == nil {
		return nil, nil
	}
	rows, err := m.pool.Query(ctx, selectListSQL, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPrompts(rows)
}

func (m *Manager) History(ctx context.Context, name, workspaceID string) ([]Prompt, error) {
	if m.pool == nil {
		return nil, nil
	}
	rows, err := m.pool.Query(ctx, selectHistorySQL, name, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPrompts(rows)
}

func scanPrompts(rows pgx.Rows) ([]Prompt, error) {
	var out []Prompt
	for rows.Next() {
		var p Prompt
		if err := rows.Scan(
			&p.ID, &p.Name, &p.Version, &p.Content, &p.Description,
			&p.WorkspaceID, &p.IsActive, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Rollback creates a NEW version carrying the content of the target
// version. We never delete history — the audit trail must remain intact
// so reviewers can see when a rollback happened and what shipped before.
func (m *Manager) Rollback(ctx context.Context, name, workspaceID string, targetVersion int) (*Prompt, error) {
	target, err := m.GetVersion(ctx, name, workspaceID, targetVersion)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, errors.New("prompts: target version not found")
	}
	return m.Update(ctx, name, workspaceID, target.Content, target.Description)
}

func (m *Manager) Diff(ctx context.Context, name, workspaceID string, fromVersion, toVersion int) (*PromptDiff, error) {
	from, err := m.versionForDiff(ctx, name, workspaceID, fromVersion)
	if err != nil {
		return nil, err
	}
	to, err := m.versionForDiff(ctx, name, workspaceID, toVersion)
	if err != nil {
		return nil, err
	}
	added, removed, diff := unifiedLineDiff(from.Content, to.Content, fromVersion, toVersion)
	return &PromptDiff{
		Name:        name,
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		Added:       added,
		Removed:     removed,
		Diff:        diff,
	}, nil
}

// versionForDiff lets Diff use cache-seeded prompts in tests while still
// preferring the DB for real callers. Cache hits are name-scoped, so we
// only accept one if its version actually matches.
func (m *Manager) versionForDiff(ctx context.Context, name, workspaceID string, version int) (*Prompt, error) {
	if m.pool != nil {
		return m.GetVersion(ctx, name, workspaceID, version)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.versions[versionKey(name, workspaceID, version)]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("prompts: version %d not found", version)
}

// unifiedLineDiff returns (added, removed, unified-diff-text) computed
// via LCS over line slices. For ten-line prompts the quadratic cost is
// trivial; for thousand-line prompts it's still microseconds.
func unifiedLineDiff(from, to string, fromVer, toVer int) (int, int, string) {
	a := strings.Split(from, "\n")
	b := strings.Split(to, "\n")
	m, n := len(a), len(b)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	var lines []string
	added, removed := 0, 0
	i, j := m, n
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && a[i-1] == b[j-1]:
			lines = append(lines, " "+a[i-1])
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			lines = append(lines, "+"+b[j-1])
			added++
			j--
		default:
			lines = append(lines, "-"+a[i-1])
			removed++
			i--
		}
	}
	// Lines were built tail-first — reverse.
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	header := fmt.Sprintf("@@ -v%d,%d +v%d,%d @@\n", fromVer, m, toVer, n)
	return added, removed, header + strings.Join(lines, "\n")
}

// Resolve scans an OpenAI- or Anthropic-shaped chat request body for a
// system message of the form "lens:prompt:<name>" and swaps in the
// active prompt content for that name in the given workspace. Bodies
// without the literal "lens:prompt:" substring short-circuit before any
// JSON or DB work.
func (m *Manager) Resolve(ctx context.Context, body []byte, workspaceID string) ([]byte, error) {
	if !bytes.Contains(body, []byte("lens:prompt:")) {
		return body, nil
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return body, nil
	}
	changed := false

	// OpenAI: messages[].role == "system" with content starting "lens:prompt:".
	if messages, ok := req["messages"].([]any); ok {
		for i, msg := range messages {
			obj, ok := msg.(map[string]any)
			if !ok {
				continue
			}
			role, _ := obj["role"].(string)
			content, _ := obj["content"].(string)
			if role != "system" || !strings.HasPrefix(content, "lens:prompt:") {
				continue
			}
			name := strings.TrimSpace(strings.TrimPrefix(content, "lens:prompt:"))
			if name == "" {
				continue
			}
			p, err := m.Get(ctx, name, workspaceID)
			if err != nil || p == nil {
				continue
			}
			obj["content"] = p.Content
			messages[i] = obj
			changed = true
		}
		req["messages"] = messages
	}

	// Anthropic: top-level "system" string field.
	if sys, ok := req["system"].(string); ok && strings.HasPrefix(sys, "lens:prompt:") {
		name := strings.TrimSpace(strings.TrimPrefix(sys, "lens:prompt:"))
		if name != "" {
			if p, err := m.Get(ctx, name, workspaceID); err == nil && p != nil {
				req["system"] = p.Content
				changed = true
			}
		}
	}

	if !changed {
		return body, nil
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, err
	}
	return out, nil
}

// seedForTest is used by tests to pre-populate the in-memory store with
// a known prompt without going through the DB layer. Seeds into both
// the active cache (if IsActive) and the versions store so Diff can
// look up specific versions without a DB call.
func (m *Manager) seedForTest(p *Prompt) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.WorkspaceID == "" {
		p.WorkspaceID = "default"
	}
	m.versions[versionKey(p.Name, p.WorkspaceID, p.Version)] = p
	if p.IsActive {
		m.cache[cacheKey(p.Name, p.WorkspaceID)] = p
	}
}
