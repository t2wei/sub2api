# Client error contract — Anthropic surface (OxSci)

Every error sub2api returns to a client on `POST /v1/messages` and
`POST /v1/messages/count_tokens` (groups on the `anthropic` platform: Anthropic
OAuth / setup-token / API-key accounts and Bedrock accounts), as the client sees
it on the wire. It is written for callers that classify these errors (e.g. the
oma-core runtime adapter calling token.xsci.ai through the Claude Agent SDK /
Claude Code CLI) and states sub2api facts only: what happened, whether the same
request can succeed, whether waiting helps. What a caller does about each row is
the caller's decision; §4 is the short version for consumers.

The table is locked by `backend/internal/handler/xsci_client_error_contract_test.go`:
every row is driven through the real code path and checked against what the
client receives, and the test fails if this document's §2 table and the test's
row table differ in any cell. See §8 before changing either.

Build: `oxsci-custom-v3` after 327cf8ae8 (upstream v0.2.23 + OxSci commits),
P160 T9 rework.

## 1. How an error looks on the wire

### 1.1 Two kinds of answer (`Class` column)

**(a) The upstream's own answer to this request.** sub2api returns what the
upstream answered, unchanged: its HTTP status and its JSON body (`type`,
`error.type`, `error.message`, `request_id`) byte for byte; in a stream, the
upstream's `event: error` frame as the upstream sent it. There is **no
`error.code`**. This is the answer whenever sub2api did not compute anything
on top: a 400 or other 4xx the upstream gave for the request, a request-level
429, an error event inside the upstream's stream, count_tokens answers.

**(b) A conclusion sub2api computed** from more than one upstream answer or
from its own state: no account available, failover ran out, concurrency /
queue limits, billing / quota / subscription / RPM limits, the caller's own
key, a model no account of the group can serve, local failures. These use the
standard Anthropic envelope plus a stable `error.code`:

```json
{"type":"error",
 "error":{"type":"rate_limit_error","code":"upstream_rate_limited",
          "message":"Upstream rate limit exceeded, please retry later",
          "upstream":{"status":429,"type":"rate_limit_error","message":"…","request_id":"req_…"}},
 "request_id":"5f0c…"}
```

- `error.type` is the standard Anthropic type of the HTTP status (table below),
  so a client that only knows the standard takes the right path.
- `error.code` is always present on (b) and never on (a): its absence means
  "this is the upstream's own answer".
- `error.upstream` is present only when upstream answers led to the conclusion:
  the last upstream answer — `status` (as sub2api classified it: an
  `overloaded_error` event before any output counts as 529), `type`, `message`,
  `request_id`. `message` is left out when the answer is about the account
  itself (upstream 401 / 402 / 403, credit balance, disabled organization).
  Nothing that identifies an account is ever included.
- `request_id` is the response's `X-Request-ID`.
- `Retry-After` (whole seconds) is sent where the `Retry-After` column says so.

| HTTP | error.type of a (b) answer |
|---|---|
| 400, 408, other 4xx | `invalid_request_error` |
| 401 | `authentication_error` |
| 402 | `billing_error` |
| 403 | `permission_error` |
| 404 | `not_found_error` |
| 413 | `request_too_large` |
| 429 | `rate_limit_error` |
| 529 | `overloaded_error` |
| 500, 502, 503 | `api_error` |

Statuses outside Anthropic's own table that sub2api keeps: **408** (the client
was too slow sending the body; HTTP's own code, SDKs retry it); **502** (the
upstream failed or no upstream account was usable; HTTP's gateway code, retried
by every SDK like 500); **503** (sub2api itself is out of capacity: account
pool, body buffer, billing lookup, concurrency store; retried like 500 and
carries `Retry-After` when known). 529 is used only for upstream overload,
which clients treat specially (Claude Code's `--fallback-model` switches model
after repeated 529s); sub2api's own capacity limits must not trigger that.

`error.code` values: the API-key middleware's codes keep their original
spelling (`API_KEY_REQUIRED`, `INVALID_API_KEY`, …), billing answers use
sub2api's billing reasons (`USER_RPM_EXCEEDED`, …), the rest are lower
snake_case. Compare them as exact strings.

`Body` column:

| Body | Shape |
|---|---|
| `envelope` | the (b) envelope above |
| `upstream` | the upstream's JSON body, byte for byte (a) |
| `upstream, enveloped` | (a) from an upstream whose body is not an Anthropic error (Bedrock): the upstream's message in `{"type":"error","error":{"type","message"},"request_id"}` with the upstream's request id; no code |
| `none` | no body |

Some messages are in Chinese, as upstream sub2api ships them (`E-AUTH-GROUP-*`,
`E-AUTH-KEY-QUOTA-EXHAUSTED`, `E-AUTH-KEY-EXPIRED`, `E-BILLING-KEY-WINDOW`);
the code is what to match on.

### 1.2 `"stream": true` (`Stream form` column)

