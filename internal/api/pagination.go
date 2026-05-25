package api

// pagination.go — generic offset + cursor pagination shared by
// all list endpoints. Sits beside middleware.go so handlers
// only need one import to wire up "list with page / page_size /
// cursor + total / total_pages / has_next / has_prev".

import (
	"net/http"
	"strconv"
)

// Bounds enforced regardless of what the caller sends.
const (
	DefaultPage     = 1
	DefaultPageSize = 20
	MaxPageSize     = 100
	MinPageSize     = 1
)

// PaginationParams is the parsed view of ?page / ?page_size /
// ?cursor query parameters.
type PaginationParams struct {
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
	Cursor   string `json:"cursor,omitempty"`
}

// Offset is a convenience for translating the (page, page_size)
// pair into a SQL OFFSET value. Always non-negative.
func (p PaginationParams) Offset() int {
	if p.Page < 1 {
		return 0
	}
	return (p.Page - 1) * p.PageSize
}

// Limit is the same value as PageSize but spelled the way SQL
// dialects expect — useful at the query call site.
func (p PaginationParams) Limit() int { return p.PageSize }

// ParsePagination reads (?page, ?page_size, ?cursor) from the
// request and clamps everything to the documented bounds.
// Never returns an error — invalid input falls back to defaults
// so callers don't have to bubble validation failures up.
func ParsePagination(r *http.Request) PaginationParams {
	q := r.URL.Query()
	p := PaginationParams{
		Page:     DefaultPage,
		PageSize: DefaultPageSize,
		Cursor:   q.Get("cursor"),
	}
	if v := q.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			p.Page = n
		}
	}
	if v := q.Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= MinPageSize {
			if n > MaxPageSize {
				n = MaxPageSize
			}
			p.PageSize = n
		}
	}
	return p
}

// PaginatedResponse is the canonical envelope every list
// endpoint returns. `Data` is left as `interface{}` so we can
// stuff anything in — typed JSON encodings still work fine.
type PaginatedResponse struct {
	Data       interface{} `json:"data"`
	Page       int         `json:"page"`
	PageSize   int         `json:"page_size"`
	Total      int         `json:"total"`
	TotalPages int         `json:"total_pages"`
	HasNext    bool        `json:"has_next"`
	HasPrev    bool        `json:"has_prev"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// NewPaginatedResponse builds the envelope from raw inputs.
// total < 0 is treated as "unknown" — total_pages stays 0 and
// the has_next flag becomes data-dependent (true only if the
// caller fills NextCursor afterwards).
func NewPaginatedResponse(data interface{}, page, pageSize, total int) PaginatedResponse {
	if page < 1 {
		page = DefaultPage
	}
	if pageSize < MinPageSize {
		pageSize = DefaultPageSize
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	totalPages := 0
	if total > 0 && pageSize > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	hasNext := false
	hasPrev := page > 1
	if total > 0 {
		hasNext = page < totalPages
	}
	return PaginatedResponse{
		Data:       data,
		Page:       page,
		PageSize:   pageSize,
		Total:      total,
		TotalPages: totalPages,
		HasNext:    hasNext,
		HasPrev:    hasPrev,
	}
}
