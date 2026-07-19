package auth

// manager.go — production auth: JWT tokens, scoped keys, and a
// unified Authenticate path that handles four credential
// shapes (JWT, workspace key, global admin key, legacy X-API-Key
// header) behind one entry point.
//
// Sits alongside the existing KeyStore (apikeys.go) — the
// legacy API-key middleware still works for backwards compat.
// New routes use Manager.Authenticate + RequireScope.

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/talyvor/lens/internal/tenant"
)

// ─── constants ───────────────────────────────────

const (
	// JWTIssuer is the iss claim Lens stamps + validates.
	JWTIssuer = "talyvor-lens"

	// DefaultTokenTTL is the JWT expiry when callers leave
	// ttl_hours unset.
	DefaultTokenTTL = 24 * time.Hour

	// MaxTokenTTL caps the longest-lived token we'll mint. 30
	// days matches the spec — anything longer should use an
	// API key.
	MaxTokenTTL = 30 * 24 * time.Hour

	// jwtCacheTTL is how long a validated token stays in the
	// validation cache. Tokens are immutable + signed, so this
	// is purely a CPU optimisation — re-running HS256 on every
	// request is wasteful.
	jwtCacheTTL = 5 * time.Minute
)

// Standard scopes — keep in sync with tenant.ValidScopes plus
// the "keys" scope which only manager.go honours.
const (
	ScopeProxy     = "proxy"
	ScopeAnalytics = "analytics"
	ScopeAdmin     = "admin"
	ScopeKeys      = "keys"
)

// AuthMethod values shipped on AuthContext.AuthMethod.
const (
	MethodJWT          = "jwt"
	MethodWorkspaceKey = "workspace_key"
	MethodGlobalKey    = "global_key"
	MethodLegacyHeader = "legacy_header"
)

// ─── errors ──────────────────────────────────────

// Single ErrInvalidAuth value covers every "creds didn't work"
// case so the wire response can never reveal *which* lookup
// failed — preventing key-enumeration probes.
var ErrInvalidAuth = errors.New("auth: invalid credentials")

// ErrMissingCredentials is the "no Authorization header at all"
// case. The caller decides whether to 401 or fall through.
var ErrMissingCredentials = errors.New("auth: missing credentials")

// ─── types ───────────────────────────────────────

// TokenClaims is the JWT payload Lens signs + verifies.
type TokenClaims struct {
	WorkspaceID string   `json:"workspace_id"`
	UserID      string   `json:"user_id"`
	Scopes      []string `json:"scopes"`
	jwt.RegisteredClaims
}

// AuthContext is the resolved identity downstream handlers see.
// Use GetAuthContext(ctx) to retrieve it after Manager.Middleware
// runs.
type AuthContext struct {
	WorkspaceID string   `json:"workspace_id"`
	UserID      string   `json:"user_id,omitempty"`
	Scopes      []string `json:"scopes"`
	AuthMethod  string   `json:"auth_method"`
	IsAdmin     bool     `json:"is_admin"`
	// APIKeyID is the scoped API key's ID for API-key (workspace-key) auth; EMPTY for JWT/admin/other auth
	// methods. Consumed by the F4 allocator (C.1) to key the per-agent LXC sub-budget on the scoped key —
	// so an EMPTY APIKeyID (JWT/admin) structurally cannot enter the agent-allocation path.
	APIKeyID string `json:"api_key_id,omitempty"`
}