- **Before any byte of the response is sent**, every error is JSON with the
  row's status and `Content-Type: application/json`, also for `"stream": true`
  and also when the upstream had already answered `200 text/event-stream`.
- **After a ping.** A `"stream": true` request that waits for a user or account
  slot gets keepalive pings (`data: {"type": "ping"}`, every
  `concurrency.ping_interval`, default 10 s). The first ping commits
  `HTTP 200` + `text/event-stream`; any later error is a frame in that stream
  and the row's HTTP status is never sent ("after ping" in the column).
- **After output started**, errors are frames in the 200 stream.
- Every error frame is `event: error` + `data: <one-line JSON>` + a blank
  line, and a failed stream carries **exactly one** error frame, last:

| Stream form | Frame data |
|---|---|
| `frame` / `frame after ping` | the (b) envelope |
| `upstream frame` / `upstream frame after ping` | the upstream's own error, as sent (mid-stream events: the upstream's `data:` line; an (a) HTTP answer after a ping: its JSON body on one line) |

### 1.3 Headers

- `Retry-After` only where the column says `yes` / `when known`. The upstream's
  own `Retry-After` is never forwarded, also not with an (a) answer.
- `X-Request-ID` on every response: the client's `X-Request-ID` if it sent a
  valid one, else a sub2api UUID. A stream whose upstream answered 200 carries
  the upstream's request id instead (`req_…` for Anthropic).
- `X-Client-Request-ID` on every `/v1` response: a sub2api UUID.
- Both ids are stored with the error in sub2api's ops error log
  (`xsci-token errors --q <id>` finds the row; `--upstream` shows each upstream
  attempt behind it).

## 2. Contract table

Placeholders in `{braces}` are filled per request; `(upstream text)` is the
upstream's `error.message` unchanged; `(upstream type)` its `error.type`.
`HTTP` is the status the client receives (`200` for frames inside a stream;
`= upstream`: the upstream's status; `= rule`: the rule's status).

