// Request and response shapes. Requests use camelCase and are converted to
// the API's snake_case; responses are returned exactly as the API sends them.

/** Lets a string union stay open to values added by newer servers. */
type Open<T extends string> = T | (string & {});

// ---------------------------------------------------------------- emails

export interface Tag {
  name: string;
  value: string;
}

export interface Attachment {
  filename: string;
  /** A Uint8Array (or Buffer) is base64-encoded for you; a string must already be base64. */
  content: Uint8Array | string;
  contentType?: string;
  /** Makes the attachment inline: reference it from the HTML as `cid:<contentId>`. */
  contentId?: string;
}

export interface SendEmailParams {
  /** `"Name <addr@domain>"` or `"addr@domain"`, on a verified domain. */
  from: string;
  to: string | string[];
  cc?: string | string[];
  bcc?: string | string[];
  replyTo?: string | string[];
  subject: string;
  html?: string;
  text?: string;
  headers?: Record<string, string>;
  tags?: Tag[];
  /** At most 20 files, 10 MB decoded in total. Not allowed in `batch`. */
  attachments?: Attachment[];
  /** Send later: in the future and at most 30 days ahead. */
  scheduledAt?: Date | string;
  /** No new attempts after this time. Default 48 h, at most 7 days. */
  expiresAt?: Date | string;
  /** Retry an attempt whose outcome is unknown (the default), or stop. */
  retryUncertain?: boolean;
  /**
   * `transactional` (the default) or `broadcast`. Broadcast mail gets a signed
   * one-click unsubscribe and is not sent to addresses that unsubscribed.
   */
  stream?: Stream;
}

export type Stream = Open<"transactional" | "broadcast">;

/** The `require_approval` rules that matched. */
export type HoldReason = Open<"always" | "first_contact" | "has_attachments" | "has_links" | "recipients_over">;

export type BatchEmailParams = Omit<SendEmailParams, "attachments">;

export interface SendOptions {
  /** The same key with the same request returns the same id. */
  idempotencyKey?: string;
}

export interface SendRawParams {
  /** The complete MIME message. The SDK base64-encodes it. */
  raw: string | Uint8Array;
  mailFrom?: string;
  /** Envelope recipients. Default: the To and Cc headers. */
  rcpts?: string[];
}

export interface ListEmailsParams {
  limit?: number;
  /** An email id: list emails older than it (use `next_before`). */
  before?: string;
  apiKeyId?: string;
}

export interface SendEmailResponse {
  id: string;
  /** Present (and `true`) when the email waits for approval; nothing is sent until then. */
  held?: true;
  /** With `held`: the key's `require_approval` rules that matched. */
  hold_reasons?: HoldReason[];
}

export interface BatchResponse {
  data: SendEmailResponse[];
}

/** What `POST /emails/check` answers: what a send would do, without sending. */
export interface CheckResult {
  object: "email_check";
  /** No refusal and no warning. */
  ok: boolean;
  /** What `emails.send` would do with these params now. */
  would: Open<"send" | "hold" | "refuse">;
  stream: Stream;
  /** The message size in bytes as it would be sent (estimated). */
  size: number;
  hold_reasons: HoldReason[];
  /** Every refusal that would apply, in the API's error shape. */
  refusals: { statusCode: number; name: string; message: string }[];
  warnings: {
    code: Open<
      | "no_text_part"
      | "html_too_large"
      | "link_domain_mismatch"
      | "missing_unsubscribe"
      | "domain_not_verified"
      | "large_message"
    >;
    message: string;
  }[];
}

export interface ListHeldParams {
  /** 1 to 50, default 20. */
  limit?: number;
  /** `next_before` of the previous page. */
  before?: string;
}

/** An email waiting for approval. */
export interface HeldEmail {
  object: "held_email";
  id: string;
  /** The key that sent it. */
  api_key_id?: string | null;
  from: string;
  to: string[];
  cc?: string[];
  bcc?: string[];
  subject: string;
  stream: Stream;
  hold_reasons: HoldReason[];
  /** The recipients waiting for approval. */
  recipients: string[];
  created_at: string;
  /** Held mail nobody decides on expires then. */
  expires_at: string;
  scheduled_at?: string | null;
  /** The first 2000 characters of the text, for the reviewer. */
  text: string;
  text_truncated?: boolean;
}

