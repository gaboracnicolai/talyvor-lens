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
	Versions        []Version `json:"versions,omitempty"`
}

// Version is one version of a listing. Artifact is present only for the listing's owner (B20.2 is how
// anyone else uses it).
type Version struct {
	Version        int             `json:"version"`
	ArtifactSHA256 string          `json:"artifact_sha256"`
	Changelog      string          `json:"changelog,omitempty"`
	Scan           Scan            `json:"scan"`
	CreatedAt      time.Time       `json:"created_at"`
	Artifact       json.RawMessage `json:"artifact,omitempty"`
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
		PricePerUseULXC: d.PricePerUseULXC, Visibility: d.Visibility, LatestVersion: 1}
	if err := tx.QueryRow(ctx, `INSERT INTO market_listings (id, workspace_id, kind, title, description, price_per_use_ulxc, visibility)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at, updated_at`,
		l.ID, workspaceID, d.Kind, d.Title, d.Description, d.PricePerUseULXC, d.Visibility).Scan(&l.CreatedAt, &l.UpdatedAt); err != nil {
		return Listing{}, fmt.Errorf("market: publish: %w", err)
	}
	v := Version{Version: 1, ArtifactSHA256: sum, Changelog: d.Changelog, Scan: scan, Artifact: artifact}
	if err := tx.QueryRow(ctx, `INSERT INTO market_listing_versions (listing_id, version, artifact, artifact_sha256, changelog, scan)
		VALUES ($1, 1, $2, $3, $4, $5) RETURNING created_at`, l.ID, artifact, sum, d.Changelog, scanJSON).Scan(&v.CreatedAt); err != nil {
		return Listing{}, fmt.Errorf("market: publish: %w", err)
	}
	l.Versions = []Version{v}
	return l, tx.Commit(ctx)
}

// PublishVersion adds a version to one of workspaceID's listings; the earlier ones stay as they were.
func (s *Store) PublishVersion(ctx context.Context, workspaceID, listingID string, artifact json.RawMessage, changelog string) (Version, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Version{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var kind, title, description string
	var latest int
	err = tx.QueryRow(ctx, `SELECT kind, title, description, latest_version FROM market_listings WHERE id = $1 AND workspace_id = $2 FOR UPDATE`,
		listingID, workspaceID).Scan(&kind, &title, &description, &latest)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	if err != nil {
		return Version{}, fmt.Errorf("market: version: %w", err)
	}
	canonical, sum, scan, err := checkArtifact(kind, artifact, title, description, changelog)
	if err != nil {
		return Version{}, err
	}
	scanJSON, _ := json.Marshal(scan)
	v := Version{Version: latest + 1, ArtifactSHA256: sum, Changelog: changelog, Scan: scan, Artifact: canonical}
	if err := tx.QueryRow(ctx, `INSERT INTO market_listing_versions (listing_id, version, artifact, artifact_sha256, changelog, scan)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`, listingID, v.Version, canonical, sum, changelog, scanJSON).Scan(&v.CreatedAt); err != nil {
		return Version{}, fmt.Errorf("market: version: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE market_listings SET latest_version = $2, updated_at = now() WHERE id = $1`, listingID, v.Version); err != nil {
		return Version{}, fmt.Errorf("market: version: %w", err)
	}
	return v, tx.Commit(ctx)
}

const listingColumns = `id, workspace_id, kind, title, description, price_per_use_ulxc, visibility, latest_version, created_at, updated_at`

func scanListing(row pgx.Row) (Listing, error) {
	var l Listing
	err := row.Scan(&l.ID, &l.WorkspaceID, &l.Kind, &l.Title, &l.Description, &l.PricePerUseULXC, &l.Visibility, &l.LatestVersion, &l.CreatedAt, &l.UpdatedAt)
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

// Catalog reads the public listings, of one kind when kind is set.
func (s *Store) Catalog(ctx context.Context, kind string) ([]Listing, error) {
	if kind != "" {
		return s.list(ctx, `visibility = 'public' AND kind = $1`, kind)
	}
	return s.list(ctx, `visibility = 'public'`)
}

// Get reads a listing and its versions as viewerWorkspace sees it: a private listing only by its owner,
// and each version's artifact only for its owner.
func (s *Store) Get(ctx context.Context, viewerWorkspace, listingID string) (Listing, error) {
	l, err := scanListing(s.pool.QueryRow(ctx, `SELECT `+listingColumns+` FROM market_listings WHERE id = $1`, listingID))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && l.Visibility == "private" && l.WorkspaceID != viewerWorkspace) {
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
		if owner {
			v.Artifact = artifact
		}
		l.Versions = append(l.Versions, v)
	}
	return l, rows.Err()
}
