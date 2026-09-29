// Package documents holds the files a workspace uploads for its chats to reference by id (B18.13).
//
// A chat request carries a document inline as base64, so the proxy's 4 MiB body cap limits it to about
// 2.5 MB. Uploaded here (POST /v1/documents, up to MaxBytes), a document is referenced from a chat by the
// id this returns — `{"type":"document","source":{"type":"file","file_id":"tdoc_…"}}` (Anthropic shape)
// or `{"type":"file","file":{"file_id":"tdoc_…"}}` (OpenAI shape) — and the proxy converts it on the way
// to the model. A document is only ever read for the workspace that uploaded it.
package documents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/distill"
)

// MaxBytes is the largest document the upload route accepts: 25 MB.
const MaxBytes = 25 << 20

// IDPrefix starts every uploaded document's id, which is how a chat request's reference is told apart
// from a provider's own file id (those pass through untouched).
const IDPrefix = "tdoc_"

// ErrNotFound is a document id this workspace did not upload.
var ErrNotFound = errors.New("document not found")

// Document is an uploaded document without its bytes.
type Document struct {
	ID         string    `json:"id"`
	MediaType  string    `json:"media_type"`
	Filename   string    `json:"filename"`
	SizeBytes  int64     `json:"size_bytes"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// Store keeps uploaded documents in Postgres (migrations/0167).
type Store struct{ pool *pgxpool.Pool }

// NewStore wraps pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Save stores a document for a workspace.
func (s *Store) Save(ctx context.Context, workspaceID, mediaType, filename string, content []byte) (Document, error) {
	d := Document{ID: IDPrefix + uuid.NewString(), MediaType: mediaType, Filename: filename, SizeBytes: int64(len(content))}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO uploaded_documents (id, workspace_id, media_type, filename, content, size_bytes)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
		d.ID, workspaceID, mediaType, filename, content, d.SizeBytes).Scan(&d.UploadedAt)
	return d, err
}

// Content is a document's bytes and media type, for the workspace that uploaded it only.
func (s *Store) Content(ctx context.Context, workspaceID, id string) ([]byte, string, error) {
	var content []byte
	var mediaType string
	err := s.pool.QueryRow(ctx, `SELECT content, media_type FROM uploaded_documents WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID).Scan(&content, &mediaType)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	return content, mediaType, err
}

// UploadHandler is POST /v1/documents: the request body is the file, its Content-Type the document's
// media type (any format the proxy can convert), and ?filename= optionally names it. It answers 201 with
// the Document whose id a chat request references.
func UploadHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, _ := auth.WorkspaceIdentity(r.Context())
		if ws == "" {
			writeErr(w, http.StatusUnauthorized, "a workspace credential is required")
			return
		}
		mediaType := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
		if _, ok := distill.FormatFromMediaType(mediaType); !ok {
			writeErr(w, http.StatusUnsupportedMediaType, fmt.Sprintf("Content-Type %q is not a document Talyvor can read: "+
				"send PDF, Word, Excel, PowerPoint, HTML, CSV, JSON, XML, Markdown or plain text", mediaType))
			return
		}
		content, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBytes))
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeErr(w, http.StatusRequestEntityTooLarge, "a document can be at most 25 MB")
			return
		case err != nil:
			writeErr(w, http.StatusBadRequest, "could not read the document")
			return
		case len(content) == 0:
			writeErr(w, http.StatusBadRequest, "the request body is the document, and it is empty")
			return
		}
		d, err := s.Save(r.Context(), ws, mediaType, r.URL.Query().Get("filename"), content)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not store the document")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(d)
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
