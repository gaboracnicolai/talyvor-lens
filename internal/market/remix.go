package market

// B32.25 — the remix button: accepting a remix licence opens the artifact.
//
// A listing's artifact is shown to its owner only (Get). A listing whose remix policy is free or royalty may be opened
// by any workspace that can see it and accepts its remix licence (docs/terms/remix.md): Remix records one
// market_remix_grants row for that workspace and version (migration 0207), holding the share the listing asked at that
// moment, and answers the version's artifact to edit. Accepting again answers the same grant. From then on, declaring
// someone else's listing as a parent needs a grant for the version declared (declareParents), and the edge carries the
// grant's share. A listing whose policy is none is never opened.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNoRemixGrant is a parent the publisher has not accepted the remix licence of.
var ErrNoRemixGrant = fmt.Errorf("%w: accept that listing's remix licence before declaring it as a parent", ErrInvalid)

// RemixGrant is a workspace's acceptance of the remix licence of one version of a listing, and the share of its
// remixes' sales that version is owed, locked when it was accepted.
type RemixGrant struct {
	WorkspaceID string    `json:"workspace_id"`
	ListingID   string    `json:"listing_id"`
	Version     int       `json:"version"`
	ShareBPS    int       `json:"share_bps"`
	AcceptedAt  time.Time `json:"accepted_at"`
}

// Remix is an opened listing version: the artifact to edit and the grant it was opened under (none for the
// listing's own workspace, which needs none).
type Remix struct {
	ListingID string          `json:"listing_id"`
	Version   int             `json:"version"`
	Kind      string          `json:"kind"`
	Title     string          `json:"title"`
	Licence   string          `json:"licence"` // the remix licence accepted (docs/terms/remix.md)
	Grant     *RemixGrant     `json:"grant,omitempty"`
	Artifact  json.RawMessage `json:"artifact"`
}

// RemixLicence names the licence a grant accepts.
const RemixLicence = "docs/terms/remix.md"

// Remix opens version (0: the latest) of a listing for workspaceID to build on: a listing it may see whose remix
// policy is free or royalty, or its own. Opening someone else's records its grant, once per workspace and version,
// with the share the listing asks now.
func (s *Store) Remix(ctx context.Context, workspaceID, listingID string, version int) (Remix, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Remix{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	l, err := visibleListing(ctx, tx, workspaceID, listingID, "FOR SHARE")
	if errors.Is(err, pgx.ErrNoRows) {
		return Remix{}, ErrNotFound
	}
	if err != nil {
		return Remix{}, fmt.Errorf("market: remix: %w", err)
	}
	if version == 0 {
		version = l.LatestVersion
	}
	if version < 1 || version > l.LatestVersion {
		return Remix{}, invalid("listing %s has no version %d", listingID, version)
	}
	out := Remix{ListingID: l.ID, Version: version, Kind: l.Kind, Title: l.Title, Licence: RemixLicence}
	if l.WorkspaceID != workspaceID {
		if l.RemixPolicy == RemixNone {
			return Remix{}, fmt.Errorf("%w (%s)", ErrNotRemixable, listingID)
		}
		share := 0
		if l.RemixPolicy == RemixRoyalty {
			share = l.RemixShareBPS
		}
		if _, err := tx.Exec(ctx, `INSERT INTO market_remix_grants (workspace_id, listing_id, version, share_bps)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, workspaceID, listingID, version, share); err != nil {
			return Remix{}, fmt.Errorf("market: remix: %w", err)
		}
		g, err := remixGrant(ctx, tx, workspaceID, listingID, version)
		if err != nil {
			return Remix{}, err
		}
		out.Grant = &g
	}
	var artifact []byte
	if err := tx.QueryRow(ctx, `SELECT artifact FROM market_listing_versions WHERE listing_id = $1 AND version = $2`,
		listingID, version).Scan(&artifact); err != nil {
		return Remix{}, fmt.Errorf("market: remix: %w", err)
	}
	out.Artifact = artifact
	return out, tx.Commit(ctx)
}

// remixGrant reads workspaceID's grant for one version of a listing; version 0 is the newest version it holds one for.
func remixGrant(ctx context.Context, tx pgx.Tx, workspaceID, listingID string, version int) (RemixGrant, error) {
	g := RemixGrant{WorkspaceID: workspaceID, ListingID: listingID}
	err := tx.QueryRow(ctx, `SELECT version, share_bps, accepted_at FROM market_remix_grants
		WHERE workspace_id = $1 AND listing_id = $2 AND ($3 = 0 OR version = $3) ORDER BY version DESC LIMIT 1`,
		workspaceID, listingID, version).Scan(&g.Version, &g.ShareBPS, &g.AcceptedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, fmt.Errorf("%w (%s)", ErrNoRemixGrant, listingID)
	}
	if err != nil {
		return g, fmt.Errorf("market: remix grant: %w", err)
	}
	return g, nil
}
