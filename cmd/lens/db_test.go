package main

import "testing"

func TestInjectSSLMode(t *testing.T) {
	tests := []struct {
		name string
		url  string
		mode string
		want string
	}{
		{
			name: "URL without sslmode gets mode injected",
			url:  "postgres://localhost:5432/lens",
			mode: "require",
			want: "postgres://localhost:5432/lens?sslmode=require",
		},
		{
			name: "URL with existing sslmode is unchanged",
			url:  "postgres://localhost:5432/lens?sslmode=disable",
			mode: "require",
			want: "postgres://localhost:5432/lens?sslmode=disable",
		},
		{
			name: "URL with other params gets sslmode appended",
			url:  "postgres://user:pass@host:5432/db?connect_timeout=5",
			mode: "verify-full",
			want: "postgres://user:pass@host:5432/db?connect_timeout=5&sslmode=verify-full",
		},
		{
			name: "postgresql:// scheme handled",
			url:  "postgresql://host/db",
			mode: "require",
			want: "postgresql://host/db?sslmode=require",
		},
		{
			name: "keyword DSN without sslmode gets appended",
			url:  "host=localhost dbname=lens",
			mode: "require",
			want: "host=localhost dbname=lens sslmode=require",
		},
		{
			name: "keyword DSN with existing sslmode is unchanged",
			url:  "host=localhost dbname=lens sslmode=disable",
			mode: "require",
			want: "host=localhost dbname=lens sslmode=disable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := injectSSLMode(tt.url, tt.mode)
			if got != tt.want {
				t.Errorf("injectSSLMode(%q, %q)\n  got  %q\n  want %q", tt.url, tt.mode, got, tt.want)
			}
		})
	}
}
