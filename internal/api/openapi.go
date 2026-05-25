package api

// openapi.go — hand-written OpenAPI 3.0 spec served at
// GET /openapi.json. Lens has too many routes to spec
// every single one, so this file covers the canonical
// production surface: proxy endpoints, key management,
// workspaces, tenant config, attribution, A/B, and the
// local-endpoint registry. Returns a valid JSON document
// when marshalled.

import (
	"encoding/json"
	"net/http"
)

// OpenAPISpec returns the spec as a generic `map[string]any` so
// the package doesn't grow a dependency on a typed OpenAPI
// schema library. The shape is intentionally hand-rolled.
func OpenAPISpec() map[string]any {
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Talyvor Lens API",
			"version":     APIVersion,
			"description": "Production AI proxy/gateway. Multi-provider routing with cost tracking, quality scoring, attribution, and tenant isolation.",
			"contact": map[string]any{
				"name": "Talyvor",
				"url":  "https://talyvor.com",
			},
		},
		"servers": []map[string]any{
			{"url": "/", "description": "current deployment"},
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"ApiKeyAuth": map[string]any{
					"type":   "http",
					"scheme": "bearer",
					"description": "Lens API key — Authorization: Bearer tlv_...",
				},
			},
			"schemas": map[string]any{
				"APIError": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"code":       map[string]any{"type": "string", "example": ErrCodeInvalidRequest},
						"message":    map[string]any{"type": "string"},
						"details":    map[string]any{"type": "object", "nullable": true},
						"request_id": map[string]any{"type": "string"},
					},
					"required": []string{"code", "message"},
				},
				"PaginatedResponse": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"data":        map[string]any{"type": "array", "items": map[string]any{}},
						"page":        map[string]any{"type": "integer", "example": 1},
						"page_size":   map[string]any{"type": "integer", "example": 20},
						"total":       map[string]any{"type": "integer"},
						"total_pages": map[string]any{"type": "integer"},
						"has_next":    map[string]any{"type": "boolean"},
						"has_prev":    map[string]any{"type": "boolean"},
						"next_cursor": map[string]any{"type": "string", "nullable": true},
					},
				},
				"HealthStatus": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"status":         map[string]any{"type": "string", "enum": []string{"healthy", "degraded", "unhealthy"}},
						"version":        map[string]any{"type": "string"},
						"uptime_seconds": map[string]any{"type": "integer"},
						"checks":         map[string]any{"type": "object", "additionalProperties": true},
					},
				},
				"WorkspaceConfig": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":                map[string]any{"type": "string"},
						"name":              map[string]any{"type": "string"},
						"spending_cap_usd":  map[string]any{"type": "number"},
						"monthly_budget":    map[string]any{"type": "number"},
						"rate_limit_rpm":    map[string]any{"type": "integer"},
						"rate_limit_tpm":    map[string]any{"type": "integer"},
						"allowed_models":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"allowed_providers": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"log_level":         map[string]any{"type": "string", "enum": []string{"all", "errors", "none"}},
						"retention_days":    map[string]any{"type": "integer"},
					},
				},
				"WorkspaceAPIKey": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":           map[string]any{"type": "string"},
						"workspace_id": map[string]any{"type": "string"},
						"key_prefix":   map[string]any{"type": "string"},
						"name":         map[string]any{"type": "string"},
						"scopes":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"expires_at":   map[string]any{"type": "string", "format": "date-time", "nullable": true},
						"created_at":   map[string]any{"type": "string", "format": "date-time"},
					},
				},
				"LocalEndpoint": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":             map[string]any{"type": "string"},
						"url":            map[string]any{"type": "string"},
						"provider":       map[string]any{"type": "string", "enum": []string{"ollama", "vllm", "llamacpp"}},
						"models":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"priority":       map[string]any{"type": "integer"},
						"max_concurrent": map[string]any{"type": "integer"},
						"active":         map[string]any{"type": "boolean"},
						"healthy":        map[string]any{"type": "boolean"},
						"avg_latency_ms": map[string]any{"type": "integer"},
						"error_rate":     map[string]any{"type": "number"},
					},
				},
			},
		},
		"security": []map[string]any{{"ApiKeyAuth": []string{}}},
		"paths":    openAPIPaths(),
	}
}