| ID | Class | HTTP | Body | error.type | error.code | message | Stream form | Retry-After |
|---|---|---|---|---|---|---|---|---|
| E-AUTH-MISSING-KEY | b | 401 | envelope | `authentication_error` | `API_KEY_REQUIRED` | `API key is required in Authorization header (Bearer scheme), x-api-key header, or x-goog-api-key header` | JSON | no |
| E-AUTH-INVALID-KEY | b | 401 | envelope | `authentication_error` | `INVALID_API_KEY` | `Invalid API key` | JSON | no |
| E-AUTH-KEY-IN-QUERY | b | 400 | envelope | `invalid_request_error` | `api_key_in_query_deprecated` | `API key in query parameter is deprecated. Please use Authorization header instead.` | JSON | no |
| E-AUTH-TOO-MANY-INVALID | b | 429 | envelope | `rate_limit_error` | `INVALID_AUTH_RATE_LIMITED` | `Too many invalid authentication attempts; retry later` | JSON | yes |
| E-AUTH-LOOKUP-OVERLOADED | b | 503 | envelope | `api_error` | `API_KEY_AUTH_OVERLOADED` | `API key authentication is temporarily unavailable` | JSON | no |
| E-AUTH-LOOKUP-FAILED | b | 500 | envelope | `api_error` | `INTERNAL_ERROR` | `Failed to validate API key` | JSON | no |
| E-AUTH-KEY-DISABLED | b | 401 | envelope | `authentication_error` | `API_KEY_DISABLED` | `API key is disabled` | JSON | no |
| E-AUTH-IP-DENIED | b | 403 | envelope | `permission_error` | `ACCESS_DENIED` | `Access denied. Your IP is {ip}` | JSON | no |
| E-AUTH-USER-INACTIVE | b | 401 | envelope | `authentication_error` | `USER_INACTIVE` | `User account is not active` | JSON | no |
| E-AUTH-GROUP-DELETED | b | 403 | envelope | `permission_error` | `GROUP_DELETED` | `API Key 所属分组已删除` | JSON | no |
| E-AUTH-GROUP-DISABLED | b | 403 | envelope | `permission_error` | `GROUP_DISABLED` | `API Key 所属分组已停用` | JSON | no |
| E-AUTH-GROUP-NOT-ALLOWED | b | 403 | envelope | `permission_error` | `GROUP_NOT_ALLOWED` | `API Key 所属专属分组不再允许当前用户使用` | JSON | no |
| E-AUTH-NO-SUBSCRIPTION | b | 403 | envelope | `permission_error` | `SUBSCRIPTION_NOT_FOUND` | `No active subscription found for this group` | JSON | no |
| E-AUTH-KEY-QUOTA-EXHAUSTED | b | 402 | envelope | `billing_error` | `API_KEY_QUOTA_EXHAUSTED` | `API key 额度已用完` | JSON | no |
| E-AUTH-KEY-EXPIRED | b | 403 | envelope | `permission_error` | `API_KEY_EXPIRED` | `API key 已过期` | JSON | no |
| E-AUTH-SUBSCRIPTION-MAINTENANCE | b | 500 | envelope | `api_error` | `SUBSCRIPTION_MAINTENANCE_FAILED` | `Failed to maintain subscription usage windows` | JSON | no |
| E-BILLING-NO-BALANCE | b | 402 | envelope | `billing_error` | `INSUFFICIENT_BALANCE` | `{balance message}` | JSON; frame after ping | no |
| E-BILLING-SUBSCRIPTION-INVALID | b | 403 | envelope | `permission_error` | `SUBSCRIPTION_INVALID` | `{subscription message}` | JSON; frame after ping | no |
| E-BILLING-SUBSCRIPTION-LIMIT | b | 429 | envelope | `rate_limit_error` | `USAGE_LIMIT_EXCEEDED` | `{daily/weekly/monthly} usage limit exceeded` | JSON; frame after ping | no |
| E-BILLING-UNAVAILABLE | b | 503 | envelope | `api_error` | `BILLING_SERVICE_ERROR` | `Billing service temporarily unavailable. Please retry later.` | JSON; frame after ping | no |
| E-BILLING-KEY-WINDOW | b | 429 | envelope | `rate_limit_error` | `API_KEY_RATE_{5H,1D,7D}_EXCEEDED` | `{key window}` | JSON; frame after ping | no |
| E-BILLING-RPM | b | 429 | envelope | `rate_limit_error` | `{USER,GROUP}_RPM_EXCEEDED` | `{rpm scope} requests-per-minute limit exceeded` | JSON; frame after ping | yes |
| E-BILLING-PLATFORM-QUOTA | b | 429 | envelope | `rate_limit_error` | `USER_PLATFORM_{DAILY,WEEKLY,MONTHLY}_QUOTA_EXHAUSTED` | `{Daily/Weekly/Monthly} usage quota exhausted for this platform.` | JSON; frame after ping | yes |
| E-GROUP-UNASSIGNED | b | 403 | envelope | `permission_error` | `group_unassigned` | `API Key is not assigned to any group and cannot be used. Please contact the administrator to assign it to a group.` | JSON | no |
| E-GROUP-MODEL-NOT-ALLOWED | b | 404 | envelope | `not_found_error` | `model_not_allowed` | `Model "{model}" is not available for this group` | JSON | no |
| E-REQ-BODY-TOO-LARGE | b | 413 | envelope | `request_too_large` | `request_body_too_large` | `Request body too large, limit is {limit}` | JSON | no |
| E-REQ-BODY-READ-TIMEOUT | b | 408 | envelope | `invalid_request_error` | `request_body_timeout` | `Timed out reading request body` | JSON | no |
| E-REQ-BODY-READ-FAILED | b | 400 | envelope | `invalid_request_error` | `request_body_unreadable` | `Failed to read request body` | JSON | no |
| E-REQ-BODY-BUSY | b | 503 | envelope | `api_error` | `request_body_buffer_full` | `Request body buffering capacity is exhausted; retry later` | JSON | yes |
| E-REQ-BODY-EMPTY | b | 400 | envelope | `invalid_request_error` | `request_body_empty` | `Request body is empty` | JSON | no |
| E-REQ-BODY-INVALID | b | 400 | envelope | `invalid_request_error` | `request_body_invalid` | `Failed to parse request body` | JSON | no |
| E-REQ-MODEL-MISSING | b | 400 | envelope | `invalid_request_error` | `model_required` | `model is required` | JSON | no |
| E-REQ-MODEL-RULE | b | 400 | envelope | `invalid_request_error` | `model_parameters_unsupported` | `{model rule}` | JSON | no |
| E-LIMIT-USER-CONCURRENCY | b | 429 | envelope | `rate_limit_error` | `gateway_concurrency_limit` | `Concurrency limit exceeded for user, please retry later` | JSON; frame after ping | no |
| E-LIMIT-ACCOUNT-CONCURRENCY | b | 429 | envelope | `rate_limit_error` | `gateway_concurrency_limit` | `Concurrency limit exceeded for account, please retry later` | JSON; frame after ping | no |
| E-LIMIT-QUEUE-FULL | b | 429 | envelope | `rate_limit_error` | `gateway_queue_full` | `Too many pending requests, please retry later` | JSON; frame after ping | no |
| E-LIMIT-SLOT-ERROR | b | 503 | envelope | `api_error` | `gateway_concurrency_unavailable` | `Service temporarily unavailable, please retry later` | JSON; frame after ping | no |
| E-POOL-EXHAUSTED | b | 503 | envelope | `api_error` | `no_available_accounts` | `No available accounts: {reason}` | JSON; frame after ping | when known |
| E-POOL-MODEL-UNSUPPORTED | b | 404 | envelope | `not_found_error` | `model_unsupported` | `Model "{model}" is not supported by any configured account in this group` | JSON; frame after ping | no |
| E-UP-MODEL-NOT-FOUND | b | 404 | envelope | `not_found_error` | `model_unsupported` | `Model "{model}" is not supported by any configured account in this group` | JSON; frame after ping | no |
| E-UP-400 | a | 400 | upstream | (upstream type) | — | (upstream text) | JSON; upstream frame after ping | no |
| E-UP-4XX | a | = upstream | upstream | (upstream type) | — | (upstream text) | JSON; upstream frame after ping | no |
| E-UP-REQUEST-429 | a | 429 | upstream | `rate_limit_error` | — | (upstream text) | JSON; upstream frame after ping | no |
| E-UP-BEDROCK-CONTENT-FILTER | a | 400 | upstream, enveloped | `invalid_request_error` | — | (upstream text) | JSON; frame after ping | no |
| E-UP-429 | b | 429 | envelope | `rate_limit_error` | `upstream_rate_limited` | `Upstream rate limit exceeded, please retry later` | JSON; frame after ping | when known |
| E-UP-529 | b | 529 | envelope | `overloaded_error` | `upstream_overloaded` | `Upstream service overloaded, please retry later` | JSON; frame after ping | no |
| E-UP-5XX | b | 502 | envelope | `api_error` | `upstream_unavailable` | `Upstream service temporarily unavailable` | JSON; frame after ping | no |
| E-UP-ACCOUNT | b | 502 | envelope | `api_error` | `upstream_account_error` | `Upstream account unavailable, please contact administrator` | JSON; frame after ping | no |
| E-UP-REQUEST-FAILED | b | 502 | envelope | `api_error` | `upstream_request_failed` | `Upstream request failed` | JSON; frame after ping | no |
| E-UP-PASSTHROUGH-RULE | b | = rule | envelope | (type of the status) | `error_passthrough_rule` | (rule message) | JSON; frame after ping | no |
| E-STREAM-UPSTREAM-ERROR | a | 200 | upstream | (upstream type) | — | (upstream text) | upstream frame | no |
| E-STREAM-DISCONNECTED | b | 200 | envelope | `api_error` | `stream_read_error` | `upstream stream disconnected: {reason}` | frame | no |
| E-STREAM-IDLE | b | 200 | envelope | `api_error` | `stream_timeout` | `upstream stream idle for {interval}` | frame | no |
| E-STREAM-LINE-TOO-LONG | b | 200 | envelope | `api_error` | `response_too_large` | `upstream SSE line exceeded {n} bytes` | frame | no |
| E-STREAM-INCOMPLETE | b | 200 | envelope | `api_error` | `upstream_stream_incomplete` | `Upstream stream ended unexpectedly` | frame | no |
| E-CLIENT-GONE | — | 499 | none | — | — | — | none | no |
| E-CT-POOL-EXHAUSTED | b | 503 | envelope | `api_error` | `no_available_accounts` | `Service temporarily unavailable` | JSON | when known |
| E-CT-BEDROCK | b | 404 | envelope | `not_found_error` | `count_tokens_unsupported` | `count_tokens endpoint is not supported for Bedrock` | JSON | no |
| E-CT-UP-ANSWER | a | = upstream | upstream | (upstream type) | — | (upstream text) | JSON | no |
| E-CT-REQUEST-FAILED | b | 502 | envelope | `api_error` | `upstream_unavailable` | `Request failed` | JSON | no |

