// Package market is the Talyvor marketplace's catalog (B20): listings a workspace publishes — an agent,
// a prompt, a skill, an evaluation or a pipeline — each a history of versions.
//
// B20.1 is publishing: a listing carries its owner workspace, kind, title, description, price per use
// (µLXC, 0 for free), visibility and its artifact; publishing a change adds a version and never rewrites
// one (migration 0148 refuses an UPDATE), so what was built on an earlier version keeps working. Every
// publish is scanned (scan.go) and refused for a secret, personal data, or — outside an evaluation — a
// prompt injection.
package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxArtifactBytes bounds one version's artifact.
const MaxArtifactBytes = 256 << 10

var (
	// ErrInvalid wraps every reason a listing or version is not well formed.
	ErrInvalid = errors.New("market: invalid listing")
	// ErrNotFound: no such listing (or version) this caller may see.
	ErrNotFound = errors.New("market: no such listing")
)

// RefusedError is a publish the scan refused; Scan says what it found.
type RefusedError struct{ Scan Scan }

func (e *RefusedError) Error() string {
	return "market: the listing cannot be published: " + e.Scan.Refused
}

// requiredField is the artifact field each kind cannot do without.
var requiredField = map[string]string{
	"agent":      "system_prompt", // with its model and tools, as the seller runs it
	"prompt":     "template",
	"skill":      "instructions",
	"evaluation": "cases", // a non-empty list of test cases
	"pipeline":   "steps", // a non-empty list of steps
}