export interface DecisionParams {
  /** Why; kept with the decision and sent in the webhook. At most 1000 characters. */
  note?: string;
}

/** What `approve` and `reject` answer. */
export interface Decision {
  object: "email";
  id: string;
  decision: Open<"approved" | "rejected">;
  /** Approved (now `queued`) or rejected. */
  recipients: string[];
  /** Approved, but suppressed since the email was held, so not sent. */
  suppressed: string[];
}

/** What `GET /emails/{id}/replies` answers. */
export interface EmailReplies {
  object: "list";
  email_id: string;
  thread_id: string;
  /** Received messages that answer the email, oldest first. */
  data: MessageSummary[];
}

/** What `POST /emails/{id}/cancel` answers. */
export interface CancelEmailResponse {
  id?: string;
  object?: string;
  [key: string]: unknown;
}

export type RecipientStatus = Open<
  | "queued"
  | "sending"
  | "deferred"
  | "delivered"
  | "bounced"
  | "expired"
  | "suppressed"
  | "unknown"
  | "canceled"
  | "held"
  | "rejected"
>;

export interface Recipient {
  email: string;
  status: RecipientStatus;
  attempts: number;
  next_attempt_at: string | null;
  last_code: number | null;
  last_response: string;
}

export interface Attempt {
  id: string;
  recipient: string;
  worker: string;
  source_ip: string;
  mx_host: string;
  outcome: string;
  smtp_code: number | null;
  enhanced_code: string;
  response: string;
  tls: string;
  started_at: string;
  finished_at: string | null;
}

export interface EmailEvent {
  type: string;
  recipient?: string;
  data?: unknown;
  created_at: string;
}

export interface Email {
  object: "email";
  id: string;
  from: string;
  to: string[];
  cc: string[] | null;
  bcc: string[] | null;
  subject: string;
  message_id: string;
  created_at: string;
  expires_at: string;
  scheduled_at?: string | null;
  last_event: string;
  tags: Record<string, string> | null;
  body_purged: boolean;
  recipients: Recipient[];
  attempts: Attempt[];
  events: EmailEvent[];
  [key: string]: unknown;
}

export interface EmailSummary {
  id: string;
  from: string;
  to: string[];
  subject: string;
  created_at: string;
  scheduled_at: string | null;
  last_event: string;
  api_key_id: string | null;
}

export interface Page<T> {
  object?: "list";
  data: T[];
  has_more: boolean;
  next_before: string | null;
}

export interface List<T> {
  object: "list";
  data: T[];
}

export interface Deleted {
  object: string;
  id: string;
  deleted: boolean;
}

// ---------------------------------------------------------------- domains

export interface DnsRecord {
  /** DKIM, SPF, MX, DMARC or INBOUND. */
  record: string;
  type: string;
  name: string;
  value: string;
  priority?: number;
  required: boolean;
  status: string;
  detail?: string;
}

export interface Domain {
  object: "domain";
  id: string;
  name: string;
  status: Open<"pending" | "verified" | "failed">;
  created_at: string;
  verified_at: string | null;
  checked_at: string | null;
  records: DnsRecord[];
  /** An unverified domain is removed at this time (7 days after it was added); `null` once verified. */
  claim_expires_at?: string | null;
  /** Where to send this domain's DMARC aggregate and TLS-RPT reports. */
  report_addresses?: {
    dmarc_rua?: string;
    tls_rpt_rua?: string;
    dmarc_record_example?: string;
    tls_rpt_record?: { name?: string; value?: string };
  };
}

export interface DkimKey {
  selector: string;
  algorithm: Open<"rsa" | "ed25519">;
  /** `active` keys sign; `pending` waits for its DNS record; `retiring` is the previous key. */
  state: Open<"active" | "pending" | "retiring">;
  /** New mail is signed with this key. */
  signs: boolean;
  record: DnsRecord;
  created_at?: string;
  retire_at?: string | null;
}