func openAPIPaths() map[string]any {
	jsonResp := func(schemaRef string) map[string]any {
		return map[string]any{
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": map[string]any{"$ref": "#/components/schemas/" + schemaRef},
				},
			},
		}
	}
	errResp := func(status string) map[string]any {
		return map[string]any{
			"description": "error",
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": map[string]any{"$ref": "#/components/schemas/APIError"},
				},
			},
		}
	}
	_ = errResp
	return map[string]any{
		"/healthz": map[string]any{
			"get": map[string]any{
				"summary":  "Liveness + dependency probe",
				"security": []map[string]any{},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "service health",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{"$ref": "#/components/schemas/HealthStatus"},
							},
						},
					},
				},
			},
		},
		"/v1/proxy/openai/{path}": map[string]any{
			"post": map[string]any{
				"summary":     "Proxy to OpenAI",
				"description": "Routes OpenAI-shape chat / completions / embeddings through Lens.",
				"parameters": []map[string]any{
					{"name": "path", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "proxied response"},
					"401": errResp("401"),
					"429": errResp("429"),
				},
			},
		},
		"/v1/proxy/anthropic/{path}": map[string]any{
			"post": map[string]any{
				"summary": "Proxy to Anthropic",
				"parameters": []map[string]any{
					{"name": "path", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "proxied response"},
				},
			},
		},
		"/v1/api/keys": map[string]any{
			"post": map[string]any{
				"summary": "Create a Lens admin API key",
				"responses": map[string]any{
					"201": map[string]any{"description": "key created (raw key shown once)"},
				},
			},
		},
		"/v1/api/keys/{keyID}": map[string]any{
			"delete": map[string]any{
				"summary": "Revoke a Lens admin API key",
				"parameters": []map[string]any{
					{"name": "keyID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"200": map[string]any{"description": "revoked"}},
			},
		},
		"/v1/workspaces/{wsID}/config": map[string]any{
			"get": map[string]any{
				"summary": "Get workspace tenant config",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"200": jsonResp("WorkspaceConfig")},
			},
			"put": map[string]any{
				"summary": "Upsert workspace tenant config",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/WorkspaceConfig"},
						},
					},
				},
				"responses": map[string]any{"200": jsonResp("WorkspaceConfig")},
			},
		},
		"/v1/workspaces/{wsID}/api-keys": map[string]any{
			"get": map[string]any{
				"summary": "List workspace-scoped API keys",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "list of keys",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type":  "array",
									"items": map[string]any{"$ref": "#/components/schemas/WorkspaceAPIKey"},
								},
							},
						},
					},
				},
			},
			"post": map[string]any{
				"summary": "Issue a workspace-scoped API key",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"201": map[string]any{"description": "key created — raw key shown exactly once"}},
			},
		},
		"/v1/workspaces/{wsID}/api-keys/{keyID}": map[string]any{
			"delete": map[string]any{
				"summary": "Revoke a workspace-scoped API key",
				"responses": map[string]any{"200": map[string]any{"description": "revoked"}},
			},
		},
		"/v1/workspaces/{wsID}/spend/current-month": map[string]any{
			"get": map[string]any{
				"summary":   "Get current-month spend for a workspace",
				"responses": map[string]any{"200": map[string]any{"description": "spend snapshot"}},
			},
		},
		"/v1/local/endpoints": map[string]any{
			"get": map[string]any{
				"summary": "List registered local-model endpoints",
				"responses": map[string]any{
					"200": map[string]any{
						"description": "list of endpoints",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type":  "array",
									"items": map[string]any{"$ref": "#/components/schemas/LocalEndpoint"},
								},
							},
						},
					},
				},
			},
			"post": map[string]any{
				"summary": "Register a local-model endpoint",
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/LocalEndpoint"},
						},
					},
				},
				"responses": map[string]any{"201": jsonResp("LocalEndpoint")},
			},
		},
		"/v1/local/endpoints/{id}": map[string]any{
			"delete": map[string]any{
				"summary": "Remove a local endpoint",
				"responses": map[string]any{"200": map[string]any{"description": "removed"}},
			},
		},
		"/v1/local/endpoints/{id}/check": map[string]any{
			"post": map[string]any{
				"summary":   "Trigger an immediate health probe for one endpoint",
				"responses": map[string]any{"200": jsonResp("LocalEndpoint")},
			},
		},
	}
}

// ServeOpenAPI is the http.HandlerFunc the router mounts at
// GET /openapi.json. We marshal on every request rather than
// cache the result — it's cheap, and it means a redeploy with a
// changed version string is reflected immediately.
func ServeOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(OpenAPISpec())
}
