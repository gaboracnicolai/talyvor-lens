/**
 * LensClient — drop-in OpenAI client wrapper.
 *
 * Constructs the right headers once at init and hands them to a lazily-
 * instantiated OpenAI client. The OpenAI library is loaded via require()
 * inside ``openai()`` so a TypeScript build doesn't pull the type
 * dependency into the build graph when the consumer hasn't installed it.
 */

import { injectLensHeaders } from "./middleware";
import { HEADER_BRANCH, HEADER_PR } from "./types";

export interface LensClientOptions {
  lensUrl: string;
  apiKey: string;
  workspaceId?: string;
  team?: string;
  feature?: string;
  sessionId?: string;
  agentName?: string;
  branch?: string;
}

export class LensClient {
  public readonly lensUrl: string;
  private readonly apiKey: string;
  private readonly headers: Record<string, string>;

  constructor(options: LensClientOptions) {
    if (!options.lensUrl) {
      throw new Error("lensUrl is required");
    }
    if (!options.apiKey) {
      throw new Error("apiKey is required");
    }
    this.lensUrl = options.lensUrl.replace(/\/$/, "");
    this.apiKey = options.apiKey;
    this.headers = injectLensHeaders(
      {},
      {
        apiKey: options.apiKey,
        workspaceId: options.workspaceId || "default",
        team: options.team,
        feature: options.feature,
        sessionId: options.sessionId,
        agentName: options.agentName,
        branch: options.branch,
      },
    );
  }

  /**
   * Returns an OpenAI client configured to talk to Lens.
   *
   * Usage:
   *   const ai = client.openai();
   *   const r = await ai.chat.completions.create({ ... });
   */
  openai(): unknown {
    // eslint-disable-next-line @typescript-eslint/no-var-requires
    const mod = require("openai");
    const OpenAI = mod.OpenAI ?? mod.default ?? mod;
    return new OpenAI({
      baseURL: `${this.lensUrl}/v1/proxy/openai/v1`,
      apiKey: this.apiKey,
      defaultHeaders: this.headers,
    });
  }

  /** Return a copy of the headers Lens will add to every request. */
  getHeaders(): Record<string, string> {
    return { ...this.headers };
  }

  /**
   * Return a new client with session attribution overridden. Original
   * workspace, team, feature, branch, etc. headers are preserved.
   */
  withSession(sessionId: string, agentName?: string): LensClient {
    return this.derive({ sessionId, agentName });
  }

  /**
   * Return a new client with Git attribution headers set. PR number is
   * optional but commonly set on CI runs.
   */
  withBranch(branch: string, prNumber?: string): LensClient {
    const clone = this.derive({ branch });
    if (prNumber) {
      clone.headers[HEADER_PR] = prNumber;
    }
    return clone;
  }

  /**
   * Construct a new LensClient inheriting state. Internal so callers
   * use the semantic ``with*`` methods.
   */
  private derive(overrides: Partial<LensClientOptions>): LensClient {
    return new LensClient({
      lensUrl: this.lensUrl,
      apiKey: this.apiKey,
      workspaceId: this.headers["X-Talyvor-Workspace"],
      team: this.headers["X-Talyvor-Team"],
      feature: this.headers["X-Talyvor-Feature"],
      sessionId: this.headers["X-Talyvor-Session"],
      agentName: this.headers["X-Talyvor-Agent"],
      branch: this.headers[HEADER_BRANCH],
      ...overrides,
    });
  }
}