/** What the DKIM calls answer. */
export interface DkimState {
  object: Open<"dkim" | "dkim_rotation" | "dkim_ed25519">;
  /** Rotation: `pending` or `rotated`. Ed25519: `pending`, `active` or `disabled`. */
  status?: string;
  /** What happened and the next step. */
  message?: string;
  keys: DkimKey[];
  /** Authentication problems receivers reported for this sending domain. */
  auth_issues?: Record<string, unknown>[];
}

export interface DomainReportsParams {
  /** 1 to 180, default 30. */
  days?: number;
}

/** DMARC aggregate and TLS-RPT reports received for a domain. */
export interface DomainReports {
  object: "domain_reports";
  domain_id: string;
  domain: string;
  days: number;
  addresses?: Record<string, unknown>;
  /** `messages`, `pass`, `fail`, `pass_rate`, `by_organization`, `by_source`, `unknown_senders`. */
  dmarc: Record<string, unknown>;
  /** `successful_sessions`, `failed_sessions`, `by_organization`, `failures`. */
  tls: Record<string, unknown>;
  /** The 50 most recent reports. */
  reports: Record<string, unknown>[];
}

// ---------------------------------------------------------------- webhooks

export type WebhookEventType = Open<
  | "email.sent"
  | "email.delivered"
  | "email.delivery_delayed"
  | "email.bounced"
  | "email.failed"
  | "email.suppressed"
  | "email.delivery_unknown"
  | "email.canceled"
  | "email.complained"
  | "email.received"
  | "email.held"
  | "email.approved"
  | "email.rejected"
  | "sending.paused"
>;

export interface CreateWebhookParams {
  /** An https URL. */
  endpoint: string;
  /** Leave out (or pass []) to receive every type. */
  events?: WebhookEventType[];
  /**
   * Also sign every request with Ed25519 (`v1a`). Verify those with the
   * webhook's `public_key` (`whpk_…`), which holds no secret.
   */
  asymmetricSigning?: boolean;
}

export interface RotateSecretParams {
  /**
   * How long the previous secret (and Ed25519 key) keep signing next to the
   * new ones: 0 to 604800 seconds, default 86400. 0 stops them at once.
   */
  overlapSeconds?: number;
}

export interface WebhookDelivery {
  id: string;
  type: string;
  status: string;
  attempts: number;
  last_status: number | null;
  last_error: string;
  created_at: string;
  delivered_at: string | null;
}

export interface Webhook {
  object: "webhook";
  id: string;
  endpoint: string;
  events: string[] | null;
  created_at: string;
  /** `whsec_…`, returned once, by `create` and `rotateSecret`. */
  signing_secret?: string;
  /** Whether requests are also signed with Ed25519 (`v1a`). */
  asymmetric_signing?: boolean;
  /** `whpk_…`: the Ed25519 public key that verifies `v1a` signatures. */
  public_key?: string;
  /** During a rotation's overlap: the previous public key, which still signs. */
  previous_public_key?: string;
  /** During a rotation's overlap: until when the previous secret (and key) still sign. */
  previous_secret_expires_at?: string;
  secret_rotated_at?: string;
  /** Failed attempts in a row; at 5 the endpoint's circuit breaker opens. */
  consecutive_failures?: number;
  /** While the circuit breaker is open: no requests until then. */
  paused_until?: string;
  /** The 20 most recent deliveries, returned by `get`. */
  recent_deliveries?: WebhookDelivery[];
}

export type Trust = Open<"authenticated" | "unauthenticated" | "failed">;

/** `sending.paused` only: the pause and the rates that tripped it. */
export interface PauseDetail {
  id?: string;
  scope?: Open<"api_key" | "domain" | "project">;
  scope_id?: string;
  scope_name?: string;
  reason?: Open<"bounce_rate" | "complaint_rate">;
  rate?: number;
  threshold?: number;
  recipients?: number;
  bounces?: number;
  complaints?: number;
}

