package screening

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/partners"
)

// maxFile is the most one downloaded list file may hold. The UK list was 50 MB on 8 October 2026.
const maxFile = 256 << 20

// maxMatches is the most matches one name answers, the closest first.
const maxMatches = 20

// ListStatus is one list as the operator sees it: the copy in force, and the last download's outcome.
type ListStatus struct {
	List        string     `json:"list"`
	SourceURL   string     `json:"source_url"`
	Entries     int        `json:"entries"`
	Published   string     `json:"published,omitempty"` // the date the list prints
	LoadedAt    *time.Time `json:"loaded_at,omitempty"`
	AttemptedAt *time.Time `json:"attempted_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	FailedAt    *time.Time `json:"failed_at,omitempty"`
	// Stale: the last download failed, and the copy in force is the one loaded at LoadedAt.
	Stale bool `json:"stale"`
}

// Store keeps the lists in Postgres, downloads them, and matches names against the copy in force.
type Store struct {
	pool      *pgxpool.Pool
	client    *http.Client
	sources   []Source
	threshold int // basis points
	maxAge    time.Duration

	mu      sync.Mutex
	version string
	idx     []indexed
	exact   map[string][]int
}

type indexed struct {
	Entry
	norm string
}

// NewStore keeps sources' lists in pool, calls a name close to a listed one when their similarity is at least
// threshold (0 to 1: LENS_SCREENING_FUZZY_THRESHOLD), and calls the lists stale once one was last downloaded more than
// maxAge ago (LENS_SCREENING_MAX_AGE_HOURS).
func NewStore(pool *pgxpool.Pool, sources []Source, threshold float64, maxAge time.Duration) *Store {
	return &Store{pool: pool, client: &http.Client{Timeout: 5 * time.Minute}, sources: sources, threshold: int(threshold*10_000 + 0.5),
		maxAge: maxAge}
}

// ThresholdBPS is the similarity, in basis points, at which a name is held for review.
func (s *Store) ThresholdBPS() int { return s.threshold }

// ListsAge is how long ago the list downloaded longest ago was last downloaded — -1 while a list has never been — and
// whether that makes the lists stale: older than LENS_SCREENING_MAX_AGE_HOURS, or not all downloaded yet (B37.4).
func (s *Store) ListsAge(ctx context.Context) (age time.Duration, stale bool, err error) {
	lists := make([]string, len(s.sources))
	for i, src := range s.sources {
		lists[i] = src.List
	}
	var loaded int
	var secs int64
	if err := s.pool.QueryRow(ctx, `SELECT count(loaded_at), coalesce(floor(extract(epoch FROM max(now() - loaded_at))), 0)::bigint
		FROM screening_lists WHERE list = ANY($1)`, lists).Scan(&loaded, &secs); err != nil {
		return 0, true, fmt.Errorf("screening: how old the lists are: %w", err)
	}
	if loaded < len(lists) {
		return -1, true, nil
	}
	age = time.Duration(secs) * time.Second
	return age, age > s.maxAge, nil
}

// Refresh downloads every list. A list whose download fails, or whose file does not read as the list, keeps the copy
// already loaded, and its failure is recorded; the answer names every list that failed.
func (s *Store) Refresh(ctx context.Context) error {
	var errs []error
	for _, src := range s.sources {
		if err := s.refresh(ctx, src); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.List, err))
			if _, rerr := s.pool.Exec(ctx, `INSERT INTO screening_lists (list, source_url, attempted_at, last_error, failed_at)
				VALUES ($1, $2, now(), $3, now())
				ON CONFLICT (list) DO UPDATE SET attempted_at = now(), last_error = EXCLUDED.last_error, failed_at = now()`,
				src.List, strings.Join(src.URLs, " "), err.Error()); rerr != nil {
				errs = append(errs, fmt.Errorf("%s: record the failure: %w", src.List, rerr))
			}
		}
	}
	return errors.Join(errs...)
}

// RefreshDue downloads each list loaded a day ago or more, or never — but not one whose download failed within the
// hour, so a list that will not download is tried each hour rather than on every call. It answers what Refresh does
// for the lists it tried.
func (s *Store) RefreshDue(ctx context.Context) error {
	var due []Source
	for _, src := range s.sources {
		var isDue bool
		err := s.pool.QueryRow(ctx, `SELECT (loaded_at IS NULL OR loaded_at <= now() - interval '1 day')
			AND (failed_at IS NULL OR failed_at <= now() - interval '1 hour') FROM screening_lists WHERE list = $1`, src.List).Scan(&isDue)
		if errors.Is(err, pgx.ErrNoRows) {
			isDue = true
		} else if err != nil {
			return fmt.Errorf("screening: is %s due: %w", src.List, err)
		}
		if isDue {
			due = append(due, src)
		}
	}
	return (&Store{pool: s.pool, client: s.client, sources: due}).Refresh(ctx)
}

func (s *Store) refresh(ctx context.Context, src Source) error {
	files := make([][]byte, 0, len(src.URLs))
	for _, u := range src.URLs {
		b, err := s.download(ctx, u)
		if err != nil {
			return err
		}
		files = append(files, b)
	}
	entries, published, err := src.Parse(files)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("%w: it holds no names", ErrNotTheList)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO screening_lists (list, source_url, version, entries, published, loaded_at, attempted_at)
		VALUES ($1, $2, 1, $3, $4, now(), now())
		ON CONFLICT (list) DO UPDATE SET source_url = EXCLUDED.source_url, version = screening_lists.version + 1,
			entries = EXCLUDED.entries, published = EXCLUDED.published, loaded_at = now(), attempted_at = now(),
			last_error = '', failed_at = NULL`,
		src.List, strings.Join(src.URLs, " "), len(entries), published); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM screening_entries WHERE list = $1`, src.List); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	rows := make([][]any, len(entries))
	for i, e := range entries {
		rows[i] = []any{src.List, e.EntryID, e.Name, Normalise(e.Name), e.Alias, e.Weak, e.Kind}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"screening_entries"}, []string{"list", "entry_id", "name", "norm", "alias", "weak", "kind"},
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

func (s *Store) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("download %s: larger than %d bytes", url, maxFile)
	}
	return b, nil
}

// Lists is every list as the operator sees it, a list never downloaded included.
func (s *Store) Lists(ctx context.Context) ([]ListStatus, error) {
	rows, err := s.pool.Query(ctx, `SELECT list, source_url, entries, published, loaded_at, attempted_at, last_error, failed_at
		FROM screening_lists ORDER BY list`)
	if err != nil {
		return nil, fmt.Errorf("screening: lists: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := []ListStatus{}
	for rows.Next() {
		var l ListStatus
		if err := rows.Scan(&l.List, &l.SourceURL, &l.Entries, &l.Published, &l.LoadedAt, &l.AttemptedAt, &l.LastError, &l.FailedAt); err != nil {
			return nil, fmt.Errorf("screening: lists: %w", err)
		}
		l.Stale = l.LastError != ""
		seen[l.List] = true
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("screening: lists: %w", err)
	}
	for _, src := range s.sources {
		if !seen[src.List] {
			out = append(out, ListStatus{List: src.List, SourceURL: strings.Join(src.URLs, " "), Stale: true, LastError: "not downloaded yet"})
		}
	}
	return out, nil
}

// load brings the index up to the copy in force, when a list has been loaded since it was built. It answers
// partners.ErrScreeningUnavailable while a list has never been loaded: a payment is not screened against half the lists.
func (s *Store) load(ctx context.Context) error {
	var version string
	var loaded []string
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(string_agg(list || ':' || version, ',' ORDER BY list), ''),
		coalesce(array_agg(list) FILTER (WHERE version > 0), '{}') FROM screening_lists WHERE version > 0`).Scan(&version, &loaded); err != nil {
		return fmt.Errorf("%w: %v", partners.ErrScreeningUnavailable, err)
	}
	for _, src := range s.sources {
		if !contains(loaded, src.List) {
			return fmt.Errorf("%w: the %s sanctions list has not been downloaded yet", partners.ErrScreeningUnavailable, src.List)
		}
	}
	if version == s.version {
		return nil
	}
	rows, err := s.pool.Query(ctx, `SELECT list, entry_id, name, norm, alias, weak, kind FROM screening_entries`)
	if err != nil {
		return fmt.Errorf("%w: %v", partners.ErrScreeningUnavailable, err)
	}
	defer rows.Close()
	idx, exact := []indexed{}, map[string][]int{}
	for rows.Next() {
		var e indexed
		if err := rows.Scan(&e.List, &e.EntryID, &e.Name, &e.norm, &e.Alias, &e.Weak, &e.Kind); err != nil {
			return fmt.Errorf("%w: %v", partners.ErrScreeningUnavailable, err)
		}
		exact[e.norm] = append(exact[e.norm], len(idx))
		idx = append(idx, e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: %v", partners.ErrScreeningUnavailable, err)
	}
	s.version, s.idx, s.exact = version, idx, exact
	return nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Match is every listed entry name is close to — exact on the normalised name (score 10,000), or at least the
// threshold similar — one match per entry, the closest first. It is partners.ScreeningList.
func (s *Store) Match(ctx context.Context, name string) ([]partners.ScreeningMatch, error) {
	n := Normalise(name)
	if n == "" {
		return nil, fmt.Errorf("%w: %q has no letters or digits to screen", partners.ErrInvalid, name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(ctx); err != nil {
		return nil, err
	}
	type key struct{ list, entry string }
	best := map[key]partners.ScreeningMatch{}
	consider := func(e indexed, score int) {
		k := key{e.List, e.EntryID}
		m := partners.ScreeningMatch{Name: name, List: e.List, Entry: e.EntryID, ScoreBPS: score, Listed: e.Name, Weak: e.Weak}
		if b, ok := best[k]; !ok || score > b.ScoreBPS || score == b.ScoreBPS && b.Weak && !e.Weak {
			best[k] = m
		}
	}
	for _, i := range s.exact[n] {
		consider(s.idx[i], 10_000)
	}
	for _, e := range s.idx {
		if e.norm == n || !couldReach(len(n), len(e.norm), s.threshold) {
			continue
		}
		if score := similarity(n, e.norm); score >= s.threshold {
			consider(e, score)
		}
	}
	out := make([]partners.ScreeningMatch, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ScoreBPS != out[j].ScoreBPS {
			return out[i].ScoreBPS > out[j].ScoreBPS
		}
		return out[i].List+out[i].Entry < out[j].List+out[j].Entry
	})
	if len(out) > maxMatches {
		out = out[:maxMatches]
	}
	return out, nil
}