// HasScope reports whether the resolved identity carries `scope`.
// Admin contexts (global key) implicitly carry every scope.
func (a *AuthContext) HasScope(scope string) bool {
	if a == nil {
		return false
	}
	if a.IsAdmin {
		return true
	}
	for _, s := range a.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// ─── token helpers ───────────────────────────────

// GenerateToken signs an ES256 JWT carrying the given scope set.
// Caller controls TTL — Manager.GenerateTokenWithCap caps it
// against MaxTokenTTL at the API-endpoint boundary.
func GenerateToken(workspaceID, userID string, scopes []string, key *ecdsa.PrivateKey, ttl time.Duration) (string, error) {
	if key == nil {
		return "", errors.New("auth: jwt signing key not configured")
	}
	now := time.Now()
	claims := TokenClaims{
		WorkspaceID: workspaceID,
		UserID:      userID,
		Scopes:      append([]string{}, scopes...),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    JWTIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Subject:   userID,
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	t.Header["kid"] = JWTKid
	signed, err := t.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("auth: sign token: %w", err)
	}
	return signed, nil
}

// ValidateToken parses + verifies a signed ES256 JWT. Returns the
// claims when everything checks out, or an error wrapping
// ErrInvalidAuth when anything fails (signature, expiry, issuer).
func ValidateToken(tokenString string, key *ecdsa.PublicKey) (*TokenClaims, error) {
	if tokenString == "" {
		return nil, ErrMissingCredentials
	}
	if key == nil {
		return nil, errors.New("auth: jwt verification key not configured")
	}
	parsed, err := jwt.ParseWithClaims(tokenString, &TokenClaims{},
		func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodECDSA); !ok {
				return nil, fmt.Errorf("auth: unexpected signing method: %v", t.Header["alg"])
			}
			return key, nil
		},
		jwt.WithIssuer(JWTIssuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAuth, err)
	}
	claims, ok := parsed.Claims.(*TokenClaims)
	if !ok || !parsed.Valid {
		return nil, ErrInvalidAuth
	}
	return claims, nil
}

// ─── manager ─────────────────────────────────────

// Manager is the unified authenticator. Wraps the existing
// KeyStore (global keys) and tenant.Store (workspace keys), and
// validates JWTs against the EC public key.
//
// Lifecycle: build once at startup, share across goroutines —
// the JWT cache uses RWMutex.
type Manager struct {
	globalKey   string
	privateKey  *ecdsa.PrivateKey
	publicKey   *ecdsa.PublicKey
	keyStore    *KeyStore
	tenantStore *tenant.Store

	mu       sync.RWMutex
	jwtCache map[string]*jwtCacheEntry
}

type jwtCacheEntry struct {
	claims  *TokenClaims
	expires time.Time
}

func NewManager(globalKey string, privateKey *ecdsa.PrivateKey, keyStore *KeyStore, tenantStore *tenant.Store) *Manager {
	var pub *ecdsa.PublicKey
	if privateKey != nil {
		pub = &privateKey.PublicKey
	}
	return &Manager{
		globalKey:   globalKey,
		privateKey:  privateKey,
		publicKey:   pub,
		keyStore:    keyStore,
		tenantStore: tenantStore,
		jwtCache:    map[string]*jwtCacheEntry{},
	}
}

// PrivateKey exposes the EC signing key so the /v1/auth/token
// endpoint can mint new tokens via GenerateToken.
func (m *Manager) PrivateKey() *ecdsa.PrivateKey { return m.privateKey }

// PublicKey exposes the EC verification key so the /v1/auth/jwks
// endpoint can publish it as a JWKS document.
func (m *Manager) PublicKey() *ecdsa.PublicKey { return m.publicKey }

// ─── credential extraction + Authenticate ───────

// extractCredential returns (raw, location). location is purely
// informational — useful for logging the auth method. Empty raw
// means no credential is present in any recognised location.
func extractCredential(r *http.Request) (raw, location string) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer "), "authorization"
	}
	if v := r.Header.Get("X-Talyvor-Key"); v != "" {
		return v, "x-talyvor-key"
	}
	if v := r.Header.Get("X-API-Key"); v != "" {
		return v, "x-api-key"
	}
	return "", ""
}

