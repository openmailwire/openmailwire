import { bytesToBase64, toBytes } from "./encoding.js";
import { OpenMailWireError, WaitTimeoutError } from "./errors.js";
import type {
  ApiKey,
  AuditEntry,
  AuditVerification,
  BatchEmailParams,
  BatchResponse,
  CancelEmailResponse,
  CheckResult,
  CreateApiKeyParams,
  CreateInboxOptions,
  CreateSuppressionParams,
  CreateWebhookParams,
  Decision,
  DecisionParams,
  Deleted,
  DkimState,
  Domain,
  DomainReports,
  DomainReportsParams,
  Email,
  EmailReplies,
  EmailSummary,
  GetInsightsParams,
  HeldEmail,
  Inbox,
  Insights,
  List,
  ListAuditLogParams,
  ListEmailsParams,
  ListHeldParams,
  ListMessagesParams,
  ListPausesParams,
  ListSuppressionsParams,
  Message,
  MessageSummary,
  Page,
  Pause,
  Project,
  ReplyParams,
  ReplyResponse,
  RotateSecretParams,
  SendEmailParams,
  SendEmailResponse,
  SendOptions,
  SendRawParams,
  Suppression,
  SuppressionCreated,
  SuppressionDeleted,
  SuppressionFilter,
  SuppressionStream,
  UpdateApiKeyParams,
  UpdateInboxParams,
  UpdateProjectParams,
  Webhook,
} from "./types.js";

export const VERSION = "0.1.0";
export const DEFAULT_BASE_URL = "http://localhost:8080";

export interface ClientOptions {
  /** An `omw_…` key. Default: the `OMW_API_KEY` environment variable. */
  apiKey?: string;
  /** Default: the `OMW_API_URL` environment variable, then `http://localhost:8080`. */
  baseUrl?: string;
  /** Per-request timeout in milliseconds. Default 30000. */
  timeoutMs?: number;
  /** A `fetch` implementation. Default: the global `fetch`. */
  fetch?: typeof fetch;
  /**
   * How many times to send a request again when it is answered 429 or 503,
   * after waiting the `Retry-After` seconds (1 s if there is none). Default
   * 0: never retry. The same body and idempotency key are sent again.
   */
  maxRetries?: number;
  /**
   * The longest `Retry-After` to wait for, in milliseconds. A longer one is
   * thrown at once. Default 60000.
   */
  maxRetryWaitMs?: number;
}

interface RequestOptions {
  body?: unknown;
  query?: Record<string, string | number | boolean | undefined>;
  idempotencyKey?: string;
  binary?: boolean;
  /** Return the body as text (e.g. CSV) instead of parsing JSON. */
  text?: boolean;
}

type Requester = <T>(method: string, path: string, options?: RequestOptions) => Promise<T>;

const PENDING = new Set(["queued", "sending", "deferred"]);

function env(name: string): string | undefined {
  return typeof process !== "undefined" ? process.env?.[name] : undefined;
}

function seg(id: string | number): string {
  return encodeURIComponent(String(id));
}