## 3. What each row means

`Prod 7d (old build)` counts client-visible errors in sub2api's ops error log
for 2026-11-12 15:54Z – 11-19 15:54Z under the build before this contract
(`xsci-token errors --since 7d`, matched by path and message, P160 T9 v1).

| ID | When it happens | Same request can succeed? | Waiting helps? | Notes | Prod 7d (old build) |
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
| E-AUTH-KEY-QUOTA-EXHAUSTED | The key's own spending quota is used up. | no | no | An admin raises the quota. | — |
| E-AUTH-KEY-EXPIRED | The key is past its expiry time. | no | no | | — |
| E-AUTH-SUBSCRIPTION-MAINTENANCE | Advancing the subscription's usage windows failed (database write). | maybe | maybe | | — |
| E-BILLING-NO-BALANCE | Balance-mode group and the user's balance is ≤ 0: at auth, or found by the billing re-check after the slot wait. | no | no | `{balance message}`: `Insufficient account balance` at auth, `insufficient balance` after the wait. Needs a top-up. | — |
| E-BILLING-SUBSCRIPTION-INVALID | The subscription expired or was suspended: at auth, or found after the slot wait. | no | no | `{subscription message}`: `subscription has expired`, `subscription is suspended` (auth), `subscription is invalid or expired` (after the wait). | — |
| E-BILLING-SUBSCRIPTION-LIMIT | The subscription's daily / weekly / monthly USD limit is reached: at auth, or after the slot wait. Same answer on both paths. | after the window resets | yes, until reset | No Retry-After. | — |
| E-BILLING-UNAVAILABLE | The balance / subscription lookup failed or the billing circuit breaker is open. | yes | yes | | — |
| E-BILLING-KEY-WINDOW | The key's 5 h / 1 d / 7 d spending window is used up. | after the window | yes | `{key window}`: `api key 5小时限额已用完`, `api key 日限额已用完`, `api key 7天限额已用完`; the code names the window. No Retry-After. | — |
| E-BILLING-RPM | User or group requests-per-minute limit. | next minute | yes | `{rpm scope}`: `user` / `group`. Retry-After = seconds to the next minute. | — |
| E-BILLING-PLATFORM-QUOTA | The user's daily / weekly / monthly quota for the platform is used up. | after the reset | yes | Retry-After = seconds to the window reset (60 when unknown). | — |
| E-GROUP-UNASSIGNED | The key has no group and ungrouped keys may not be scheduled (prod: not allowed). | no | no | | — |
| E-GROUP-MODEL-NOT-ALLOWED | The group's model allowlist is on and the requested model is not on it. | no | no | Another model may be allowed. | — |
| E-REQ-BODY-TOO-LARGE | Body over `gateway.max_body_size`. | no | no | `{limit}` like `256MB`. With the group allowlist on, the allowlist reads the body first and the text is `Request body is too large` (same status and code). | — |
| E-REQ-BODY-READ-TIMEOUT | The client was too slow sending the body. | yes | — | | — |
| E-REQ-BODY-READ-FAILED | The body could not be read (truncated, bad encoding, transport error). | maybe | — | | — |
| E-REQ-BODY-BUSY | sub2api's request-body memory budget is exhausted (many large bodies in flight). | yes | yes, seconds | Retry-After: `1`. | — |
| E-REQ-BODY-EMPTY | Empty body. | no | no | | — |
| E-REQ-BODY-INVALID | Body is not valid JSON for the Messages API. | no | no | | — |
| E-REQ-MODEL-MISSING | No `model`. | no | no | | — |
| E-REQ-MODEL-RULE | A `*-5-5` model with a thinking setup that model does not take; checked before sending (messages and count_tokens). | no | no | `{model rule}` names the rule, e.g. `claude-sonnet-5-5 requires adaptive thinking or thinking.type=between_tools; omit thinking or use one of those modes`. | — |
| E-LIMIT-USER-CONCURRENCY | The user is at its concurrency limit and no slot freed within 30 s. | yes | yes | A stream request is pinged while it waits, so this usually arrives as a frame in a 200 stream (old build, 7d: 2147 of 2969 were stream requests). | 2969 |
| E-LIMIT-ACCOUNT-CONCURRENCY | Every candidate account stayed busy past the account wait timeout. | yes | yes | | — |
| E-LIMIT-QUEUE-FULL | Too many requests are already waiting for the user's or the account's slots; answered at once. | yes | yes | | 182 |
| E-LIMIT-SLOT-ERROR | The concurrency store (Redis) failed. | maybe | yes | | 7 |
| E-POOL-EXHAUSTED | No account of the group can take the model now (rate-limited, overloaded, temporarily unschedulable, cooling the model down for a reason other than "model not found", or none schedulable). | yes, later | yes | Retry-After (OxSci) = seconds until the first account becomes selectable, when sub2api knows it; same estimate as `GET /v1/sub2api/availability`. `{reason}` is `no available accounts` with the default scheduler. It can also follow upstream errors: after an upstream 503 sub2api waits 2 s, forgets which accounts it already tried and selects again; if by then every account is cooling down, this row is the answer. | 7515 |
| E-POOL-MODEL-UNSUPPORTED | The group has accounts, but none is configured for the model (model mapping / whitelist). | no | no | Another model may be served. Also on count_tokens. | — |
| E-UP-MODEL-NOT-FOUND | Every account of the group configured for the model answered the upstream's "model not found" (404 `not_found_error`, `model: …`): in this request (failover tried them all; `error.upstream` is the last answer), or earlier, while the model's cooldown on each of them lasts (30 min; no upstream call, no `error.upstream`). | no | no — the cooldown only spares upstream calls | Another model may be served. Same code and text as `E-POOL-MODEL-UNSUPPORTED`; `error.upstream` tells the two apart. Also on count_tokens and in `GET /v1/sub2api/availability` (`code: model_unsupported`). Until P160 T9 the first request was `502 Upstream request failed` and the next ones `503 No available accounts` + Retry-After for the length of the cooldown. | — |
| E-UP-400 | The upstream rejected the request with 400 (invalid parameter, model id not valid on that upstream, client too old for the model, …). | no | no | No failover. sub2api first retries silently on thinking-signature and thinking-budget 400s. Account-level 400s (credit balance too low, organization disabled, identity verification) are not this row: they take the account out and fail over (`E-UP-ACCOUNT`). | 31 |
| E-UP-4XX | The first account answered another 4xx for the request (404 not about the model, 405, 409, 413, 422, …): no failover. Also a 4xx an API-key account with custom error codes kept giving when retried on the same account (5 tries / 10 s). | no | no | The upstream's status and body as they are. 401 / 402 / 403 never come back this way (`E-UP-ACCOUNT`). | part of the old E-UP-OTHER's 18 |
| E-UP-REQUEST-429 | An Anthropic subscription account answered 429 `rate_limit_error` "Error" with no rate-limit window headers: Anthropic rejected this request, not the account (P160 T3). | no — every account gives the same answer | not observed | No failover; the account is not marked. | in E-UP-429's 66 |
| E-UP-BEDROCK-CONTENT-FILTER | A Bedrock 500 / 502 / 503 / 504 whose message contains `blocked by content filtering policy`. | no | no | No failover. Bedrock's body is not an Anthropic error, so its message is put in the envelope (400, no code). | — |
| E-UP-429 | Every tried account answered 429 (account rate limit). | yes, later | yes | Each account is cooled down (to its window reset, or 5 s). Retry-After (OxSci) = seconds until the first account becomes selectable again, the same estimate as `E-POOL-EXHAUSTED`; `GET /v1/sub2api/availability` returns `retry_after_s`. | 66 (incl. request-level) |
| E-UP-529 | Every tried account was overloaded (HTTP 529, or an `overloaded_error` event before any output). | yes, later | yes | Overloaded accounts cool down (prod: 10 min). Status 529 since P160 T9 (was 503). | — |
| E-UP-5XX | Every tried account answered 500 / 502 / 503 / 504 (or another 5xx), the connection failed, or the stream broke before its first byte. | yes | maybe | A broken stream is retried on the same account first. `error.upstream` only when an upstream answered. | 4 |
| E-UP-ACCOUNT | Failover ran out on answers about the accounts themselves: upstream 401 / 403 (the account's credential or permission), 402, or an account-level 400 (credit balance too low, organization disabled, identity verification). Also count_tokens getting such an answer. | no | an admin has to act | Never 401 / 403 to the client: the caller's key is fine (Claude Code would ask to log in again). `error.upstream` has status, type and request id, no message. Subscription accounts retry a 403 on the same account first (up to 5 tries / 10 s). | — |
| E-UP-REQUEST-FAILED | The request could not be completed and no row above applies: a local failure before or while forwarding (no credential, request build), or failover ran out on an answer without its own row (e.g. "model not found" on the accounts tried while an account of the group was not tried). | maybe | maybe | `error.upstream` when an upstream answered. | part of the old E-UP-OTHER's 18 |
| E-UP-PASSTHROUGH-RULE | An admin error-passthrough rule matched the last upstream answer once failover ran out (§5). | per rule | — | Status and message as the rule says (status kept, or the rule's code; upstream text or the rule's text); `error.type` is the standard type of that status; `error.upstream` attached. Rules never change (a) answers. | — |
| E-STREAM-UPSTREAM-ERROR | The upstream sent `event: error` after output had started (`Overloaded`, `api_error`, content filter, …); or before any output with a type other than `overloaded_error`, and every account tried gave such an event. | Overloaded / api_error: yes; content filter: no | — | Forwarded as the upstream sent it, in the 200 stream. No failover once output started; before output it fails over first. | 7 |
| E-STREAM-DISCONNECTED | The upstream connection broke after output started. | yes | — | `{reason}`: `connection reset by peer`, `unexpected EOF`, `EOF`, `… timeout`, `upstream connection error`, …. One frame (until P160 T9 a second, generic frame followed). | 3 |
| E-STREAM-IDLE | No upstream data for `gateway.stream_data_interval_timeout` after output started. | yes | — | `{interval}` like `3m0s` (default 180 s); off when the setting is 0. One frame. | — |
| E-STREAM-LINE-TOO-LONG | One upstream SSE line was longer than `gateway.max_line_size`. | no | no | One frame. | — |
| E-STREAM-INCOMPLETE | The upstream stream ended without `message_stop` and without an error event, after output started; also how Bedrock stream failures end. | yes | — | | part of the old E-UP-OTHER's 18 |
| E-CLIENT-GONE | The client disconnected or cancelled before the answer. | — | — | Only in sub2api's logs; nobody receives it. | 1 |
| E-CT-POOL-EXHAUSTED | count_tokens: no account available. | yes, later | yes | Same code and Retry-After as `E-POOL-EXHAUSTED`, other text. count_tokens never fails over. | 45 |
| E-CT-BEDROCK | count_tokens selected a Bedrock account. | maybe — selection is per request | — | Bedrock has no count_tokens; count locally. | 108 |
| E-CT-UP-ANSWER | count_tokens: the upstream answered an error (429, 529, 4xx, 5xx other than the account's own 401 / 402 / 403). | depends on the status | — | The upstream's status and body as they are (count_tokens never fails over). | 34 |
| E-CT-REQUEST-FAILED | count_tokens: the connection to the upstream failed. | yes | maybe | | — |

## 4. For consumers

- Read `error.code` first and match it as an exact string.
- No `error.code`: the answer is the upstream's own (a) — handle it by the
  standard `error.type` / HTTP status, exactly as for api.anthropic.com. A
  client written only for the standard (or for another gateway) keeps working;
  one that knows sub2api gets the precise reason.
- In a stream, read the JSON of the `event: error` frame; it has the same
  fields as the JSON body.
- `error.upstream` (when present) says what the upstream last said; use it for
  logs and for finer decisions, not instead of the code.
- `GET /v1/sub2api/availability?model=…` answers `{"available":false,"code":…}`
  with the code a request would get now: `no_available_accounts` (with
  `retry_after_s` when known) or `model_unsupported`.

| Codes | Meaning | What helps |
|---|---|---|
| `gateway_concurrency_limit`, `gateway_queue_full`, `gateway_concurrency_unavailable`, `no_available_accounts`, `upstream_rate_limited`, `upstream_overloaded`, `upstream_unavailable`, `upstream_stream_incomplete`, `stream_read_error`, `stream_timeout`, `request_body_buffer_full`, `request_body_timeout`, `API_KEY_AUTH_OVERLOADED`, `BILLING_SERVICE_ERROR`, `INVALID_AUTH_RATE_LIMITED` | Capacity or a transient failure, in sub2api or upstream. | Wait and retry the same request (Retry-After when sent). |
| `USER_RPM_EXCEEDED`, `GROUP_RPM_EXCEEDED`, `USER_PLATFORM_*_QUOTA_EXHAUSTED`, `API_KEY_RATE_*_EXCEEDED`, `USAGE_LIMIT_EXCEEDED` | A usage window of this key / user / subscription is full. | Wait for the window (Retry-After when sent: next minute / quota reset); retrying sooner fails the same way. |
| `model_unsupported`, `model_not_allowed` | No account of this group can serve this model (not configured, the upstream does not know it, or the group's allowlist excludes it). Waiting does not help. | Another model. |
| `upstream_account_error`, `INSUFFICIENT_BALANCE`, `API_KEY_QUOTA_EXHAUSTED`, `BILLING_ERROR`, `SUBSCRIPTION_NOT_FOUND`, `SUBSCRIPTION_INVALID`, `API_KEY_REQUIRED`, `INVALID_API_KEY`, `API_KEY_DISABLED`, `API_KEY_EXPIRED`, `USER_INACTIVE`, `ACCESS_DENIED`, `GROUP_DELETED`, `GROUP_DISABLED`, `GROUP_NOT_ALLOWED`, `group_unassigned` | The key, its user, its group, its money or the upstream accounts behind it are not usable. | An admin. Not retrying, not another model. |
| `api_key_in_query_deprecated`, `request_body_too_large`, `request_body_unreadable`, `request_body_empty`, `request_body_invalid`, `model_required`, `model_parameters_unsupported` | The request itself is wrong. | Change the request. |
| `count_tokens_unsupported` | count_tokens cannot be answered by the selected account. | Count locally. |
| `upstream_request_failed`, `INTERNAL_ERROR`, `SUBSCRIPTION_MAINTENANCE_FAILED`, `response_too_large`, `error_passthrough_rule`, `gateway_error` | Other failures (`gateway_error`: any gateway answer without a more specific code; `error_passthrough_rule`: shaped by an admin rule). | Decide by `error.type` / status; `error.upstream` when present. |

## 5. Error-passthrough rules (prod rule 1)

Runtime configuration on prod since 2026-11-19 (P160 T3 Q2), not code:

```json
{"id": 1, "name": "anthropic: pass through mid-stream upstream errors", "enabled": true, "priority": 10,
 "platforms": ["anthropic"], "error_codes": [403], "match_mode": "all",
 "keywords": ["content filtering", "Unable to complete this request", "Overloaded"],
 "passthrough_code": false, "response_code": 502, "passthrough_body": true}
```

On this surface a rule is consulted only for a gateway answer once failover
ran out (`E-UP-PASSTHROUGH-RULE`); it never changes an upstream answer (a).
The cases rule 1 was made for — mid-stream and pre-output `event: error` with
"Overloaded", content filtering or "Unable to complete this request" — are now
upstream frames forwarded as they are (`E-STREAM-UPSTREAM-ERROR`) or
`E-UP-529`, whatever the rule says. Rule 1 can still match only an HTTP 403
whose body carries a keyword after failover: it turns that `E-UP-ACCOUNT`
answer into `E-UP-PASSTHROUGH-RULE` (502, upstream text as message). Rule 1 is
therefore redundant; deleting it changes only that case back to `E-UP-ACCOUNT`.
The test drives rule 1 on mid-stream, pre-output and HTTP-403 answers.

## 6. Settings this contract assumes

Prod, read 2026-11-19: `allow_ungrouped_key_scheduling` false;
`min_claude_code_version` / `max_claude_code_version` empty; the beta policy has
no `block` rule; error-passthrough rules: rule 1 only; the Anthropic accounts
have no pool mode and no custom error codes (P160 T3). Config defaults for the
rest (`gateway.scheduling.load_batch_enabled`, `gateway.max_account_switches`,
the invalid-auth limiter, the ping interval). The "model not found" cooldown
is 30 minutes (code constant `upstreamModelNotFoundCooldown`). A change to any
of these can add or change rows; check §7.

## 7. Not covered

- `POST /v1/responses`, `POST /v1/chat/completions`, and `/v1/messages` on an
  OpenAI-platform group (routed to the OpenAI gateway): their own error bodies
  and stream events. (On an OpenAI-platform group, `/v1/messages` errors from
  the API-key middleware and the group gate do follow §1.)
- `/antigravity/v1/messages`, Gemini (`/v1beta/…`): their own forwarders.
- `GET /v1/models`, `/v1/usage`, `/v1/sub2api/*` (except the availability
  `code` in §4): not model calls.
- Off for OxSci's anthropic groups (see §6); each answers with its own error
  when turned on (through the same envelope, code `gateway_error`): the Claude
  Code version gate (400), beta-policy `block` rules (400 `beta feature … is not
  allowed`), content moderation / security audit, the reasoning-effort policy
  (403), composite groups (400 `Model is not supported by composite groups`),
  TypeSafe System One groups (404), group profit control (503 `No available
  accounts: all candidates rejected by group profit control`, code
  `no_available_accounts`), `gateway.failover_on_400`, the Antigravity
  prompt-too-long fallback. Warmup interception answers with a mock 200.
- Not reachable in this build: the handler's own `Invalid API key` /
  `User context not found` (the middleware always sets the context);
  `USER_NOT_FOUND` (a key without a user is looked up as unknown →
  `E-AUTH-INVALID-KEY`); a bare `No available accounts` (no scheduler path
  returns an account without a slot or a wait plan).
- Below the router: header / body read deadlines of the HTTP server (the
  connection closes), and a panic (500 from the recovery middleware, generic
  body).

## 8. Changing this contract

The shaping is one OxSci layer: `backend/internal/service/xsci_client_errors.go`
and `backend/internal/handler/xsci_client_errors.go`, switched on per request by
the route middleware `handler.XSciClientErrorContract` (first on `/v1`, and
wrapped around the root `/messages/count_tokens` alias). Upstream functions
reach it through one-line `[OXSCI] P160 T9` hooks; requests without the mark
(other paths, upstream's own unit tests) keep upstream's behaviour.

A change to what a client sees (status, body, `error.type`, `error.code`,
message, `error.upstream`, frame, Retry-After) on these endpoints — our own
commit or an upstream sync — must update, in the same commit:

1. the code;
2. `ceContract` in `backend/internal/handler/xsci_client_error_contract_test.go`
   (the row and its drives);
3. §2 and §3 of this document (same ids, same order, same cells).

`go test -tags unit ./internal/handler/ -run 'TestXSciClientErrorContract|TestXSciAvailability'`
runs the checks: `TestXSciClientErrorContract` drives every row (and checks the
§1 rules on each: (a) rows carry no code, (b) rows always do and use the
standard type of their status), `TestXSciClientErrorContractMatchesDoc`
compares this document with `ceContract`, `TestXSciAvailabilityAgreesWithContract`
checks the availability probe. A sub2api sync (upstream rebase) is not done
until they pass. Tell the consumers (P175 / oma-core) about any row that
changed.

## 9. How the test drives the rows

The harness (`xsci_client_error_contract_harness_test.go`) builds the `/v1`
middleware chain of `routes/gateway.go` in the same order (contract mark,
request logger, body limit, client request id, ops error logger, endpoint,
API-key auth, group model allowlist, group assignment; the composite-group
rewrite is left out, a no-op for anthropic groups), a real `GatewayHandler` and
`GatewayService`, a scripted upstream, and in-memory repositories (account
state writes — rate limits, model cooldowns — land on the in-memory accounts,
as the database write does on prod), and sends one HTTP request per drive.
`E-UP-MODEL-NOT-FOUND` also sends the follow-up requests of the same client to
show the cooled model answers the same without reaching the upstream.

Driven at the closest function instead, because the real path needs state a
unit test cannot set cheaply:

- `E-LIMIT-USER-CONCURRENCY`: the 30 s user-slot wait is a constant; the drive
  calls the same wait with 60 ms and the handler's error writer.
- `E-BILLING-UNAVAILABLE`, `E-BILLING-KEY-WINDOW`, `E-BILLING-RPM`,
  `E-BILLING-PLATFORM-QUOTA` and the after-the-wait drives of
  `E-BILLING-SUBSCRIPTION-*`: the billing error is fed to `billingErrorDetails`
  and the handler's writer (the same two calls the handler makes).
  `E-BILLING-NO-BALANCE` after the wait is driven end to end, plain and after
  a ping.
- `E-AUTH-TOO-MANY-INVALID`: invalid keys are sent until the limiter blocks,
  with the threshold lowered to 10.