// Authenticate is the single entry point. Tries each credential
// shape in priority order:
//  1. JWT bearer (looks like a JWT — 3 segments separated by ".")
//  2. Workspace key (starts with tenant.KeyPrefix)
//  3. Global admin key (exact match with LENS_API_KEY)
//  4. Legacy X-API-Key header → treated as global key fallback
//
// Returns ErrMissingCredentials when no credential is present at
// all, and ErrInvalidAuth (deliberately opaque) for every "we
// recognised this but it's wrong" case.
func (m *Manager) Authenticate(r *http.Request) (*AuthContext, error) {
	raw, _ := extractCredential(r)
	if raw == "" {
		return nil, ErrMissingCredentials
	}

	// JWT detection: a JWT has three dot-separated base64 parts
	// — workspace keys never contain dots, so this is unambiguous.
	if strings.Count(raw, ".") == 2 {
		claims, err := m.validateJWTCached(raw)
		if err != nil {
			return nil, ErrInvalidAuth
		}
		return &AuthContext{
			WorkspaceID: claims.WorkspaceID,
			UserID:      claims.UserID,
			Scopes:      claims.Scopes,
			AuthMethod:  MethodJWT,
			IsAdmin:     false,
		}, nil
	}

	// Workspace key — always validate against the DB, never
	// cached, so a revoked key stops working immediately.
	if strings.HasPrefix(raw, tenant.KeyPrefix) {
		if m.tenantStore == nil {
			return nil, ErrInvalidAuth
		}
		key, err := m.tenantStore.ValidateAPIKey(r.Context(), raw)
		if err != nil {
			return nil, ErrInvalidAuth
		}
		return &AuthContext{
			WorkspaceID: key.WorkspaceID,
			Scopes:      key.Scopes,
			AuthMethod:  MethodWorkspaceKey,
			IsAdmin:     false,
			APIKeyID:    key.ID, // F4 C.0: the scoped key's ID (the per-agent LXC sub-budget key). API-key branch ONLY.
		}, nil
	}

	// Global admin key — exact match. Always carries every
	// scope (HasScope shortcircuits via IsAdmin).
	if m.globalKey != "" && raw == m.globalKey {
		return &AuthContext{
			WorkspaceID: "",
			Scopes:      []string{ScopeProxy, ScopeAnalytics, ScopeAdmin, ScopeKeys},
			AuthMethod:  MethodGlobalKey,
			IsAdmin:     true,
		}, nil
	}

	return nil, ErrInvalidAuth
}

func (m *Manager) validateJWTCached(token string) (*TokenClaims, error) {
	m.mu.RLock()
	if e, ok := m.jwtCache[token]; ok && time.Now().Before(e.expires) {
		m.mu.RUnlock()
		return e.claims, nil
	}
	m.mu.RUnlock()

	claims, err := ValidateToken(token, m.publicKey)
	if err != nil {
		return nil, err
	}
	// The cache entry must never outlive the credential: bounded by
	// min(token exp, cache TTL). Without the bound, a token expiring 1s
	// after first use stayed accepted for up to jwtCacheTTL more — the
	// cache lifetime was silently decoupled from the credential lifetime.
	cacheExp := time.Now().Add(jwtCacheTTL)
	if claims.ExpiresAt != nil && claims.ExpiresAt.Before(cacheExp) {
		cacheExp = claims.ExpiresAt.Time
	}
	m.mu.Lock()
	m.jwtCache[token] = &jwtCacheEntry{claims: claims, expires: cacheExp}
	m.mu.Unlock()
	return claims, nil
}

// ─── middleware ──────────────────────────────────

// authContextCtxKey is the context slot for the resolved
// AuthContext. Use GetAuthContext to read it back.
type authContextCtxKey struct{}