/** The JSON body of a webhook request. */
export interface WebhookEvent {
  type: WebhookEventType;
  created_at: string;
  data: {
    email_id?: string;
    created_at?: string;
    from?: string;
    to?: string[];
    subject?: string;
    tags?: Record<string, string>;
    bounce?: { type: string; subType: string; message: string };
    delivery?: {
      attempt_id?: string;
      mx_host?: string;
      smtp_code?: number;
      enhanced_code?: string;
      response?: string;
    };
    /** `email.complained` only. */
    complaint?: { feedback_type?: string; user_agent?: string };
    /**
     * `email.held`: `{ reasons }`. `email.approved`: `{ approved_by, note }`.
     * `email.rejected`: `{ rejected_by, note }`.
     */
    approval?: { reasons?: HoldReason[]; approved_by?: string; rejected_by?: string; note?: string };
    /** `sending.paused` only. */
    pause?: PauseDetail;
    // email.received: `email_id` is the received message (read it with
    // messages.get), `to` the inbox address and `header_to` its To header.
    inbox_id?: string;
    inbox?: string;
    thread_id?: string;
    in_reply_to_email_id?: string;
    message_id?: string;
    cc?: string[];
    header_to?: string[];
    trust?: Trust;
    authentication?: { spf?: string; dkim?: string; dmarc?: string };
    held?: boolean;
    suspicious_instructions?: boolean;
    [key: string]: unknown;
  };
}

// ---------------------------------------------------------------- API keys

export type Permission = Open<"full_access" | "sending_access">;

/** Holds the key's emails for approval when any rule matches. */
export interface RequireApproval {
  /** Every email. */
  always?: boolean;
  /** A recipient this project has never sent to. */
  first_contact?: boolean;
  has_attachments?: boolean;
  /** A web link in the text or HTML. */
  has_links?: boolean;
  /** More than this many recipients (to, cc and bcc together). */
  recipients_over?: number;
}

/** Sending rules enforced by the server. Every field is optional; absent means no limit. */
export interface ApiKeyPolicy {
  max_per_hour?: number;
  max_per_day?: number;
  /** Patterns such as `*@acme.com`. */
  allowed_recipients?: string[];
  allowed_from?: string[];
  require_approval?: RequireApproval;
  /** This key may approve and reject held emails of other keys. */
  can_approve?: boolean;
  /** Inbox address patterns a `sending_access` key may read and reply from. */
  inboxes?: string[];
}

export interface CreateApiKeyParams {
  name: string;
  /** Default `full_access`. */
  permission?: Permission;
  policy?: ApiKeyPolicy;
  /** Addresses or CIDR prefixes the key may be used from. Default: any. */
  allowedIps?: string[] | null;
  /** When the key stops working; must be in the future. Default: never. */
  expiresAt?: Date | string | null;
}

export interface UpdateApiKeyParams {
  name?: string;
  /** Replaces the whole policy; `{}` removes every limit. */
  policy?: ApiKeyPolicy;
  /** Replaces the allowed networks; `null` removes the restriction. */
  allowedIps?: string[] | null;
  /** Replaces the expiry; `null` makes the key never expire. */
  expiresAt?: Date | string | null;
}

export interface ApiKey {
  id: string;
  name: string;
  prefix: string;
  permission: Permission;
  policy: ApiKeyPolicy;
  created_at: string;
  last_used_at: string | null;
  /** Networks the key may be used from; `null` allows every address. */
  allowed_ips?: string[] | null;
  /** When the key stops working (`401 api_key_expired`); `null`: never. */
  expires_at?: string | null;
  /** The secret `omw_…` token, returned once, by `create`. */
  token?: string;
}

// ---------------------------------------------------------------- inboxes

export interface Inbox {
  object?: "inbox";
  id: string;
  address: string;
  created_at: string;
  domain_id?: string;
  /** Mail whose sender is not authenticated is held from `sending_access` keys. Default `true`. */
  hold_unauthenticated?: boolean;
  [key: string]: unknown;
}

export interface CreateInboxOptions {
  /** Hold mail whose sender is not authenticated. Default `true`. */
  holdUnauthenticated?: boolean;
}

export interface UpdateInboxParams {
  holdUnauthenticated: boolean;
}

export interface ListMessagesParams {
  limit?: number;
  before?: string;
  threadId?: string;
}

