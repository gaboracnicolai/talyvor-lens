/**
 * Talyvor Lens SDK — public surface.
 */

export { LensClient } from "./client";
export type { LensClientOptions } from "./client";
export { injectLensHeaders } from "./middleware";
export type { InjectHeadersOptions } from "./middleware";
export type { AttributionContext } from "./types";
export { AgentBank, AgentBankError, PaymentRefused } from "./agentBank";
export type {
  AgentAccount,
  AgentApproval,
  AgentBalance,
  AgentPayment,
  AgentReceipt,
  AgentRules,
  ReceiptPosting,
} from "./types";
