# Client error contract — Anthropic surface (OxSci)

Every error sub2api returns to a client on `POST /v1/messages` and
`POST /v1/messages/count_tokens` (groups on the `anthropic` platform: Anthropic
OAuth / setup-token / API-key accounts and Bedrock accounts), as the client sees
it on the wire. It is written for callers that classify these errors (e.g. the
oma-core runtime adapter calling token.xsci.ai through the Claude Agent SDK /
Claude Code CLI) and states sub2api facts only: what happened, whether the same
request can succeed, whether waiting helps. What a caller does about each row is
the caller's decision.

The table is locked by `backend/internal/handler/xsci_client_error_contract_test.go`:
every row is driven through the real code path and checked against what the
client receives, and the test fails if this document's §2 table and the test's
row table differ in any cell. See §7 before changing either.

Build: `oxsci-custom-v3` from 6888e28f9 (upstream v0.2.23 + OxSci commits).

## 1. How an error looks on the wire

### 1.1 Bodies (`Body` column)

| Body | Shape | Rows |
|---|---|---|
| `anthropic` | `{"type":"error","error":{"type":<error.type>,"message":<message>}}`; the `E-LIMIT-*` rows also carry `"code"` inside `error` | most rows |
| `flat` | `{"code":<code>,"message":<message>}` — no `type`, no `error` object | API-key middleware (`E-AUTH-*`) |
| `upstream` | the upstream's JSON body, byte for byte (its own `error.type`, `message`, `request_id`) | `E-UP-400` |
| `none` | no body | `E-CLIENT-GONE` |

`error.type` is not always one of Anthropic's types. sub2api adds
`upstream_error`, `billing_error`, `billing_service_error`, `rate_limit_exceeded`,
`model_not_found`, `stream_read_error`, `stream_timeout` and `response_too_large`.
Some messages are in Chinese, as upstream sub2api ships them
(`E-AUTH-GROUP-*`, `E-AUTH-KEY-QUOTA-EXHAUSTED`, `E-AUTH-KEY-EXPIRED`, `E-BILLING-KEY-WINDOW`).

### 1.2 `"stream": true` (`Stream form` column)

- **Before any byte of the response is sent**, every error is a plain JSON body
  with the row's HTTP status, also for `"stream": true`. When the upstream had
  already answered `200 text/event-stream` and then failed before any output
  (`E-UP-529`, `E-UP-5XX`, `E-UP-403` from an error event or a broken stream),
  the JSON error keeps `Content-Type: text/event-stream`.
- **After a ping.** A `"stream": true` request that waits for a user or account
  slot gets keepalive pings (`data: {"type": "ping"}`, every
  `concurrency.ping_interval`, default 10 s). The first ping commits
  `HTTP 200` + `text/event-stream`. Any later error arrives as a frame in that
  200 stream; the row's HTTP status is never sent. "after ping" in the
  `Stream form` column marks the rows where this happens.
- **After output started**, errors are frames in the 200 stream (`E-STREAM-*`).
- Frame spellings, exactly:

| Frame | Bytes | Written by |
|---|---|---|
| `data:` frame | `data: {"type":"error","error":{"type":…,"message":…}}` + blank line. **No `event:` line.** | the handler (failover result, limits, pool, fallback) |
| `event: error` frame | `event: error` / `data: {"error":{"message":…,"type":…},"type":"error"}` | the upstream stream reader |
| `event:error` frame | `event:error` / `data:{"error":{"message":…,"type":…},"type":"error"}` (no spaces) | the upstream-error writer, after a ping |

  A `data:`-only frame has the SSE event name `message`; a reader that acts only
  on `event: error` does not see it as an error. Every error frame, whatever its
  `event:` line, has `"type":"error"` at the top of its JSON.
- `E-STREAM-DISCONNECTED`, `E-STREAM-IDLE` and `E-STREAM-LINE-TOO-LONG` end with
  **two** frames: the `event: error` frame of the row, then the handler's
  `data:` frame `{"type":"upstream_error","message":"Upstream request failed"}`.

### 1.3 Headers

- `Retry-After` (whole seconds) only where the column says `yes` / `when known`.
  The upstream's own `Retry-After` is never forwarded.
