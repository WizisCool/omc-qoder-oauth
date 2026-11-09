# Usage

## Requirements

- CLIProxyAPI v8 with dynamic plugin support and the quota provider.
- A CPA build that accepts the plugin's declared ABI and RPC schema: the plugin
  registers ABI 1 and schema 10 from its pinned SDK, and an older host refuses it at
  registration with `plugin schema version 10 is not supported`.
- **CPA v8.0.49 or later** to load the plugin (schema 10 first shipped there), and
  **CPA v8.0.51 or later** for the separate Qoder and Qoder CN sign-in cards:
  per-entry name and logo first shipped in v8.0.51. On an older host both entries
  still work; a panel that predates the field shows one card per *identifier*
  decorated with the plugin's own name and logo.
- **CPA v8.0.62 or later** for credential-wide cooldowns: an account-level `429`
  cools every model of the credential. An older host ignores the scope and cools
  only the reporting model.
- Go 1.27.1 for local development. Release builds are produced by CI.

Sign-in is not covered by an offline test. The unit tests drive the OAuth calls
through a fake sender, so the requests and responses the plugin builds are checked,
but a live device flow needs real Qoder and Qoder CN accounts. Verify a login
against your own accounts before relying on a build, and report what you observe.

## Install from the plugin store

```yaml
plugins:
  enabled: true
  dir: /path/to/plugins
  store-sources:
    - https://raw.githubusercontent.com/omc-community/omc-plugins/main/official.json
  configs:
    omc-qoder-oauth:
      enabled: true
      max_concurrency: 3
      cosy_version: 1.1.69
      max_tokens: 32000
      # qoder (default) or qoder-cn. Only a fallback: a login request that names
      # an entry picks that entry's portal.
      site: qoder
```

Use CPA's plugin store to install `omc-qoder-oauth`, or call:

```text
POST /v0/management/plugin-store/omc-qoder-oauth/install
POST /v8/management/plugins/store/omc-qoder-oauth/install
```

Add `?version=<version>` to install a specific release. The store requires the
release to carry `omc-qoder-oauth_<version>_<os>_<arch>.zip` and `checksums.txt`,
and verifies the archive against the checksum before unpacking it.

The archive contains one library file and is installed under
`<plugins-dir>/<os>/<arch>/` as `omc-qoder-oauth-v<version>.so`, `.dylib` or
`.dll`, depending on the target platform: the installed version is part of the
file name. CPA also loads a hand-placed unversioned `omc-qoder-oauth.so`; that file
carries no version, so a pinned version in `plugins.configs` does not match it.
Restart CPA if it reports that the loaded plugin must be replaced.

Manual layout, for the version you install:

```text
<plugins-dir>/linux/amd64/omc-qoder-oauth-v<version>.so
<plugins-dir>/linux/arm64/omc-qoder-oauth-v<version>.so
<plugins-dir>/darwin/amd64/omc-qoder-oauth-v<version>.dylib
<plugins-dir>/darwin/arm64/omc-qoder-oauth-v<version>.dylib
<plugins-dir>/windows/amd64/omc-qoder-oauth-v<version>.dll
<plugins-dir>/windows/arm64/omc-qoder-oauth-v<version>.dll
```

## Description declaration

A release may publish an optional `<plugin_id>.omc.json` (`schema_version` 1) with
localized descriptions. When it does, the file is `omc-qoder-oauth.omc.json` and the
plugin serves the same bytes at:

```text
GET /v0/resource/plugins/omc-qoder-oauth/omc.json
```

The parts that matter here:

- `plugin_id` is `omc-qoder-oauth`, and `plugin_version` is the release version
  without a leading `v`. A document naming another id or version is ignored, not
  guessed at.
- `i18n.description` may contain any subset of `zh`, `en`, `zh-Hant` and `ms`; each
  value is plain text.
- It declares no menu, settings label or action, and it never changes the plugin
  name, logo or the description CPA's store already shows.
- CPA serves plugin resources without management authentication, and answers 404 for
  them in Home mode. When the resource is unavailable, a reader uses the release
  asset; when neither exists, the description in the official plugin source is the
  result.

