export {
  OpenMailWire,
  Emails,
  Domains,
  Webhooks,
  ApiKeys,
  Inboxes,
  Messages,
  Suppressions,
  Pauses,
  ProjectSettings,
  AuditLog,
  DeliveryInsights,
  VERSION,
  DEFAULT_BASE_URL,
} from "./client.js";
export type { ClientOptions } from "./client.js";
export { OpenMailWireError, WebhookVerificationError, WaitTimeoutError, ErrorNames } from "./errors.js";
export type { ErrorName } from "./errors.js";
export { verifyWebhook } from "./webhook.js";
export type { HeadersLike } from "./webhook.js";
export type * from "./types.js";
export { OpenMailWire as default } from "./client.js";