- `X-Request-ID` on every response: the client's `X-Request-ID` if it sent a
  valid one, else a sub2api UUID. A stream whose upstream answered 200 (before
  any ping) carries the upstream's request id instead (`req_…` for Anthropic).
- `X-Client-Request-ID` on every `/v1` response: a sub2api UUID.
- Both ids are stored with the error in sub2api's ops error log
  (`xsci-token errors --q <id>` finds the row; `--upstream` shows each upstream
  attempt behind it).

## 2. Contract table

Placeholders in `{braces}` are filled per request; `(upstream text)` is the
upstream's `error.message` unchanged; `(upstream type)` its `error.type`.
`HTTP` is the status the client receives (`200` for frames inside a stream).

| ID | HTTP | Body | error.type | code | message | Stream form | Retry-After |
|---|---|---|---|---|---|---|---|
| E-AUTH-MISSING-KEY | 401 | flat | — | `API_KEY_REQUIRED` | `API key is required in Authorization header (Bearer scheme), x-api-key header, or x-goog-api-key header` | JSON | no |
| E-AUTH-INVALID-KEY | 401 | flat | — | `INVALID_API_KEY` | `Invalid API key` | JSON | no |
| E-AUTH-KEY-IN-QUERY | 400 | flat | — | `api_key_in_query_deprecated` | `API key in query parameter is deprecated. Please use Authorization header instead.` | JSON | no |
| E-AUTH-TOO-MANY-INVALID | 429 | flat | — | `INVALID_AUTH_RATE_LIMITED` | `Too many invalid authentication attempts; retry later` | JSON | yes |
| E-AUTH-LOOKUP-OVERLOADED | 503 | flat | — | `API_KEY_AUTH_OVERLOADED` | `API key authentication is temporarily unavailable` | JSON | no |
| E-AUTH-LOOKUP-FAILED | 500 | flat | — | `INTERNAL_ERROR` | `Failed to validate API key` | JSON | no |
| E-AUTH-KEY-DISABLED | 401 | flat | — | `API_KEY_DISABLED` | `API key is disabled` | JSON | no |
| E-AUTH-IP-DENIED | 403 | flat | — | `ACCESS_DENIED` | `Access denied. Your IP is {ip}` | JSON | no |
| E-AUTH-USER-INACTIVE | 401 | flat | — | `USER_INACTIVE` | `User account is not active` | JSON | no |
| E-AUTH-GROUP-DELETED | 403 | flat | — | `GROUP_DELETED` | `API Key 所属分组已删除` | JSON | no |
| E-AUTH-GROUP-DISABLED | 403 | flat | — | `GROUP_DISABLED` | `API Key 所属分组已停用` | JSON | no |
| E-AUTH-GROUP-NOT-ALLOWED | 403 | flat | — | `GROUP_NOT_ALLOWED` | `API Key 所属专属分组不再允许当前用户使用` | JSON | no |
| E-AUTH-NO-SUBSCRIPTION | 403 | flat | — | `SUBSCRIPTION_NOT_FOUND` | `No active subscription found for this group` | JSON | no |
| E-AUTH-KEY-QUOTA-EXHAUSTED | 429 | flat | — | `API_KEY_QUOTA_EXHAUSTED` | `API key 额度已用完` | JSON | no |
| E-AUTH-KEY-EXPIRED | 403 | flat | — | `API_KEY_EXPIRED` | `API key 已过期` | JSON | no |
| E-AUTH-SUBSCRIPTION-INVALID | 403 | flat | — | `SUBSCRIPTION_INVALID` | `{subscription error}` | JSON | no |
| E-AUTH-SUBSCRIPTION-LIMIT | 429 | flat | — | `USAGE_LIMIT_EXCEEDED` | `{subscription error}` | JSON | no |
| E-AUTH-SUBSCRIPTION-MAINTENANCE | 500 | flat | — | `SUBSCRIPTION_MAINTENANCE_FAILED` | `Failed to maintain subscription usage windows` | JSON | no |
| E-AUTH-NO-BALANCE | 403 | flat | — | `INSUFFICIENT_BALANCE` | `Insufficient account balance` | JSON | no |
| E-GROUP-UNASSIGNED | 403 | anthropic | `permission_error` | — | `API Key is not assigned to any group and cannot be used. Please contact the administrator to assign it to a group.` | JSON | no |
| E-GROUP-MODEL-NOT-ALLOWED | 404 | anthropic | `not_found_error` | — | `Model "{model}" is not available for this group` | JSON | no |
| E-REQ-BODY-TOO-LARGE | 413 | anthropic | `invalid_request_error` | — | `Request body too large, limit is {limit}` | JSON | no |
| E-REQ-BODY-READ-TIMEOUT | 408 | anthropic | `invalid_request_error` | — | `Timed out reading request body` | JSON | no |
| E-REQ-BODY-READ-FAILED | 400 | anthropic | `invalid_request_error` | — | `Failed to read request body` | JSON | no |
| E-REQ-BODY-BUSY | 503 | anthropic | `api_error` | — | `Request body buffering capacity is exhausted; retry later` | JSON | yes |
| E-REQ-BODY-EMPTY | 400 | anthropic | `invalid_request_error` | — | `Request body is empty` | JSON | no |
| E-REQ-BODY-INVALID | 400 | anthropic | `invalid_request_error` | — | `Failed to parse request body` | JSON | no |
| E-REQ-MODEL-MISSING | 400 | anthropic | `invalid_request_error` | — | `model is required` | JSON | no |
| E-REQ-MODEL-RULE | 400 | anthropic | `invalid_request_error` | — | `{model rule}` | JSON | no |
| E-LIMIT-USER-CONCURRENCY | 429 | anthropic | `rate_limit_error` | `gateway_concurrency_limit` | `Concurrency limit exceeded for user, please retry later` | JSON; `data:` frame after ping | no |
| E-LIMIT-ACCOUNT-CONCURRENCY | 429 | anthropic | `rate_limit_error` | `gateway_concurrency_limit` | `Concurrency limit exceeded for account, please retry later` | JSON; `data:` frame after ping | no |
| E-LIMIT-QUEUE-FULL | 429 | anthropic | `rate_limit_error` | `gateway_queue_full` | `Too many pending requests, please retry later` | JSON; `data:` frame after ping | no |
| E-LIMIT-SLOT-ERROR | 503 | anthropic | `api_error` | — | `Service temporarily unavailable, please retry later` | JSON; `data:` frame after ping | no |
| E-BILLING-UNAVAILABLE | 503 | anthropic | `billing_service_error` | — | `Billing service temporarily unavailable. Please retry later.` | JSON; `data:` frame after ping | no |
| E-BILLING-DENIED | 403 | anthropic | `billing_error` | — | `{billing error}` | JSON; `data:` frame after ping | no |
| E-BILLING-KEY-WINDOW | 429 | anthropic | `rate_limit_exceeded` | — | `{key window}` | JSON; `data:` frame after ping | no |
| E-BILLING-RPM | 429 | anthropic | `rate_limit_exceeded` | — | `{rpm scope} requests-per-minute limit exceeded` | JSON; `data:` frame after ping | yes |
| E-BILLING-PLATFORM-QUOTA | 429 | anthropic | `rate_limit_exceeded` | — | `{Daily/Weekly/Monthly} usage quota exhausted for this platform.` | JSON; `data:` frame after ping | yes |
| E-POOL-EXHAUSTED | 503 | anthropic | `api_error` | — | `No available accounts: {reason}` | JSON; `data:` frame after ping | when known |
| E-POOL-MODEL-UNSUPPORTED | 404 | anthropic | `model_not_found` | — | `Model "{model}" is not supported by any configured account in this group` | JSON; `data:` frame after ping | no |
| E-UP-400 | 400 | upstream | (upstream type) | — | (upstream text) | JSON; `event:error` frame after ping | no |
| E-UP-REQUEST-429 | 429 | anthropic | `rate_limit_error` | — | (upstream text) | JSON; `event:error` frame after ping | no |
| E-UP-429 | 429 | anthropic | `rate_limit_error` | — | `Upstream rate limit exceeded, please retry later` | JSON; `data:` frame after ping | no |
| E-UP-529 | 503 | anthropic | `overloaded_error` | — | `Upstream service overloaded, please retry later` | JSON; `data:` frame after ping | no |
| E-UP-5XX | 502 | anthropic | `upstream_error` | — | `Upstream service temporarily unavailable` | JSON; `data:` frame after ping | no |
| E-UP-401 | 502 | anthropic | `upstream_error` | — | `Upstream authentication failed, please contact administrator` | JSON; `data:` frame after ping | no |
| E-UP-403 | 502 | anthropic | `upstream_error` | — | `Upstream access forbidden, please contact administrator` | JSON; `data:` frame after ping | no |
| E-UP-403-RULE1 | 502 | anthropic | `upstream_error` | — | (upstream text) | JSON; `data:` frame after ping | no |
| E-UP-OTHER | 502 | anthropic | `upstream_error` | — | `Upstream request failed` | JSON; `event:error` or `data:` frame after ping | no |
| E-UP-RETRIES-EXHAUSTED | 502 | anthropic | `upstream_error` | — | `Upstream request failed after retries` | JSON; `event:error` frame after ping | no |
| E-UP-BEDROCK-CONTENT-FILTER | 400 | anthropic | `invalid_request_error` | — | (upstream text) | JSON; `event:error` frame after ping | no |
| E-STREAM-UPSTREAM-ERROR | 200 | anthropic | `upstream_error` | — | `Upstream access forbidden, please contact administrator` | `data:` frame | no |
| E-STREAM-UPSTREAM-ERROR-RULE1 | 200 | anthropic | `upstream_error` | — | (upstream text) | `data:` frame | no |
| E-STREAM-DISCONNECTED | 200 | anthropic | `stream_read_error` | — | `upstream stream disconnected: {reason}` | `event: error` frame, then `data:` frame | no |
| E-STREAM-IDLE | 200 | anthropic | `stream_timeout` | — | `upstream stream idle for {interval}` | `event: error` frame, then `data:` frame | no |
| E-STREAM-LINE-TOO-LONG | 200 | anthropic | `response_too_large` | — | `upstream SSE line exceeded {n} bytes` | `event: error` frame, then `data:` frame | no |
| E-STREAM-INCOMPLETE | 200 | anthropic | `upstream_error` | — | `Upstream request failed` | `data:` frame | no |
| E-CLIENT-GONE | 499 | none | — | — | — | none | no |
| E-CT-POOL-EXHAUSTED | 503 | anthropic | `api_error` | — | `Service temporarily unavailable` | JSON | when known |
| E-CT-BEDROCK | 404 | anthropic | `not_found_error` | — | `count_tokens endpoint is not supported for Bedrock` | JSON | no |
| E-CT-UP-429 | 429 | anthropic | `upstream_error` | — | `Rate limit exceeded` | JSON | no |
| E-CT-UP-529 | 529 | anthropic | `upstream_error` | — | `Service overloaded` | JSON | no |
| E-CT-UP-ERROR | = upstream | anthropic | `upstream_error` | — | `Upstream request failed` | JSON | no |
| E-CT-REQUEST-FAILED | 502 | anthropic | `upstream_error` | — | `Request failed` | JSON | no |