function time<T extends Date | string | null | undefined>(v: T): string | Exclude<T, Date> {
  return (v instanceof Date ? v.toISOString() : v) as string | Exclude<T, Date>;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function parseRetryAfter(v: string | null): number | null {
  if (!v) return null;
  if (/^\d+$/.test(v.trim())) return Number(v.trim());
  const at = Date.parse(v);
  return Number.isNaN(at) ? null : Math.max(0, Math.ceil((at - Date.now()) / 1000));
}

async function toError(res: Response): Promise<OpenMailWireError> {
  const retryAfter = parseRetryAfter(res.headers.get("retry-after"));
  const text = await res.text().catch(() => "");
  try {
    const j = JSON.parse(text) as { name?: unknown; message?: unknown };
    if (j && typeof j.name === "string") {
      return new OpenMailWireError(res.status, j.name, typeof j.message === "string" ? j.message : "", retryAfter);
    }
  } catch {
    // not JSON: fall through
  }
  return new OpenMailWireError(res.status, "http_error", text.slice(0, 500) || `HTTP ${res.status}`, retryAfter);
}

function sendBody(p: SendEmailParams): Record<string, unknown> {
  return {
    from: p.from,
    to: p.to,
    cc: p.cc,
    bcc: p.bcc,
    reply_to: p.replyTo,
    subject: p.subject,
    html: p.html,
    text: p.text,
    headers: p.headers,
    tags: p.tags,
    attachments: p.attachments?.map((a) => ({
      filename: a.filename,
      content: typeof a.content === "string" ? a.content : bytesToBase64(a.content),
      content_type: a.contentType,
      content_id: a.contentId,
    })),
    scheduled_at: time(p.scheduledAt),
    expires_at: time(p.expiresAt),
    retry_uncertain: p.retryUncertain,
    stream: p.stream,
  };
}

function filterQuery(f: SuppressionFilter): Record<string, string | undefined> {
  return { reason: f.reason, stream: f.stream, address: f.address, domain: f.domain };
}

export class Emails {
  constructor(private readonly request: Requester) {}

  /** Queues an email and returns its id. */
  send(params: SendEmailParams, options: SendOptions = {}): Promise<SendEmailResponse> {
    return this.request("POST", "/emails", { body: sendBody(params), idempotencyKey: options.idempotencyKey });
  }

  /** One email with each recipient's status, every attempt and the event timeline. */
  get(id: string): Promise<Email> {
    return this.request("GET", `/emails/${seg(id)}`);
  }

  /** Emails, newest first. Pass `next_before` as `before` for the next page. */
  list(params: ListEmailsParams = {}): Promise<Page<EmailSummary>> {
    return this.request("GET", "/emails", {
      query: { limit: params.limit, before: params.before, api_key_id: params.apiKeyId },
    });
  }

  /** Cancels every recipient not tried yet. 409 `cannot_cancel` if there is none. */
  cancel(id: string): Promise<CancelEmailResponse> {
    return this.request("POST", `/emails/${seg(id)}/cancel`);
  }

  /** Queues 1 to 100 emails (no attachments) in one transaction. */
  batch(emails: BatchEmailParams[], options: SendOptions = {}): Promise<BatchResponse> {
    return this.request("POST", "/emails/batch", {
      body: emails.map((e) => sendBody(e)),
      idempotencyKey: options.idempotencyKey,
    });
  }

  /** Queues a complete MIME message. Its From domain must be verified. */
  sendRaw(params: SendRawParams, options: SendOptions = {}): Promise<SendEmailResponse> {
    return this.request("POST", "/emails/raw", {
      body: { raw: bytesToBase64(toBytes(params.raw)), mail_from: params.mailFrom, rcpts: params.rcpts },
      idempotencyKey: options.idempotencyKey,
    });
  }

  /**
   * Reports what `send` would do with these params now (send, hold or
   * refuse, with every refusal and warning), without sending anything.
   */
  check(params: SendEmailParams): Promise<CheckResult> {
    return this.request("POST", "/emails/check", { body: sendBody(params) });
  }

  /** Emails waiting for approval, newest first. */
  listHeld(params: ListHeldParams = {}): Promise<Page<HeldEmail>> {
    return this.request("GET", "/emails/held", { query: { limit: params.limit, before: params.before } });
  }

  /**
   * Approves a held email: its recipients are queued. Needs a `full_access`
   * key or one with `can_approve`; no key approves its own email.
   */
  approve(id: string, params: DecisionParams = {}): Promise<Decision> {
    return this.request("POST", `/emails/${seg(id)}/approve`, { body: { note: params.note } });
  }

  /** Rejects a held email: it is never sent. */
  reject(id: string, params: DecisionParams = {}): Promise<Decision> {
    return this.request("POST", `/emails/${seg(id)}/reject`, { body: { note: params.note } });
  }

  /** The received messages that answer a sent email, oldest first. */
  replies(id: string): Promise<EmailReplies> {
    return this.request("GET", `/emails/${seg(id)}/replies`);
  }
}

export class Domains {
  constructor(private readonly request: Requester) {}

  /** Adds a domain and returns the DNS records to publish. */
  create(name: string): Promise<Domain> {
    return this.request("POST", "/domains", { body: { name } });
  }

  list(): Promise<List<Domain>> {
    return this.request("GET", "/domains");
  }

  get(id: string): Promise<Domain> {
    return this.request("GET", `/domains/${seg(id)}`);
  }

  /** Checks the DNS records now. */
  verify(id: string): Promise<Domain> {
    return this.request("POST", `/domains/${seg(id)}/verify`);
  }

  delete(id: string): Promise<Deleted> {
    return this.request("DELETE", `/domains/${seg(id)}`);
  }

  /** The domain's DKIM keys, with the records to publish. */
  dkim(id: string): Promise<DkimState> {
    return this.request("GET", `/domains/${seg(id)}/dkim`);
  }

  /**
   * Rotates the DKIM key in two steps: the first call creates a key to
   * publish (`status: "pending"`), a later call switches signing to it once
   * its record is in DNS (`status: "rotated"`). 409 `dkim_record_not_found`
   * while the record is missing.
   */
  rotateDkim(id: string): Promise<DkimState> {
    return this.request("POST", `/domains/${seg(id)}/dkim/rotate`);
  }

  /**
   * Turns the extra Ed25519 DKIM signature on or off. Turning it on creates
   * a key to publish first; call again once its record is in DNS.
   */
  setEd25519(id: string, enabled: boolean): Promise<DkimState> {
    return this.request("POST", `/domains/${seg(id)}/dkim/ed25519`, { body: { enabled } });
  }

  /** DMARC aggregate and TLS-RPT reports received for the domain. */
  reports(id: string, params: DomainReportsParams = {}): Promise<DomainReports> {
    return this.request("GET", `/domains/${seg(id)}/reports`, { query: { days: params.days } });
  }
}

export class Webhooks {
  constructor(private readonly request: Requester) {}

  /**
   * Creates a webhook. The answer carries `signing_secret`, shown once, and,
   * with `asymmetricSigning`, the Ed25519 `public_key`.
   */
  create(params: CreateWebhookParams): Promise<Webhook> {
    return this.request("POST", "/webhooks", {
      body: { endpoint: params.endpoint, events: params.events, asymmetric_signing: params.asymmetricSigning },
    });
  }

  /**
   * Replaces the signing secret (and Ed25519 key). The previous ones keep
   * signing for `overlapSeconds` (default one day). The answer carries the
   * new `signing_secret`, shown once.
   */
  rotateSecret(id: string, params: RotateSecretParams = {}): Promise<Webhook> {
    return this.request("POST", `/webhooks/${seg(id)}/rotate-secret`, {
      body: { overlap_seconds: params.overlapSeconds },
    });
  }

  list(): Promise<List<Webhook>> {
    return this.request("GET", "/webhooks");
  }

  /** One webhook with its 20 most recent deliveries. */
  get(id: string): Promise<Webhook> {
    return this.request("GET", `/webhooks/${seg(id)}`);
  }

  delete(id: string): Promise<Deleted> {
    return this.request("DELETE", `/webhooks/${seg(id)}`);
  }
}

export class ApiKeys {
  constructor(private readonly request: Requester) {}

  /** Creates a key. The answer carries `token`, shown once. Needs full access. */
  create(params: CreateApiKeyParams): Promise<ApiKey> {
    return this.request("POST", "/api-keys", {
      body: {
        name: params.name,
        permission: params.permission,
        policy: params.policy,
        allowed_ips: params.allowedIps,
        expires_at: time(params.expiresAt),
      },
    });
  }

  list(): Promise<List<ApiKey>> {
    return this.request("GET", "/api-keys");
  }

  get(id: string): Promise<ApiKey> {
    return this.request("GET", `/api-keys/${seg(id)}`);
  }

  /**
   * Renames a key, or replaces its policy, allowed networks or expiry. Fields
   * left out are unchanged; `null` clears `allowedIps` or `expiresAt`.
   */
  update(id: string, params: UpdateApiKeyParams): Promise<ApiKey> {
    return this.request("PATCH", `/api-keys/${seg(id)}`, {
      body: {
        name: params.name,
        policy: params.policy,
        allowed_ips: params.allowedIps,
        expires_at: time(params.expiresAt),
      },
    });
  }

  /** Revokes a key. */
  delete(id: string): Promise<Deleted> {
    return this.request("DELETE", `/api-keys/${seg(id)}`);
  }
}

export class Inboxes {
  constructor(private readonly request: Requester) {}

  /** Creates an inbox for an address on one of the project's domains. */
  create(address: string, options: CreateInboxOptions = {}): Promise<Inbox> {
    return this.request("POST", "/inboxes", {
      body: { address, hold_unauthenticated: options.holdUnauthenticated },
    });
  }

  list(): Promise<List<Inbox>> {
    return this.request("GET", "/inboxes");
  }

  get(id: string): Promise<Inbox> {
    return this.request("GET", `/inboxes/${seg(id)}`);
  }

  /** Changes whether mail from unauthenticated senders is held. Needs full access. */
  update(id: string, params: UpdateInboxParams): Promise<Inbox> {
    return this.request("PATCH", `/inboxes/${seg(id)}`, {
      body: { hold_unauthenticated: params.holdUnauthenticated },
    });
  }

  delete(id: string): Promise<Deleted> {
    return this.request("DELETE", `/inboxes/${seg(id)}`);
  }

  /** Messages received by an inbox, newest first. */
  listMessages(inboxId: string, params: ListMessagesParams = {}): Promise<Page<MessageSummary>> {
    return this.request("GET", `/inboxes/${seg(inboxId)}/messages`, {
      query: { limit: params.limit, before: params.before, thread_id: params.threadId },
    });
  }
}

export class Messages {
  constructor(private readonly request: Requester) {}

  /** One received message, with its text, HTML, headers and attachment list. */
  get(id: string): Promise<Message> {
    return this.request("GET", `/messages/${seg(id)}`);
  }

  /** The raw bytes of one attachment (`index` from `attachments[].index`). */
  getAttachment(id: string, index: number): Promise<Uint8Array> {
    return this.request("GET", `/messages/${seg(id)}/attachments/${seg(index)}`, { binary: true });
  }

  /** Replies from the inbox address, threaded with `In-Reply-To` and `References`. */
  reply(id: string, params: ReplyParams): Promise<ReplyResponse> {
    return this.request("POST", `/messages/${seg(id)}/reply`, {
      body: { text: params.text, html: params.html, to: params.to },
    });
  }

  /**
   * Releases a held message, so keys with access to its inbox see it.
   * Needs full access. Answers the message without its content.
   */
  release(id: string): Promise<Message> {
    return this.request("POST", `/messages/${seg(id)}/release`);
  }
}

export class Suppressions {
  constructor(private readonly request: Requester) {}

  /** Suppressed addresses, newest first. Pass `next_before` as `before` for the next page. */
  list(params: ListSuppressionsParams = {}): Promise<Page<Suppression>> {
    return this.request("GET", "/suppressions", {
      query: { limit: params.limit, before: params.before, ...filterQuery(params) },
    });
  }

  /** Stops sending to an address. `created` is false if it was already suppressed. Needs full access. */
  create(params: CreateSuppressionParams): Promise<SuppressionCreated> {
    return this.request("POST", "/suppressions", {
      body: { address: params.address, reason: params.reason, detail: params.detail, stream: params.stream },
    });
  }

  /** Sends to an address again: removes its suppressions, or only `stream`'s. Needs full access. */
  delete(address: string, params: { stream?: SuppressionStream } = {}): Promise<SuppressionDeleted> {
    return this.request("DELETE", `/suppressions/${seg(address)}`, { query: { stream: params.stream } });
  }

  /** Every matching suppression as CSV text. */
  export(params: SuppressionFilter = {}): Promise<string> {
    return this.request("GET", "/suppressions/export", { query: filterQuery(params), text: true });
  }
}

export class Pauses {
  constructor(private readonly request: Requester) {}

  /** Circuit-breaker pauses, newest first. */
  list(params: ListPausesParams = {}): Promise<List<Pause>> {
    return this.request("GET", "/pauses", { query: { active: params.active, limit: params.limit } });
  }

  /** Lets the paused key, domain or project send again. Needs full access. */
  resume(id: string): Promise<Pause> {
    return this.request("POST", `/pauses/${seg(id)}/resume`);
  }
}

export class ProjectSettings {
  constructor(private readonly request: Requester) {}

  /** The project's per-recipient cap and circuit-breaker settings. */
  get(): Promise<Project> {
    return this.request("GET", "/project");
  }

  /** Changes them. Fields left out keep their value. Needs full access. */
  update(params: UpdateProjectParams): Promise<Project> {
    return this.request("PATCH", "/project", {
      body: { max_per_recipient_per_day: params.maxPerRecipientPerDay, circuit_breaker: params.circuitBreaker },
    });
  }
}

export class AuditLog {
  constructor(private readonly request: Requester) {}

  /** Entries, newest first. Needs full access. */
  list(params: ListAuditLogParams = {}): Promise<Page<AuditEntry>> {
    return this.request("GET", "/audit-log", {
      query: { limit: params.limit, before: params.before, action: params.action },
    });
  }

  /** Checks the whole hash chain on the server. Keep `head_hash` to compare later. */
  verify(): Promise<AuditVerification> {
    return this.request("GET", "/audit-log/verify");
  }
}

export class DeliveryInsights {
  constructor(private readonly request: Requester) {}

  /**
   * Acceptance, deferral and bounce rates and time to acceptance, per
   * mailbox provider and sending IP, for the last 24 hours, 7 days and 30
   * days. With `window`, only that window is kept in `windows`. Needs full
   * access.
   */
  async get(params: GetInsightsParams = {}): Promise<Insights> {
    const insights = await this.request<Insights>("GET", "/insights");
    if (params.window && insights && Array.isArray(insights.windows)) {
      insights.windows = insights.windows.filter((w) => w.window === params.window);
    }
    return insights;
  }
}

export class OpenMailWire {
  readonly baseUrl: string;
  readonly emails: Emails;
  readonly domains: Domains;
  readonly webhooks: Webhooks;
  readonly apiKeys: ApiKeys;
  readonly inboxes: Inboxes;
  readonly messages: Messages;
  readonly suppressions: Suppressions;
  readonly pauses: Pauses;
  readonly project: ProjectSettings;
  readonly auditLog: AuditLog;
  readonly insights: DeliveryInsights;

  readonly #apiKey: string;
  readonly #timeoutMs: number;
  readonly #fetch: typeof fetch;
  readonly #maxRetries: number;
  readonly #maxRetryWaitMs: number;

  constructor(options: ClientOptions = {}) {
    const apiKey = options.apiKey ?? env("OMW_API_KEY");
    if (!apiKey) {
      throw new Error("OpenMailWire: no API key. Pass { apiKey: 'omw_…' } or set OMW_API_KEY.");
    }
    this.#apiKey = apiKey;
    this.baseUrl = (options.baseUrl || env("OMW_API_URL") || DEFAULT_BASE_URL).replace(/\/+$/, "");
    this.#timeoutMs = options.timeoutMs ?? 30_000;
    this.#maxRetries = Math.max(0, Math.floor(options.maxRetries ?? 0));
    this.#maxRetryWaitMs = options.maxRetryWaitMs ?? 60_000;
    const f = options.fetch ?? globalThis.fetch;
    if (typeof f !== "function") throw new Error("OpenMailWire: no fetch available; use Node 18+ or pass { fetch }.");
    this.#fetch = (input, init) => f(input, init);

    const request: Requester = (method, path, opts) => this.#request(method, path, opts);
    this.emails = new Emails(request);
    this.domains = new Domains(request);
    this.webhooks = new Webhooks(request);
    this.apiKeys = new ApiKeys(request);
    this.inboxes = new Inboxes(request);
    this.messages = new Messages(request);
    this.suppressions = new Suppressions(request);
    this.pauses = new Pauses(request);
    this.project = new ProjectSettings(request);
    this.auditLog = new AuditLog(request);
    this.insights = new DeliveryInsights(request);
  }

  /** Sends a request, and again after `Retry-After` on 429 and 503 when `maxRetries` allows. */
  async #request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
    for (let attempt = 0; ; attempt++) {
      try {
        return await this.#send<T>(method, path, opts);
      } catch (err) {
        if (!(err instanceof OpenMailWireError) || !err.retryable || attempt >= this.#maxRetries) throw err;
        const waitMs = (err.retryAfter ?? 1) * 1000;
        if (waitMs > this.#maxRetryWaitMs) throw err;
        await sleep(waitMs);
      }
    }
  }

  async #send<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
    const url = new URL(this.baseUrl + path);
    for (const [k, v] of Object.entries(opts.query ?? {})) {
      if (v !== undefined && v !== "") url.searchParams.set(k, String(v));
    }
    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.#apiKey}`,
      Accept: opts.binary ? "*/*" : opts.text ? "text/csv, text/plain, */*" : "application/json",
      "User-Agent": `openmailwire-node/${VERSION}`,
    };
    let body: string | undefined;
    if (opts.body !== undefined) {
      headers["Content-Type"] = "application/json";
      body = JSON.stringify(opts.body);
    }
    if (opts.idempotencyKey) headers["Idempotency-Key"] = opts.idempotencyKey;

    const failed = (err: unknown): OpenMailWireError => {
      const e = err as Error & { cause?: { message?: string } };
      const why =
        e?.name === "TimeoutError"
          ? `no answer within ${this.#timeoutMs} ms`
          : `${e?.message ?? String(err)}${e?.cause?.message ? `: ${e.cause.message}` : ""}`;
      return new OpenMailWireError(0, "connection_error", `${method} ${this.baseUrl}${path}: ${why}`);
    };

    let res: Response;
    let text: string;
    try {
      // Redirects are not followed, so the API key is never sent to another address.
      res = await this.#fetch(url, {
        method,
        headers,
        body,
        redirect: "manual",
        signal: AbortSignal.timeout(this.#timeoutMs),
      });
      if (res.type === "opaqueredirect" || (res.status >= 300 && res.status < 400)) {
        const location = res.headers.get("location");
        throw new OpenMailWireError(
          res.status,
          "redirect_refused",
          `${method} ${this.baseUrl}${path} answered ${res.status}, a redirect to ${location ?? "another address"}. ` +
            "Redirects are not followed, so the API key never goes elsewhere: set baseUrl to the address the API answers at.",
        );
      }
      if (!res.ok) throw await toError(res);
      if (opts.binary) return new Uint8Array(await res.arrayBuffer()) as T;
      text = await res.text();
    } catch (err) {
      throw err instanceof OpenMailWireError ? err : failed(err);
    }
    if (opts.text) return text as T;
    if (!text) return null as T;
    try {
      return JSON.parse(text) as T;
    } catch {
      throw new OpenMailWireError(
        res.status,
        "invalid_response",
        `${method} ${this.baseUrl}${path} answered ${res.status} with a body that is not JSON: ${text.slice(0, 200)}`,
      );
    }
  }

  /**
   * Polls `emails.get` until no recipient is queued, sending or deferred, and
   * returns that email. Every recipient then has a final status (delivered,
   * bounced, expired, suppressed, unknown or canceled): check each one.
   *
   * @throws WaitTimeoutError after `timeoutMs`, carrying the last state seen.
   */
  async waitForDelivery(emailId: string, timeoutMs = 60_000, { intervalMs = 1_000 } = {}): Promise<Email> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      const email = await this.emails.get(emailId);
      const pending = (email.recipients ?? []).filter((r) => PENDING.has(r.status));
      if (pending.length === 0) return email;
      const left = deadline - Date.now();
      if (left <= 0) {
        throw new WaitTimeoutError(
          `email ${emailId}: ${pending.length} recipient(s) still ${pending.map((r) => r.status).join(", ")} after ${timeoutMs} ms`,
          email,
        );
      }
      await sleep(Math.min(intervalMs, left));
    }
  }

  /**
   * Polls the inboxes until a received message answers the sent email
   * `emailId` (its `in_reply_to_email_id`), and returns those messages, newest
   * first. Returns an empty array if none arrived within `timeoutMs`.
   * Looks at the 50 newest messages of each inbox, or only of `inboxId`.
   */
  async waitForReply(
    emailId: string,
    timeoutMs = 60_000,
    { inboxId, intervalMs = 2_000 }: { inboxId?: string; intervalMs?: number } = {},
  ): Promise<MessageSummary[]> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      const ids = inboxId ? [inboxId] : (await this.inboxes.list()).data.map((i) => i.id);
      const found: MessageSummary[] = [];
      for (const id of ids) {
        const page = await this.inboxes.listMessages(id, { limit: 50 });
        found.push(...page.data.filter((m) => m.in_reply_to_email_id === emailId));
      }
      if (found.length > 0) {
        return found.sort((a, b) => (a.received_at < b.received_at ? 1 : a.received_at > b.received_at ? -1 : 0));
      }
      const left = deadline - Date.now();
      if (left <= 0) return [];
      await sleep(Math.min(intervalMs, left));
    }
  }
}