The served body is never re-serialized: the plugin embeds the same file that
packaging copies into the release, so served and published bytes are comparable by
hash. Bump the file together with the plugin version.

## Build locally

```sh
cd go
go mod download
GOPROXY=off GOSUMDB=off go test ./...
GOPROXY=off GOSUMDB=off go vet ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o ../build/omc-qoder-oauth.so .
```

Name the output with your platform's suffix: `.so` on Linux, `.dylib` on macOS,
`.dll` on Windows. `build/` is ignored by git. Official release artifacts for
Linux, macOS and Windows on both amd64 and arm64 are built by the release workflow
when a `v*` tag is pushed.

## Sign in

The plugin registers **two OAuth login entries**, one per portal:

| Entry identifier | Portal | Sign-in page | OpenAPI host |
| --- | --- | --- | --- |
| `qoder` | Qoder | `https://qoder.com/device/selectAccounts` | `openapi.qoder.sh` |
| `qoder-cn` | Qoder CN | `https://qoder.cn/device/selectAccounts` | `openapi.qoder.com.cn` |

Both produce the same credential shape (`"type": "qoder"`) and use the same
executor. On a panel that supports it, each entry renders as its own card with its
own name and logo (CPA v8.0.51+); the identifier is what CPA routes the login, poll
and callback by.

Start a login through CPA's shared OAuth endpoint, which is the one plugin providers
use:

```text
GET /v8/management/oauth/auth-url?provider=qoder
GET /v8/management/oauth/auth-url?provider=qoder-cn
```

The caller must supply the management key in the `Authorization: Bearer ...`
header. Do not put the key in a URL. Use the URL returned by CPA to complete the
sign-in in the browser. A request without that header cannot initiate a flow.
Public plugin resources serve only the static `logo.png`, `qoder-color.svg`,
`qoder.svg` and `omc.json` assets; other paths answer 404 and a non-GET request
answers 405.

Portal resolution order is: the entry the request names, then a `site` value in the
login request's metadata, then the configured `site`, then Qoder. The entry wins, so
`provider=qoder&site=qoder-cn` signs in to Qoder. `site` matters only for a login
start that names no entry, for example a host that calls the plugin under its own
identifier; an explicit invalid value there is rejected rather than falling back to
the configuration.

### Flow

1. The plugin derives a PKCE verifier from the install's state key and a fresh
   flow nonce, and returns
   `https://qoder.com/device/selectAccounts?...` (`https://qoder.cn/...` for the
   CN portal) with `challenge`, `challenge_method=S256`, `nonce`, `machine_id` and
   `client_id`. It sends no `redirect_uri`, because Qoder's CLI sends none.
2. The browser page finishes in the tab. The plugin polls
   `https://openapi.qoder.sh/api/v1/deviceToken/poll` (or
   `openapi.qoder.com.cn`) with the nonce and the derived verifier. A `200` with a
   token completes the step, a `403` means the sign-in was declined, and any other
   answer stays pending.
3. The device token is traded for a job token at
   `POST /api/v1/me/jobToken` with `{"clientId": "<portal client id>"}`. A Qoder CN
   account whose exchange is refused with a 4xx keeps its device token as the chat
   token, and the credential records that.
4. The account identity is read from `/api/v1/userinfo`. A failure there is not
   fatal: the token response usually names the user, and the account works without
   a display name.

The opaque state CPA carries is sealed with AES-GCM under
`<auth-dir>/.qoder-oauth-state.key`, holds the portal, the nonce and the machine id,
and expires after 15 minutes, the window Qoder's own client allows. The verifier is
derived from that key and the nonce instead of being stored, so the state stays
within CPA's 128-character limit. Keep the auth directory private and writable.

Poll a pending flow with the state CPA returned:

```text
GET /v8/management/oauth/status?state=<opaque-state>
```

`/v0/management/get-auth-status?state=<opaque-state>` is the v0 alias. Cancel with
`DELETE /v8/management/oauth/session?state=<opaque-state>` (or
`DELETE /v0/management/oauth-session?state=<opaque-state>`). Cancellation is handled
by CPA; the plugin does not implement a separate cancellation endpoint.

