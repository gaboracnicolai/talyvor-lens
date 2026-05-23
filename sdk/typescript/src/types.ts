/**
 * Type definitions and header constants for the Talyvor Lens SDK.
 *
 * Keep the HEADER_* exports in sync with the Go server's handlers in
 * internal/proxy and internal/workspace — if the server adds a new
 * X-Talyvor-* header, mirror it here so SDK users can set it.
 */

export const HEADER_AUTHORIZATION = "Authorization";
export const HEADER_WORKSPACE = "X-Talyvor-Workspace";
export const HEADER_TEAM = "X-Talyvor-Team";
export const HEADER_FEATURE = "X-Talyvor-Feature";
export const HEADER_SESSION = "X-Talyvor-Session";
export const HEADER_AGENT = "X-Talyvor-Agent";
export const HEADER_BRANCH = "X-Talyvor-Branch";
export const HEADER_PR = "X-Talyvor-PR";
export const HEADER_COMMIT = "X-Talyvor-Commit";
export const HEADER_REPOSITORY = "X-Talyvor-Repository";

/** Aggregate of the optional attribution context fields. */
export interface AttributionContext {
  workspaceId?: string;
  team?: string;
  feature?: string;
  sessionId?: string;
  agentName?: string;
  branch?: string;
  prNumber?: string;
  commit?: string;
  repository?: string;
}