## 3. What each row means

`Prod 7d` counts client-visible errors in sub2api's ops error log for
2026-11-12 15:54Z – 11-19 15:54Z (`xsci-token errors --since 7d`, matched by
path and message). Prod ran the build before a9563225b in that week, so
request-level 429s were still answered as `E-UP-429`, and prod rule 1 (§4) only
existed for its last hours.

| ID | When it happens | Same request can succeed? | Waiting helps? | Notes | Prod 7d |
|---|---|---|---|---|---|
| E-AUTH-MISSING-KEY | No `Authorization: Bearer`, `x-api-key` or `x-goog-api-key` header. | no | no | Client configuration. | — |
| E-AUTH-INVALID-KEY | Key unknown, or a credential header over the size limit. | no | no | Each one counts toward `E-AUTH-TOO-MANY-INVALID` for the client IP. | — |
| E-AUTH-KEY-IN-QUERY | `?key=` or `?api_key=` on the URL. | no | no | | — |
| E-AUTH-TOO-MANY-INVALID | The client IP sent too many invalid keys (default 120 in 60 s) and is blocked (default 60 s). | after Retry-After | yes | While blocked, a valid key gets this too. Retry-After = seconds left in the block. | — |
| E-AUTH-LOOKUP-OVERLOADED | Too many key lookups in flight (database slow). | yes | yes, seconds | | — |
| E-AUTH-LOOKUP-FAILED | Key lookup failed (database error). | maybe | maybe | | — |
| E-AUTH-KEY-DISABLED | Key disabled. | no | no | | — |
| E-AUTH-IP-DENIED | Key has an IP allow / deny list and the client IP is not allowed. | no | no | `{ip}` is the client IP as sub2api sees it. | — |
| E-AUTH-USER-INACTIVE | The key's user is disabled. | no | no | | — |
| E-AUTH-GROUP-DELETED | The key's group was deleted. | no | no | | — |
| E-AUTH-GROUP-DISABLED | The key's group is disabled. | no | no | | — |
| E-AUTH-GROUP-NOT-ALLOWED | Key bound to an exclusive group the user may no longer use. | no | no | | — |
| E-AUTH-NO-SUBSCRIPTION | Subscription-type group; the user has no active subscription to it. | no | no | | 10 |
| E-AUTH-KEY-QUOTA-EXHAUSTED | The key's own spending quota is used up. | no | no | | — |
| E-AUTH-KEY-EXPIRED | The key is past its expiry time. | no | no | | — |
| E-AUTH-SUBSCRIPTION-INVALID | Subscription expired or suspended. | no | no | `{subscription error}` is a Go error string: `error: code=403 reason="SUBSCRIPTION_EXPIRED" message="subscription has expired" metadata=map[]` (or `SUBSCRIPTION_SUSPENDED` / `subscription is suspended`). | — |
| E-AUTH-SUBSCRIPTION-LIMIT | The subscription's daily / weekly / monthly USD limit is reached. | after the window resets | yes, until reset | Same string format, reason `DAILY_LIMIT_EXCEEDED` / `WEEKLY_…` / `MONTHLY_…`. No Retry-After. | — |
| E-AUTH-SUBSCRIPTION-MAINTENANCE | Advancing the subscription's usage windows failed (database write). | maybe | maybe | | — |
| E-AUTH-NO-BALANCE | Balance-mode group and the user's balance is ≤ 0. | no | no | Needs a top-up. | — |
| E-GROUP-UNASSIGNED | The key has no group and ungrouped keys may not be scheduled (prod: not allowed). | no | no | | — |
| E-GROUP-MODEL-NOT-ALLOWED | The group's model allowlist is on and the requested model is not on it. | no | no | Another model may be allowed. | — |
| E-REQ-BODY-TOO-LARGE | Body over `gateway.max_body_size`. | no | no | `{limit}` like `256MB`. With the group allowlist on, the allowlist reads the body first and the text is `Request body is too large` (same status and type). | — |
| E-REQ-BODY-READ-TIMEOUT | The client was too slow sending the body. | yes | — | | — |
| E-REQ-BODY-READ-FAILED | The body could not be read (truncated, bad encoding, transport error). | maybe | — | | — |
| E-REQ-BODY-BUSY | sub2api's request-body memory budget is exhausted (many large bodies in flight). | yes | yes, seconds | Retry-After: `1`. | — |
| E-REQ-BODY-EMPTY | Empty body. | no | no | | — |
| E-REQ-BODY-INVALID | Body is not valid JSON for the Messages API. | no | no | | — |
| E-REQ-MODEL-MISSING | No `model`. | no | no | | — |
| E-REQ-MODEL-RULE | A `*-5-5` model with a thinking setup that model does not take; checked before sending. | no | no | `{model rule}` names the rule, e.g. `claude-sonnet-5-5 requires adaptive thinking or thinking.type=between_tools; omit thinking or use one of those modes`. | — |
| E-LIMIT-USER-CONCURRENCY | The user is at its concurrency limit and no slot freed within 30 s. | yes | yes | A stream request is pinged while it waits, so this usually arrives as a `data:` frame in a 200 stream (prod 7d: 2147 of 2969 were stream requests). | 2969 |
| E-LIMIT-ACCOUNT-CONCURRENCY | Every candidate account stayed busy past the account wait timeout. | yes | yes | | — |
| E-LIMIT-QUEUE-FULL | Too many requests are already waiting for the user's or the account's slots; answered at once. | yes | yes | | 182 |
| E-LIMIT-SLOT-ERROR | The concurrency store (Redis) failed. | maybe | yes | | 7 |
| E-BILLING-UNAVAILABLE | The balance / subscription lookup failed or the billing circuit breaker is open. | yes | yes | | — |
| E-BILLING-DENIED | The billing re-check after the slot wait failed: balance below the threshold (`insufficient balance`), subscription invalid (`subscription is invalid or expired`) or a subscription window limit (`daily usage limit exceeded`, `weekly …`, `monthly …`). | no | window limits: until reset | The same situations as `E-AUTH-NO-BALANCE` / `E-AUTH-SUBSCRIPTION-*`, caught after the wait, with another status and body. | — |
| E-BILLING-KEY-WINDOW | The key's 5 h / 1 d / 7 d spending window is used up. | after the window | yes | `{key window}`: `api key 5小时限额已用完`, `api key 日限额已用完`, `api key 7天限额已用完`. No Retry-After. | — |
| E-BILLING-RPM | User or group requests-per-minute limit. | next minute | yes | `{rpm scope}`: `user` / `group`. Retry-After = seconds to the next minute. | — |
| E-BILLING-PLATFORM-QUOTA | The user's daily / weekly / monthly quota for the platform is used up. | after the reset | yes | Retry-After = seconds to the window reset (60 when unknown). | — |
| E-POOL-EXHAUSTED | No account of the group can take the model now (rate-limited, overloaded, temporarily unschedulable, or none schedulable). | yes, later | yes | Retry-After (OxSci) = seconds until the first account becomes selectable, when sub2api knows it; same estimate as `GET /v1/sub2api/availability`. `{reason}` is `no available accounts` with the default scheduler. It can also follow upstream errors: after an upstream 503 sub2api waits 2 s, forgets which accounts it already tried and selects again; if by then every account is cooling down, this row is the answer. | 7515 |
| E-POOL-MODEL-UNSUPPORTED | The group has accounts, but none is configured for the model. | no | no | Another model may be served. | — |
| E-UP-400 | The upstream rejected the request with 400 (invalid parameter, model id not valid on that upstream, client too old for the model, …). | no | no | No failover. sub2api first retries silently on thinking-signature and thinking-budget 400s. A few 400s are account problems (Anthropic credit balance too low, organization disabled): those take the account out and fail over, and if every account fails that way the answer is `E-UP-OTHER`. | 31 |
| E-UP-REQUEST-429 | An Anthropic subscription account answered 429 `rate_limit_error` "Error" with no rate-limit window headers: Anthropic rejected this request, not the account (P160 T3). | no — every account gives the same answer | not observed | No failover; the account is not marked. Until a9563225b these were `E-UP-429`. | — |
| E-UP-429 | Every tried account answered 429 (account rate limit). | yes, later | yes | Each account is cooled down (to its window reset, or 5 s). No Retry-After; `GET /v1/sub2api/availability` returns `retry_after_s`. | 66 |
| E-UP-529 | Every tried account was overloaded (HTTP 529, or an `overloaded_error` event before any output). | yes, later | yes | Overloaded accounts cool down (prod: 10 min). | — |
| E-UP-5XX | Every tried account answered 500 / 502 / 503 / 504, the connection failed, or the stream broke before its first byte. | yes | maybe | A broken stream is retried on the same account first. | 4 |
| E-UP-401 | Every tried account's credential was rejected. | no | an admin has to act | | — |
| E-UP-403 | Every tried account answered 403, or sent a non-overload `event: error` before any output (content filter, `Unable to complete this request…`, other). | no | no | The upstream's reason is not passed on (see `E-UP-403-RULE1`). Subscription accounts retry a 403 on the same account first (up to 5 tries / 10 s). | — |
| E-UP-403-RULE1 | As `E-UP-403`, with prod rule 1 loaded and the upstream error containing `content filtering`, `Unable to complete this request` or `Overloaded`. | content filter: no; `Unable to complete…`: yes | — | Message = the upstream's `error.message`. | — |
| E-UP-OTHER | (a) The first account answered another 4xx (405, 409, 413, 422, a 404 not about the model, …): no failover. (b) Failover ran out on a status without its own row (402, 404 model-not-found on every account, an account-level 400, 501, 520, …). (c) Local failure before or while forwarding (no credential, request build, empty upstream response). | (a) no; (b), (c) maybe | — | Stream after ping: (a) `event:error` frame, (b) and (c) `data:` frame. Prod 7d includes `E-STREAM-INCOMPLETE` frames (same text). | 18 |
| E-UP-RETRIES-EXHAUSTED | An API-key account with custom error codes got an unlisted 4xx and was retried on the same account until the 5-try / 10 s budget ran out. | no | no | No prod account has custom error codes. | — |
| E-UP-BEDROCK-CONTENT-FILTER | A Bedrock 500 / 502 / 503 / 504 whose message contains `blocked by content filtering policy`. | no | no | No failover. | — |
| E-STREAM-UPSTREAM-ERROR | The upstream sent `event: error` after output had started (`Overloaded`, `api_error`, content filter, …). | Overloaded / api_error: yes; content filter: no — the text does not say which | — | No failover once output started. Without rule 1 every mid-stream upstream error reads like this. | 7 |
| E-STREAM-UPSTREAM-ERROR-RULE1 | As `E-STREAM-UPSTREAM-ERROR`, with prod rule 1 loaded and a keyword in the upstream event. | as the upstream text says | — | Other mid-stream errors (no keyword) stay `E-STREAM-UPSTREAM-ERROR`. | — |
| E-STREAM-DISCONNECTED | The upstream connection broke after output started. | yes | — | `{reason}`: `connection reset by peer`, `unexpected EOF`, `EOF`, `… timeout`, `upstream connection error`, …. Followed by the fallback `data:` frame (§1.2). | 3 |
| E-STREAM-IDLE | No upstream data for `gateway.stream_data_interval_timeout` after output started. | yes | — | `{interval}` like `3m0s` (default 180 s); off when the setting is 0. Followed by the fallback frame. | — |
| E-STREAM-LINE-TOO-LONG | One upstream SSE line was longer than `gateway.max_line_size`. | no | no | Followed by the fallback frame. | — |
| E-STREAM-INCOMPLETE | The upstream stream ended without `message_stop` and without an error event; also how Bedrock stream failures end. | yes | — | Prod 7d is counted under `E-UP-OTHER` (same text). | — |
| E-CLIENT-GONE | The client disconnected or cancelled before the answer. | — | — | Only in sub2api's logs; nobody receives it. | 1 |
| E-CT-POOL-EXHAUSTED | count_tokens: no account available. | yes, later | yes | Same Retry-After as `E-POOL-EXHAUSTED`, other text. count_tokens never fails over. | 45 |
| E-CT-BEDROCK | count_tokens selected a Bedrock account. | maybe — selection is per request | — | Bedrock has no count_tokens. | 108 |
| E-CT-UP-429 | count_tokens: the upstream answered 429. | yes, later | yes | Status passed through. | — |
| E-CT-UP-529 | count_tokens: the upstream answered 529. | yes, later | yes | Status passed through (529, not 503). | 3 |
| E-CT-UP-ERROR | count_tokens: the upstream answered any other error status. | depends on the status | — | Status passed through, text replaced (also for 400). | 31 |
| E-CT-REQUEST-FAILED | count_tokens: the connection to the upstream failed. | yes | maybe | | — |