// Listing is one published listing.
type Listing struct {
	ID              string    `json:"id"`
	WorkspaceID     string    `json:"workspace_id"`
	Kind            string    `json:"kind"`
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	PricePerUseULXC int64     `json:"price_per_use_ulxc"` // 0 is free
	Visibility      string    `json:"visibility"`         // public | unlisted | private
	LatestVersion   int       `json:"latest_version"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	ReviewStatus    string    `json:"review_status"`           // approved | held | taken_down (review.go)
	ReviewReason    string    `json:"review_reason,omitempty"` // why it is held or was taken down
	Versions        []Version `json:"versions,omitempty"`
}

// Version is one version of a listing. Artifact is present only for the listing's owner (B20.2 is how
// anyone else uses it); what a use of it asks for — Needs and Model — is shown to everyone who may use it.
type Version struct {
	Version        int             `json:"version"`
	ArtifactSHA256 string          `json:"artifact_sha256"`
	Changelog      string          `json:"changelog,omitempty"`
	Scan           Scan            `json:"scan"`
	CreatedAt      time.Time       `json:"created_at"`
	Needs          Needs           `json:"needs"`
	Artifact       json.RawMessage `json:"artifact,omitempty"`
}

// Needs is what a use of a version asks the buyer for (B20.3): an input (an agent or a skill), the
// prompt's {{variables}}, and the model it runs on unless the buyer names another ("" when none).
type Needs struct {
	Input     bool     `json:"input"`
	Variables []string `json:"variables"`
	Model     string   `json:"model"`
	Cases     int      `json:"cases,omitempty"` // an evaluation's cases
}

// needsOf reads what a use of kind's artifact asks for, without giving the artifact away.
func needsOf(kind string, raw []byte) Needs {
	n := Needs{Variables: []string{}}
	var a map[string]any
	if json.Unmarshal(raw, &a) != nil {
		return n
	}
	n.Model, _ = a["model"].(string)
	switch kind {
	case "agent", "skill", "pipeline":
		n.Input = true
	case "prompt":
		t, _ := a["template"].(string)
		seen := map[string]bool{}
		for _, m := range variable.FindAllStringSubmatch(t, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				n.Variables = append(n.Variables, m[1])
			}
		}
	case "evaluation":
		cases, _ := a["cases"].([]any)
		n.Cases = len(cases)
	}
	return n
}

// Draft is what a publish carries.
type Draft struct {
	Kind            string          `json:"kind"`
	Title           string          `json:"title"`
	Description     string          `json:"description"`
	PricePerUseULXC int64           `json:"price_per_use_ulxc"`
	Visibility      string          `json:"visibility"`
	Artifact        json.RawMessage `json:"artifact"`
	Changelog       string          `json:"changelog"`
}

// Store reads and writes the catalog.
type Store struct{ pool *pgxpool.Pool }

// NewStore wraps a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// checkArtifact validates artifact for kind and scans it with the listing's words.
func checkArtifact(kind string, artifact json.RawMessage, words ...string) (canonical []byte, sum string, scan Scan, err error) {
	if len(artifact) == 0 || len(artifact) > MaxArtifactBytes {
		return nil, "", scan, invalid("the artifact must be a JSON object of at most %d KB", MaxArtifactBytes>>10)
	}
	var obj map[string]any
	if err := json.Unmarshal(artifact, &obj); err != nil {
		return nil, "", scan, invalid("the artifact must be a JSON object")
	}
	field := requiredField[kind]
	switch v := obj[field].(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, "", scan, invalid("a %s's artifact needs %q", kind, field)
		}
	case []any:
		if len(v) == 0 {
			return nil, "", scan, invalid("a %s's artifact needs a non-empty %q", kind, field)
		}
	default:
		return nil, "", scan, invalid("a %s's artifact needs %q", kind, field)
	}
	canonical, _ = json.Marshal(obj)
	h := sha256.Sum256(canonical)
	scan = scanText(kind, stringsIn(obj, words))
	if scan.Refused != "" {
		return nil, "", scan, &RefusedError{Scan: scan}
	}
	if kind == "pipeline" && scan.Held == "" {
		scan.Held = reviewPipeline(obj)
	}
	return canonical, hex.EncodeToString(h[:]), scan, nil
}

// Publish creates a listing owned by workspaceID, with its first version.
func (s *Store) Publish(ctx context.Context, workspaceID string, d Draft) (Listing, error) {
	if _, ok := requiredField[d.Kind]; !ok {
		return Listing{}, invalid("kind must be agent, prompt, skill, evaluation or pipeline")
	}
	d.Title = strings.TrimSpace(d.Title)
	if d.Title == "" || len(d.Title) > 200 {
		return Listing{}, invalid("a listing needs a title of at most 200 characters")
	}
	if d.PricePerUseULXC < 0 {
		return Listing{}, invalid("the price per use cannot be negative (0 is free)")
	}
	if d.Visibility == "" {
		d.Visibility = "public"
	}
	if d.Visibility != "public" && d.Visibility != "unlisted" && d.Visibility != "private" {
		return Listing{}, invalid("visibility must be public, unlisted or private")
	}
	artifact, sum, scan, err := checkArtifact(d.Kind, d.Artifact, d.Title, d.Description, d.Changelog)
	if err != nil {
		return Listing{}, err
	}
	scanJSON, _ := json.Marshal(scan)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Listing{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	l := Listing{ID: "lst_" + uuid.NewString(), WorkspaceID: workspaceID, Kind: d.Kind, Title: d.Title, Description: d.Description,
		PricePerUseULXC: d.PricePerUseULXC, Visibility: d.Visibility, LatestVersion: 1, ReviewStatus: ReviewApproved}
	if scan.Held != "" {
		l.ReviewStatus, l.ReviewReason = ReviewHeld, scan.Held
	}
	if err := tx.QueryRow(ctx, `INSERT INTO market_listings (id, workspace_id, kind, title, description, price_per_use_ulxc, visibility, review_status, review_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING created_at, updated_at`,
		l.ID, workspaceID, d.Kind, d.Title, d.Description, d.PricePerUseULXC, d.Visibility, l.ReviewStatus, l.ReviewReason).Scan(&l.CreatedAt, &l.UpdatedAt); err != nil {
		return Listing{}, fmt.Errorf("market: publish: %w", err)
	}
	v := Version{Version: 1, ArtifactSHA256: sum, Changelog: d.Changelog, Scan: scan, Needs: needsOf(d.Kind, artifact), Artifact: artifact}
	if err := tx.QueryRow(ctx, `INSERT INTO market_listing_versions (listing_id, version, artifact, artifact_sha256, changelog, scan)
		VALUES ($1, 1, $2, $3, $4, $5) RETURNING created_at`, l.ID, artifact, sum, d.Changelog, scanJSON).Scan(&v.CreatedAt); err != nil {
		return Listing{}, fmt.Errorf("market: publish: %w", err)
	}
	l.Versions = []Version{v}
	return l, tx.Commit(ctx)
}

// PublishVersion adds a version to one of workspaceID's listings; the earlier ones stay as they were. A
// version the review holds holds the whole listing; a clean one never releases a hold (an admin does), and
// a taken-down listing takes no versions.
func (s *Store) PublishVersion(ctx context.Context, workspaceID, listingID string, artifact json.RawMessage, changelog string) (Version, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Version{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var kind, title, description, review string
	var latest int
	err = tx.QueryRow(ctx, `SELECT kind, title, description, latest_version, review_status FROM market_listings WHERE id = $1 AND workspace_id = $2 FOR UPDATE`,
		listingID, workspaceID).Scan(&kind, &title, &description, &latest, &review)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	if err != nil {
		return Version{}, fmt.Errorf("market: version: %w", err)
	}
	if review == ReviewTakenDown {
		return Version{}, ErrTakenDown
	}
	canonical, sum, scan, err := checkArtifact(kind, artifact, title, description, changelog)
	if err != nil {
		return Version{}, err
	}
	scanJSON, _ := json.Marshal(scan)
	v := Version{Version: latest + 1, ArtifactSHA256: sum, Changelog: changelog, Scan: scan, Needs: needsOf(kind, canonical), Artifact: canonical}
	if err := tx.QueryRow(ctx, `INSERT INTO market_listing_versions (listing_id, version, artifact, artifact_sha256, changelog, scan)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`, listingID, v.Version, canonical, sum, changelog, scanJSON).Scan(&v.CreatedAt); err != nil {
		return Version{}, fmt.Errorf("market: version: %w", err)
	}
	if scan.Held != "" {
		if _, err := tx.Exec(ctx, `UPDATE market_listings SET review_status = 'held', review_reason = $2 WHERE id = $1`,
			listingID, fmt.Sprintf("version %d: %s", v.Version, scan.Held)); err != nil {
			return Version{}, fmt.Errorf("market: version: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE market_listings SET latest_version = $2, updated_at = now() WHERE id = $1`, listingID, v.Version); err != nil {
		return Version{}, fmt.Errorf("market: version: %w", err)
	}
	return v, tx.Commit(ctx)
}

