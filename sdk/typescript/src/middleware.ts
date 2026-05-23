/**
 * Standalone header-injection helper.
 *
 * Use ``injectLensHeaders`` when you already have an HTTP client (fetch,
 * axios, undici) and want to add the Lens routing headers without
 * instantiating the full LensClient. Empty/undefined values are omitted.
 */

import {
  HEADER_AGENT,
  HEADER_AUTHORIZATION,
  HEADER_BRANCH,
  HEADER_COMMIT,
  HEADER_FEATURE,
  HEADER_PR,
  HEADER_REPOSITORY,
  HEADER_SESSION,
  HEADER_TEAM,
  HEADER_WORKSPACE,
} from "./types";

export interface InjectHeadersOptions {
  apiKey: string;
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

// Optional → header map. Listed in one place so the mapping is the same
// in client.ts (which uses injectLensHeaders) and in any direct callers.
const OPTIONAL_HEADER_MAP: ReadonlyArray<
  readonly [keyof InjectHeadersOptions, string]
> = [
  ["team", HEADER_TEAM],
  ["feature", HEADER_FEATURE],
  ["sessionId", HEADER_SESSION],
  ["agentName", HEADER_AGENT],
  ["branch", HEADER_BRANCH],
  ["prNumber", HEADER_PR],
  ["commit", HEADER_COMMIT],
  ["repository", HEADER_REPOSITORY],
];

/**
 * Return a new object with Lens routing headers merged into the input.
 *
 * The ``existing`` map is shallow-copied — the caller's object is never
 * mutated, so passing the same default headers object to multiple
 * requests is safe.
 */
export function injectLensHeaders(
  existing: Record<string, string> = {},
  options: InjectHeadersOptions,
): Record<string, string> {
  const headers: Record<string, string> = { ...existing };
  headers[HEADER_AUTHORIZATION] = `Bearer ${options.apiKey}`;
  headers[HEADER_WORKSPACE] = options.workspaceId || "default";
  for (const [key, headerName] of OPTIONAL_HEADER_MAP) {
    const value = options[key];
    if (value) {
      headers[headerName] = String(value);
    }
  }
  return headers;
}