## 4. Prod error-passthrough rule 1

Runtime configuration on prod since 2026-11-19 (P160 T3 Q2), not code:

```json
{"id": 1, "name": "anthropic: pass through mid-stream upstream errors", "enabled": true, "priority": 10,
 "platforms": ["anthropic"], "error_codes": [403], "match_mode": "all",
 "keywords": ["content filtering", "Unable to complete this request", "Overloaded"],
 "passthrough_code": false, "response_code": 502, "passthrough_body": true}
```

It matches an upstream error with status 403 whose JSON contains any keyword
(case-insensitive substring of the whole error JSON, so `Overloaded` also matches
`overloaded_error`). Mid-stream and pre-output `event: error` events count as
403 here, except an `overloaded_error` event before any output, which is 529.
With the rule loaded:

- `E-UP-403` becomes `E-UP-403-RULE1` (502, `upstream_error`, upstream text);
- `E-STREAM-UPSTREAM-ERROR` becomes `E-STREAM-UPSTREAM-ERROR-RULE1` (frame,
  `upstream_error`, upstream text);
- nothing else changes; errors without a keyword keep their row.

The test loads exactly this rule for the `-RULE1` rows and for the "rule loaded,
no keyword" drives of `E-UP-403` and `E-STREAM-UPSTREAM-ERROR`. Deleting the
rule on prod reverts to the rows without `-RULE1`.