const listingColumns = `id, workspace_id, kind, title, description, price_per_use_ulxc, visibility, latest_version, created_at, updated_at, review_status, review_reason`

func scanListing(row pgx.Row) (Listing, error) {
	var l Listing
	err := row.Scan(&l.ID, &l.WorkspaceID, &l.Kind, &l.Title, &l.Description, &l.PricePerUseULXC, &l.Visibility, &l.LatestVersion, &l.CreatedAt, &l.UpdatedAt,
		&l.ReviewStatus, &l.ReviewReason)
	return l, err
}

func (s *Store) list(ctx context.Context, where string, args ...any) ([]Listing, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+listingColumns+` FROM market_listings WHERE `+where+` ORDER BY created_at DESC, id LIMIT 500`, args...)
	if err != nil {
		return nil, fmt.Errorf("market: listings: %w", err)
	}
	defer rows.Close()
	out := []Listing{}
	for rows.Next() {
		l, err := scanListing(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// OwnListings reads workspaceID's listings, whatever their visibility.
func (s *Store) OwnListings(ctx context.Context, workspaceID string) ([]Listing, error) {
	return s.list(ctx, `workspace_id = $1`, workspaceID)
}

// Catalog reads the public listings the review approved, of one kind when kind is set.
func (s *Store) Catalog(ctx context.Context, kind string) ([]Listing, error) {
	if kind != "" {
		return s.list(ctx, `visibility = 'public' AND review_status = 'approved' AND kind = $1`, kind)
	}
	return s.list(ctx, `visibility = 'public' AND review_status = 'approved'`)
}

// hidden says whether viewer may not see l: a private listing, or one held or taken down, is its owner's alone.
func hidden(l Listing, viewer string) bool {
	return l.WorkspaceID != viewer && (l.Visibility == "private" || l.ReviewStatus != ReviewApproved)
}

// Get reads a listing and its versions as viewerWorkspace sees it: a private, held or taken-down listing
// only by its owner, and each version's artifact only for its owner.
func (s *Store) Get(ctx context.Context, viewerWorkspace, listingID string) (Listing, error) {
	l, err := scanListing(s.pool.QueryRow(ctx, `SELECT `+listingColumns+` FROM market_listings WHERE id = $1`, listingID))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hidden(l, viewerWorkspace)) {
		return Listing{}, ErrNotFound
	}
	if err != nil {
		return Listing{}, fmt.Errorf("market: listing: %w", err)
	}
	owner := l.WorkspaceID == viewerWorkspace
	rows, err := s.pool.Query(ctx, `SELECT version, artifact_sha256, changelog, scan, created_at, artifact FROM market_listing_versions
		WHERE listing_id = $1 ORDER BY version`, listingID)
	if err != nil {
		return Listing{}, fmt.Errorf("market: versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v Version
		var scan, artifact []byte
		if err := rows.Scan(&v.Version, &v.ArtifactSHA256, &v.Changelog, &scan, &v.CreatedAt, &artifact); err != nil {
			return Listing{}, err
		}
		_ = json.Unmarshal(scan, &v.Scan)
		v.Needs = needsOf(l.Kind, artifact)
		if owner {
			v.Artifact = artifact
		}
		l.Versions = append(l.Versions, v)
	}
	return l, rows.Err()
}