export interface MessageSummary {
  id: string;
  from: string;
  to: string[] | string;
  subject: string;
  received_at: string;
  thread_id: string | null;
  in_reply_to_email_id: string | null;
  snippet: string;
  /** The sender verdict from SPF, DKIM and DMARC. */
  trust?: Trust;
  /** Held messages are visible only to full-access keys until released. */
  held?: boolean;
  /** The text matches one of the prompt-injection rules. */
  suspicious_instructions?: boolean;
}

export interface MessageAttachment {
  index: number;
  filename: string;
  content_type: string;
  size: number;
}

export interface Message {
  id: string;
  inbox_id: string;
  from: string;
  to: string[] | string;
  cc: string[] | string | null;
  subject: string;
  text: string | null;
  html: string | null;
  headers: {
    "message-id"?: string;
    "in-reply-to"?: string;
    references?: string;
    date?: string;
    [key: string]: string | undefined;
  };
  attachments: MessageAttachment[];
  thread_id: string | null;
  in_reply_to_email_id: string | null;
  received_at: string;
  /** The sender verdict from SPF, DKIM and DMARC. */
  trust?: Trust;
  /** SPF, each DKIM signature, DMARC, the verdict's reason, and an Authentication-Results `header`. */
  authentication_results?: Record<string, unknown> | null;
  /** Held messages are visible only to full-access keys until released. */
  held?: boolean;
  released_at?: string | null;
  /** The text matches one of the prompt-injection rules (`suspicious_rules`). */
  suspicious_instructions?: boolean;
  suspicious_rules?: string[];
  reply_to?: string[];
  /** The envelope sender (MAIL FROM). */
  mail_from?: string;
  /** A Reply-To address is on another organizational domain than From. */
  reply_to_mismatch?: boolean;
  /** Another message in the inbox has the same Message-ID but different content. */
  message_id_conflict?: boolean;
  size?: number;
  tls?: boolean;
  body_purged?: boolean;
}

export interface ReplyParams {
  text?: string;
  html?: string;
  /** Recipients. Default: only the first Reply-To address, or From. */
  to?: string[];
}

/** What `messages.reply` answers. */
export interface ReplyResponse {
  /** The sent email; follow it with `emails.get`. */
  id: string;
  thread_id: string;
  /** Who the reply was sent to. */
  to?: string[];
  /** Present when other Reply-To addresses were left out, or Reply-To is on another domain than From. */
  warnings?: string[];
  held?: true;
  hold_reasons?: HoldReason[];
}

// ---------------------------------------------------------------- suppressions

export type SuppressionStream = Open<"all" | "broadcast">;

export interface Suppression {
  address: string;
  /** `all`: no mail at all. `broadcast`: only broadcast mail (one-click unsubscribes). */
  stream: SuppressionStream;
  /** `hard_bounce`, `complaint`, `unsubscribe`, `manual`, or a name given to `create`. */
  reason: string;
  detail: string;
  created_at: string;
}

export interface SuppressionFilter {
  /** Only this reason, e.g. `hard_bounce`. */
  reason?: string;
  stream?: SuppressionStream;
  /** Only this address. */
  address?: string;
  /** Only addresses at this domain. */
  domain?: string;
}

export interface ListSuppressionsParams extends SuppressionFilter {
  limit?: number;
  /** `next_before` of the previous page (an opaque cursor). */
  before?: string;
}

export interface CreateSuppressionParams {
  address: string;
  /** A short name such as `customer_request`. Default `manual`. */
  reason?: string;
  /** Free text kept with it, at most 300 characters. */
  detail?: string;
  /** Default `all`. */
  stream?: SuppressionStream;
}

export interface SuppressionCreated extends Partial<Suppression> {
  object: "suppression";
  address: string;
  stream: SuppressionStream;
  reason: string;
  /** `false` when the address was already suppressed for that stream. */
  created: boolean;
}

export interface SuppressionDeleted {
  object: "suppression";
  address: string;
  deleted: true;
  removed: Suppression[];
}

// ---------------------------------------------------------------- pauses