// Middleware returns a chi-compatible handler that requires every
// request to authenticate via Manager.Authenticate. The resolved
// AuthContext lands on the request context.
//
// `onSuccess` and `onFailure` are optional callbacks for the auth
// logging hook described in the spec. main.go wires them to the
// structured slog logger.
func (m *Manager) Middleware(
	onSuccess func(ctx *AuthContext, r *http.Request),
	onFailure func(reason string, r *http.Request),
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authCtx, err := m.Authenticate(r)
			if err != nil {
				if onFailure != nil {
					onFailure(err.Error(), r)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			if onSuccess != nil {
				onSuccess(authCtx, r)
			}
			ctx := context.WithValue(r.Context(), authContextCtxKey{}, authCtx)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// WithAuthContext attaches a resolved AuthContext to ctx — the same slot
// GetAuthContext reads and AuthMiddleware populates internally. Exported so
// handlers and tests can inject identity symmetrically with WithAPIKey (the
// context key type is unexported, so this is the only entry point from outside
// the package).
func WithAuthContext(ctx context.Context, actx *AuthContext) context.Context {
	return context.WithValue(ctx, authContextCtxKey{}, actx)
}

// GetAuthContext returns the resolved AuthContext, or nil when
// the middleware didn't run (or the request was unauthenticated
// public).
func GetAuthContext(ctx context.Context) *AuthContext {
	if v, ok := ctx.Value(authContextCtxKey{}).(*AuthContext); ok {
		return v
	}
	return nil
}

// WorkspaceIdentity resolves the authenticated caller's workspace and whether it
// is the global admin, unifying the two credential slots AuthMiddleware
// populates: the AuthContext (JWT / global key — the only IsAdmin carrier) takes
// precedence, then the APIKey (DB workspace/team keys, never admin). Returns
// ("", false) for an unauthenticated context — fails closed. Handlers in any
// package use this to derive workspace identity from the credential rather than
// caller-supplied input (the #146 cross-tenant authorization fix).
func WorkspaceIdentity(ctx context.Context) (workspaceID string, isAdmin bool) {
	if actx := GetAuthContext(ctx); actx != nil {
		return actx.WorkspaceID, actx.IsAdmin
	}
	if k := GetAPIKey(ctx); k != nil {
		return k.WorkspaceID, false
	}
	return "", false
}

// RequireScope produces a middleware that enforces a single scope. Place it
// AFTER AuthMiddleware — it reads the identity that middleware stamped and 403s
// a credential that presents scopes but not this one.
//
// Two back-compat carve-outs keep existing keys working (they must, or a retrofit
// silently revokes access):
//
//   - An AuthContext with an EMPTY scope set is grandfathered as all-scopes.
//     Every workspace key/JWT minted before enforcement carries an empty set
//     (tenant.ValidateScopes has always accepted empty), so treating empty as
//     "deny" would break every one of them. A key opts into least privilege by
//     being created with an explicit, non-empty scope set.
//   - A request with no AuthContext but a validated fast-path api_keys
//     credential (auth.KeyStore — the APIKey struct has no scope field at all)
//     is grandfathered too. It authenticated; there is nothing to check it
//     against.
//
// Admin is unchanged: the global key sets IsAdmin, which short-circuits HasScope.
// Only a request that reached here with NO credential (i.e. not behind
// AuthMiddleware) is rejected — 401.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ctx := GetAuthContext(r.Context()); ctx != nil {
				// Grandfather empty scopes; HasScope covers admin + an
				// explicit match. A non-empty set missing the scope is denied.
				if len(ctx.Scopes) == 0 || ctx.HasScope(scope) {
					next.ServeHTTP(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"forbidden: missing scope ` + scope + `"}`))
				return
			}
			// No AuthContext ⇒ a fast-path api_keys credential (no scope field);
			// grandfather it. A request with no credential at all still 401s.
			if GetAPIKey(r.Context()) != nil {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		})
	}
}

// ClampTTL applies the MaxTokenTTL ceiling — callers can pass a
// requested duration straight from the API body without
// pre-validating it themselves.
func ClampTTL(requested time.Duration) time.Duration {
	if requested <= 0 {
		return DefaultTokenTTL
	}
	if requested > MaxTokenTTL {
		return MaxTokenTTL
	}
	return requested
}