## 5. Settings this contract assumes

Prod, read 2026-11-19: `allow_ungrouped_key_scheduling` false;
`min_claude_code_version` / `max_claude_code_version` empty; the beta policy has
no `block` rule; error-passthrough rules: rule 1 only; the Anthropic accounts
have no pool mode and no custom error codes (P160 T3). Config defaults for the
rest (`gateway.scheduling.load_batch_enabled`, the invalid-auth limiter, the
ping interval). A change to any of these can add or change rows; check §6.

## 6. Not covered

- `POST /v1/responses`, `POST /v1/chat/completions`, and `/v1/messages` on an
  OpenAI-platform group (routed to the OpenAI gateway): their own error bodies
  and stream events.
- Gemini (`/v1beta/…`): Google error format.
- Antigravity (`/antigravity/…`): its own forwarders.
- `GET /v1/models`, `/v1/usage`, `/v1/sub2api/*`: not model calls.
- Off for OxSci's anthropic groups (see §5); each answers with its own error
  when turned on: the Claude Code version gate (400), beta-policy `block` rules
  (400 `beta feature … is not allowed`), content moderation / security audit,
  the reasoning-effort policy (403 `permission_error`), composite groups
  (400 `Model is not supported by composite groups`), TypeSafe System One
  groups (404), group profit control (503 `No available accounts: all candidates
  rejected by group profit control`), `gateway.failover_on_400`, the Antigravity
  prompt-too-long fallback. Warmup interception answers with a mock 200, not an
  error.
