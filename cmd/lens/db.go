package main

import (
	"net/url"
	"strings"
)

// injectSSLMode returns rawURL with sslmode set to mode, unless the URL
// already contains an explicit sslmode parameter (in which case the
// operator's choice is honoured without modification).
//
// It handles two DSN formats:
//
//	URL form   postgres://host/db?key=val  (most common for LENS_DATABASE_URL)
//	Keyword form   host=h dbname=d sslmode=…  (less common but valid for pgx)
//
// If rawURL looks like a URL but cannot be parsed, the keyword-form fallback
// appends " sslmode=<mode>" — pgx will reject the overall URL at connect time
// if it is truly malformed, which is the right place for that error.
func injectSSLMode(rawURL, mode string) string {
	if strings.HasPrefix(rawURL, "postgres://") || strings.HasPrefix(rawURL, "postgresql://") {
		u, err := url.Parse(rawURL)
		if err == nil {
			q := u.Query()
			if q.Get("sslmode") == "" {
				q.Set("sslmode", mode)
				u.RawQuery = q.Encode()
			}
			return u.String()
		}
		// url.Parse failed — fall through to keyword-form handling.
	}
	// Keyword=value DSN: append only if sslmode is not already present.
	if strings.Contains(rawURL, "sslmode=") {
		return rawURL
	}
	return rawURL + " sslmode=" + mode
}