### Credential

The saved file is `qoder-<hash>.json` and carries `"type": "qoder"`, the portal as
`site`, the same value as `oauth_provider`, the account identity, the machine id,
and the tokens. Its card label is the account email, else a masked phone number,
else the account name, else the user id.

Refresh tokens are single-use, so two concurrent refreshes for the same credential
share one exchange instead of spending the token twice. The plugin asks CPA to renew
about ten minutes before the job token ends. A refresh refused with `401` or `403`
falls back to the credential's device pair; only when both pairs are refused does the
credential need a new sign-in. An existing `"type": "qoder"` auth file is
recognized and republished when CPA imports it.

## Models and routing

Model IDs are the catalogue's own display names, lowercased and slugged for CPA
routing: `GLM-5.3` is published as `glm-5.3`, `Qwen3.8-Max` as `qwen3.8-max`. A
panel therefore lists models by the name its vendor uses, not by Qoder's internal
catalogue key. The plugin reads the catalogue from the portal's API host
(`api3.qoder.sh`, or `gateway.qoder.com.cn` for Qoder CN) once per credential and
caches it for ten minutes; a lookup that misses refreshes it once. A job token
Qoder answers `105` for is retried once with the credential's device token, for the
catalogue and for chat alike, because the same account's device pair can still read
and chat while the job token has lapsed.

- Qoder's catalogue key (`gmodel`, `dmodel`, ...) stays the routing key sent
  upstream, and it still resolves in a request: a client that names the old key,
  or the display name in any casing, reaches the same catalogue entry. CPA's
  model rules (aliases, excluded models, payload rules) match the published ID
  instead, so a rule written for the old key has to name the new one.
- An entry with no display name of its own takes the name the shipped list carries
  for its key; an unknown entry with none keeps its key as its ID.
- A display name that would collide with another entry's ID or with another
  entry's key falls back to that entry's key, so each published ID names exactly
  one model.
- `auto` and `default` are dropped, because they route inside Qoder instead of
  naming a model, and entries the catalogue disables are skipped.
- A failed read keeps the last good catalogue for that account; with no cached
  catalogue at all, the shipped fallback list is used. Qoder ships 17 entries
  including Ultimate, Fast, Performance, Efficient, the Qwen, Kimi, GLM, DeepSeek
  and MiniMax keys. Qoder CN's fallback is the four tiers its CLI names: Ultimate,
  Performance, Efficient and Lite.
- Context windows, image support and thinking efforts come from the catalogue. A
  model that always thinks is advertised with its levels only, without a `none`
  level.
- A `vendor/model` prefix is trimmed before lookup, so a prefixed ID resolves to the
  same catalogue entry.

Execution is OpenAI chat completions in and out: the plugin receives OpenAI chat
completion requests and answers in OpenAI chunks or in one OpenAI completion. CPA
translates Claude Messages, Responses and Gemini clients to OpenAI for this
executor. Streaming and non-streaming requests are both supported, including tool
calls, reasoning content, image inputs and usage.

Token counting is not supported and answers `501`. Qoder documents no token-count
endpoint, and a guess would misbill clients that trust it.

## Quota

```text
GET  /v8/management/plugins/omc-qoder-oauth/quota
POST /v8/management/plugins/omc-qoder-oauth/quota
{"auth_index":"<auth index>"}
```

`GET` describes the provider: `qoder`, display name `Qoder`, reset not supported.
`POST` fetches one credential's usage from `/sash/api/v2/me/usage` on the portal's
OpenAPI host, with the device token, the one the account pages use.

The response is normalized into a subscription (the plan name from the usage
envelope, or `Enterprise` for an enterprise display mode) and one group of credit
buckets: the plan's own credits, add-on credits, an organisation's shared package,
and any dedicated resource packages. Each bucket reports the remaining fraction and
a `N / M <unit> left` description. The plan credits bucket carries the plan's
expiry as its reset time. A free account with no pool reports zero credits left
rather than no data. Resets happen on the Qoder website; a reset request answers
with that message instead of performing one.