export interface Pause extends PauseDetail {
  object: "pause";
  id: string;
  scope: Open<"api_key" | "domain" | "project">;
  scope_id: string;
  reason: Open<"bounce_rate" | "complaint_rate">;
  /** The rate when it tripped, as a fraction (0.07 is 7%), over 24 hours. */
  rate: number;
  threshold: number;
  created_at: string;
  resumed_at?: string | null;
  /** The key that resumed it. */
  resumed_by?: string | null;
  /** Still in force: sends from its scope are refused with `403 sending_paused`. */
  active: boolean;
}

export interface ListPausesParams {
  /** `true`: only pauses still in force. */
  active?: boolean;
  /** 1 to 100, default 50. */
  limit?: number;
}

// ---------------------------------------------------------------- project

export interface CircuitBreaker {
  enabled?: boolean;
  /** Pause when bounces over 24 hours are above this fraction of recipients. Default 0.05. */
  bounce_rate?: number;
  /** Pause when complaints are above this fraction. Default 0.001. */
  complaint_rate?: number;
  /** Rates count only once a scope queued this many recipients in 24 hours. Default 50. */
  min_recipients?: number;
}

export interface Project {
  object: "project";
  id: string;
  name: string;
  /** At most this many emails per recipient in any 24 hours, across every key; `null` is no cap. */
  max_per_recipient_per_day: number | null;
  circuit_breaker: CircuitBreaker;
  updated_at?: string | null;
}

export interface UpdateProjectParams {
  /** 1 to 10000; `null` turns the cap off. */
  maxPerRecipientPerDay?: number | null;
  /** Fields left out keep their value. Uses the API's field names. */
  circuitBreaker?: CircuitBreaker;
}

// ---------------------------------------------------------------- audit log

/** One entry of the project's hash-chained audit log. */
export interface AuditEntry {
  object: "audit_entry";
  id: number;
  project_id: string | null;
  created_at: string;
  actor: Open<"api_key" | "cli" | "system">;
  actor_key_id: string | null;
  /** `<thing>.<verb>`, e.g. `api_key.created`. */
  action: string;
  target_type: string;
  target_id: string;
  data: Record<string, unknown>;
  /** The exact JSON text that was hashed. */
  entry: string;
  prev_hash: string;
  hash: string;
}

export interface ListAuditLogParams {
  /** 1 to 200, default 50. */
  limit?: number;
  /** An entry id: only older entries (`next_before` of the previous page). */
  before?: string | number;
  /** Only this action, e.g. `api_key.created`. */
  action?: string;
}

export interface AuditVerification {
  object: "audit_verification";
  valid: boolean;
  entries: number;
  /** The last entry's hash. Keep it to detect entries removed from the end later. */
  head_hash: string;
  first_invalid_id: number | null;
  problem?: string;
}

// ---------------------------------------------------------------- insights

export type InsightWindowName = Open<"24h" | "7d" | "30d">;

export interface InsightStats {
  attempts: number;
  accepted: number;
  deferred: number;
  bounced: number;
  unknown: number;
  acceptance_rate?: number | null;
  deferral_rate?: number | null;
  bounce_rate?: number | null;
  time_to_acceptance_p50_seconds?: number | null;
  time_to_acceptance_p95_seconds?: number | null;
}

export interface InsightGroup extends InsightStats {
  /** `google`, `microsoft`, `yahoo`, … or `other`, `unknown`. */
  provider?: string;
  sending_ip?: string;
}

export interface InsightWindow {
  window: InsightWindowName;
  total: InsightStats;
  by_provider: InsightGroup[];
  by_sending_ip: InsightGroup[];
}

export interface Insights {
  object: "insights";
  generated_at: string;
  window_basis?: "sent_at";
  provider_grouping?: "mx_host_suffix";
  emails_considered: number;
  /** More than 100,000 emails in 30 days: the longer windows cover only the newest 100,000. */
  truncated: boolean;
  windows: InsightWindow[];
}

export interface GetInsightsParams {
  /**
   * Keep only this window in `windows`. The server always computes `24h`,
   * `7d` and `30d`; the SDK filters the answer.
   */
  window?: InsightWindowName;
}
