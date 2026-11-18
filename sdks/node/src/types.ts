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
}

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
}

export interface BatchResponse {
  data: { id: string }[];
}

/** What `POST /emails/{id}/cancel` answers. */
export interface CancelEmailResponse {
  id?: string;
  object?: string;
  [key: string]: unknown;
}

export type RecipientStatus = Open<
  "queued" | "sending" | "deferred" | "delivered" | "bounced" | "expired" | "suppressed" | "unknown" | "canceled"
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
>;

export interface CreateWebhookParams {
  /** An https URL. */
  endpoint: string;
  /** Leave out (or pass []) to receive every type. */
  events?: WebhookEventType[];
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
  /** `whsec_…`, returned once, by `create`. */
  signing_secret?: string;
  /** The 20 most recent deliveries, returned by `get`. */
  recent_deliveries?: WebhookDelivery[];
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
    [key: string]: unknown;
  };
}

// ---------------------------------------------------------------- API keys

export type Permission = Open<"full_access" | "sending_access">;

/** Sending rules enforced by the server. Every field is optional; absent means no limit. */
export interface ApiKeyPolicy {
  max_per_hour?: number;
  max_per_day?: number;
  /** Patterns such as `*@acme.com`. */
  allowed_recipients?: string[];
  allowed_from?: string[];
}

export interface CreateApiKeyParams {
  name: string;
  /** Default `full_access`. */
  permission?: Permission;
  policy?: ApiKeyPolicy;
}

export interface UpdateApiKeyParams {
  name?: string;
  policy?: ApiKeyPolicy;
}

export interface ApiKey {
  id: string;
  name: string;
  prefix: string;
  permission: Permission;
  policy: ApiKeyPolicy;
  created_at: string;
  last_used_at: string | null;
  /** The secret `omw_…` token, returned once, by `create`. */
  token?: string;
}

// ---------------------------------------------------------------- inboxes

export interface Inbox {
  id: string;
  address: string;
  created_at: string;
  [key: string]: unknown;
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
}

export interface ReplyParams {
  text?: string;
  html?: string;
}