## Error handling

Upstream refusals are classified by Qoder's own codes and status, and the client
sees the mapped status:

| Upstream | Client sees |
| --- | --- |
| Queue notice, code `10605`, or a queue object with `isQueued` / `serviceAvailable` | `503` with `Retry-After` (from `retryAfterSeconds`, 30 by default) |
| Codes `110`, `112` to `119`, `122` | `429` |
| Codes `406`, `416`, `430`, `80411`, `80412`, `100400`, `100401`, `100403` | `400` |
| Code `145` / `MODEL_PAID_CONFIRM_REQUIRED`, QoDuck's daily free allowance used up | `429`, model-scoped |
| Code `105`, or any other `401` / `403` | `401`, sign-in expired, sign in again |
| A message saying `error in upstream response` | `502` |
| Transport failure, or an unavailable upstream stream | `503` |
| Other upstream statuses | The upstream status with Qoder's message and code |

A quota refusal (`429` from the credit-pool codes above, or any `429` whose message
names the quota) is reported as credential-wide: Qoder's plan, add-on and
organisation pools are shared by every model, so CPA cools all of them until the
stated `Retry-After`. A bare `429` with no quota wording, the queue notice and
QoDuck's spent free allowance stay model-scoped.

QoDuck (`qoduck`, catalogue key `lmodel`) is free up to a daily allowance. When it
runs out, Qoder answers a `403` carrying code `145` and asks the account to confirm
continuing at normal model prices from its credits. The plugin cannot accept that
confirmation on the account holder's behalf, so it reports the refusal as a
model-scoped `429` with Qoder's own sentence and leaves the rest of the credential
usable. Confirm in Qoder's own client, or wait for the allowance window to end.

Other behaviour worth knowing:

- A refusal is answered with its mapped status so CPA can decide whether another
  credential should carry the request. The one retry made in place is a lapsed job
  token, which is re-signed once with the account's device token.
- A refusal that arrives inside an `OK` stream frame keeps its real status: streaming
  requests read up to the first answer event before any chunk reaches the client.
- A non-streaming answer with no text, no reasoning and no tool call is reported
  as `502` rather than an empty `200`.
- A stream chunk is emitted as one OpenAI `data:` frame, and the stream ends with
  `data: [DONE]`.
- Panics are caught at the plugin boundary and answered as a machine-coded error, and
  client-facing messages never carry token bytes or internal paths.

## Troubleshooting

- **The provider is absent:** enable the global `plugins` switch and
  `plugins.configs.omc-qoder-oauth.enabled`, then restart CPA if the library was
  replaced.
- **`unknown login entry: use qoder or qoder-cn` or `invalid login site: use
  qoder or qoder-cn`:** the login was started with another provider key or with an
  invalid `site` value. Start it from the `qoder` or `qoder-cn` entry, or set the
  native `site` field to one of those two values.
- **Login stays pending:** complete the browser page for the entry you started. The
  flow expires after 15 minutes; start a new sign-in after that.
- **`Qoder sign-in has expired; sign in again`:** both the job pair and the device
  pair were refused, so the stored credential cannot be renewed. Start a new sign-in.
- **No models appear:** the catalogue read failed and the shipped fallback list took
  over, or the credential is not active. A job token that lapsed does not fall back on
  its own: that read is retried with the device token, and only a failure of both
  leaves the shipped list in place. Check CPA logs for `model_error` and the
  credential status.
- **`unknown or disabled Qoder model`:** the requested name is not in the
  account's catalogue. List the models from the panel and use the ID it shows; the
  internal key (`gmodel`) is still accepted.
- **Quota says the credential has no usage token:** the credential was written
  without a device token. Sign in again.
- **`503` with `Retry-After`:** Qoder queued the request. Honour the delay rather
  than retrying immediately.
- **Login fails with `CPA auth directory unavailable`:** CPA reported no auth
  directory to the plugin. Set one, and keep it writable: it holds the state key and
  pending login state.