- Not reachable in this build: the handler's own `Invalid API key` /
  `User context not found` (the middleware always sets the context);
  `USER_NOT_FOUND` (a key without a user is looked up as unknown →
  `E-AUTH-INVALID-KEY`); a bare `No available accounts` (no scheduler path
  returns an account without a slot or a wait plan).
- Below the router: header / body read deadlines of the HTTP server (the
  connection closes), and a panic (500 from the recovery middleware, generic
  body).

## 7. Changing this contract

A change to what a client sees (status, body, `error.type`, `code`, message,
frame, Retry-After) on these endpoints — our own commit or an upstream sync —
must update, in the same commit:

1. the code;
2. `ceContract` in `backend/internal/handler/xsci_client_error_contract_test.go`
   (the row and its drives);
3. §2 and §3 of this document (same ids, same order, same cells).

`go test -tags unit ./internal/handler/ -run TestXSciClientErrorContract` runs
both checks: `TestXSciClientErrorContract` drives every row,
`TestXSciClientErrorContractMatchesDoc` compares this document with
`ceContract`. A sub2api sync (upstream rebase) is not done until both pass.
Tell the consumers (P175 / oma-core) about any row that changed.

## 8. How the test drives the rows

The harness (`xsci_client_error_contract_harness_test.go`) builds the `/v1`
middleware chain of `routes/gateway.go` in the same order (request logger, body
limit, client request id, ops error logger, endpoint, API-key auth, group model
allowlist, group assignment; the composite-group rewrite is left out, a no-op
for anthropic groups), a real `GatewayHandler` and `GatewayService`, a scripted
upstream, and in-memory repositories, and sends one HTTP request per drive.

Driven at the closest function instead, because the real path needs state a
unit test cannot set cheaply:

- `E-LIMIT-USER-CONCURRENCY`: the 30 s user-slot wait is a constant; the drive
  calls the same wait with 60 ms and the handler's error writer.
- `E-BILLING-UNAVAILABLE`, `E-BILLING-KEY-WINDOW`, `E-BILLING-RPM`,
  `E-BILLING-PLATFORM-QUOTA` and two `E-BILLING-DENIED` drives: the billing
  error is fed to `billingErrorDetails` and the handler's writer (the same two
  calls the handler makes). `E-BILLING-DENIED` (balance) is also driven end to
  end, plain and after a ping.
- `E-AUTH-TOO-MANY-INVALID`: invalid keys are sent until the limiter blocks,
  with the threshold lowered to 10.
