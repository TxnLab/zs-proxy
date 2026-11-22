# ZeroSignal proxy (`zs-proxy`)

OpenAI-compatible front door. Local clients (LM Studio, OpenAI SDKs, agents) point at this proxy as if it were OpenAI. Every request is wrapped in an [`age`](https://age-encryption.org)-encrypted envelope destined for a remote node, and the (possibly streaming) response is decrypted before being returned. Clients can't tell they aren't talking to OpenAI.

**Single-user, localhost process.** The proxy is **not** a multi-tenant server — it is a `localhost` daemon serving exactly one user, the **payer** whose keystore mnemonic signs every `escrow.open`. Its `localhost` *is* that user's machine (the SSRF guard depends on this), and it reaches the operator mesh from that one machine's vantage. So it is the CLI/daemon counterpart of the [zerosignal.ai](https://zerosignal.ai) browser app (`client/`) — two front ends for the same single payer — and it is meant to **route identically** to the client: model/node/relay selection is the shared, golden-vectored `selection` policy, with the same app-side refinement layers (reachability, relay reputation, and **client-observed latency**) measured from the proxy's own — i.e. the payer's own — position. Anything described here as "the proxy decides X" is "this one payer's process decides X", never "a shared server decides X for many users".

System overview, service map, and protocol spec: [`README.md`](https://github.com/TxnLab/zerosignal/blob/main/README.md) and [`SPEC.md`](https://github.com/TxnLab/zerosignal/blob/main/SPEC.md) in [TxnLab/zerosignal](https://github.com/TxnLab/zerosignal), the protocol and SDK repo. Paths written `proto/…` or `../proto/…` below are in that repo, which development checks out beside this one as `../proto` (under a `go.work` that `use`s `./proto/go`); `client/` and `tui/` are the browser app and terminal UI.

This file is the **developer** reference for the module. The **user-facing** docs are
published at
[docs.zerosignal.ai/using-the-proxy/overview](https://docs.zerosignal.ai/using-the-proxy/overview) —
notably the [CLI reference](https://docs.zerosignal.ai/reference/cli-1),
[Configuration](https://docs.zerosignal.ai/using-the-proxy/configuration), and
[Routing preferences](https://docs.zerosignal.ai/reference/routing-preferences), which cover
the same surfaces for end users. A user-visible change here lands the doc page in the same
change; the condensed front page is [`README.md`](./README.md).

## HTTP surface

| Method | Path | Notes |
|---|---|---|
| `GET` | `/healthz` | Liveness probe. |
| `POST` | `/shutdown` | **Internal**, loopback-only + `X-Shutdown-Token` (from `daemon.json`). Triggers a graceful drain — the Windows `proxy stop` path (no SIGTERM). Registered only when the daemon set a token; not a client route. |
| `POST` | `/v1/chat/completions` | Streaming or buffered; routes to `dispatchStreamable`. |
| `POST` | `/v1/completions` | Legacy text-completion shape, **emulated** on top of chat completions (`handleCompletions`): the proxy translates `prompt`→`messages` before sealing, dispatches an ordinary `/v1/chat/completions` to the node, and reshapes the chat response (`message`/`delta.content`→`text`, `object`→`text_completion`) back on the way out. Proxy-only — no node/relay/wire change. Served by chat models, so it carries chat semantics; only a single string prompt is supported (array/token prompts are rejected). |
| `POST` | `/v1/responses` | Streaming or buffered; routes to `dispatchStreamable`. |
| `POST` | `/v1/images/generations` | Image generation; routes to `dispatchStreamable`. |
| `POST` | `/v1/images/edits` | Image edits (multipart upload); routes to `dispatchStreamable`. |
| `GET` | `/v1/models` | Model list (on-chain registry rollup). |
| `GET` | `/v1/models/{id...}` | Single-model lookup (on-chain registry rollup). Trailing wildcard, not `{id}`: model ids carry an embedded slash (`google/gemma-5.2`), which a single-segment wildcard won't match — ServeMux would 404 before the handler runs. |
| `GET` | `/models`, `/models/{id...}` | Host-relative **aliases** for the two rows above. Some clients derive the models URL from the host rather than the configured API root (JetBrains AI Assistant does, while sending chat to `/v1/chat/completions` in the same session) and otherwise got Go's non-JSON `404 page not found`. Discovery is a read-only GET with no wire or payment surface, so answering both spellings removes a class of base-URL misconfiguration. **POST endpoints deliberately get no alias** — there the version prefix is part of the protocol contract. |
| `GET` | `/v1/zs/details` | Operator-discovery view (NFD-resolvable directory); unions built-in tools across operators. |
| `GET` | `/v1/zs/operators` | Per-operator registry view (each operator's own models/capabilities, no cross-operator union). On a confidential node, `tee.attested_model_ids` is the model list the hardware evidence was verified over: `null` when no verified list exists (not verified, stub, or not yet checked), `[]` when the node verified an empty catalog. A node can carry a list and still not be `attested`. |

**The discovery surface advertises only what dispatch can actually route to.** A node is held out of
`/v1/models`, `/v1/models/{id...}` and the `/v1/zs/details` aggregate when it is flagged `staging`
on-chain (unless `zs.allow_staging` is set), speaks an incompatible wire major, or has a signing
account below the routing floor. `/v1/zs/operators` applies the staging rule only — see below.

For staging that means: a node its operator flagged `staging` on-chain contributes
nothing to them, so the catalog doesn't advertise a staging node dispatch would then refuse.
Selection already holds staging nodes out as targets, so without this a model served only by them
was listed and then answered `400 no_production_operator` — and, more quietly, such a node could set
the cross-operator minimum context window and lowest "from $X" price for a model nobody could buy.
A model in that state is now absent from the list and **404s** on `/v1/models/{id}`, matching it.
Staging nodes remain usable as **relays** either way — a relay forwards bytes and runs no inference
— so `AllOperators`, which feeds the relay pool, is deliberately left unfiltered and the filter
lives in the `/v1/zs/operators` handler.

The same rule covers **wire-protocol incompatibility**: a node on a different `wire.ProtoVersion`
major can never be sealed for — `SelectTargets` refuses it and dispatch answers `400
no_compatible_operator` — so it contributes to no aggregate either. Note an **empty** `ProtoVersion`
counts as incompatible (`ProtoVersionCompatible` maps it to `LegacyProtoVersion`), which is safe
only because the field is set by the details probe and every advertisable node has had a successful
one; it drops nodes whose probe predates the field, never one awaiting its first. Unlike staging
there is no opt-in knob, which is why `/v1/zs/operators` deliberately keeps listing these with their
`proto_version` — an operator on the wrong major has to be able to see that somewhere.

**Signer funding** follows the same shape. A node's signing account pays the network fees of every
`open()` group and settle it signs, so `SelectTargets` refuses a node with less than
`selection.MinSignerSpendableMicroAlgos` spendable (`amount − min-balance`), and a model only such
nodes serve answers `503 no_funded_operator`. The balances come from one algod read per distinct
signer on every chain refresh (`proto/go/zs/signer_balance.go`), in shuffled order under a 10s cap
so it can't starve the details probes. A never-read balance is eligible, and a failed read keeps the
last-known value. Every signer is read, never only the likely target, so the reads reveal nothing
about which node a request will use. `/v1/zs/operators` keeps listing an underfunded node with
`signer_spendable_microalgos` and `signer_underfunded: true`, so a payer can see why a model left.
Like staging, it is target-only: the relay pool is unfiltered.

The discovery rows are **lean by default**. The compact weights-integrity signal — each operator's `weights_digest` / `weights_unverifiable` — rides the bare list; the proxy folds it into a cross-operator **consensus `weights_digest` + `weights_mismatch`** flag on the `/v1/zs/details` aggregate (and passes each operator's value through **verbatim** on `/v1/zs/operators`, so consumers see both the rollup and the raw per-node evidence). The verbose per-model taxonomy (HF-discovered family / size / quant / variant) and integrity detail (`weights_unverifiable_reason` / manifest) ride [`SPEC.md`](https://github.com/TxnLab/zerosignal/blob/main/SPEC.md) §3c's `?expand=coordinates,digest&model={id}` **deep path** — a per-node surface the proxy **forwards verbatim** on drill-in rather than aggregating, so its own `/v1/zs/details` rollup stays lean and the detail reflects the specific node. Advisory only: none of it affects operator selection, price, or settlement.

`/v1/embeddings` stays disabled (commented out in `../proto/go/zs/handler/server.go::routes()`) — there's no chat equivalent to emulate it with. Leave it disabled unless explicitly re-enabling.

The client's `Authorization` header is **always** discarded. The proxy injects no bearer auth — admission to the operator is via the on-chain sealed envelope, not a static API key.

### Routing preferences (`provider`)

A caller can guide which operator/node the proxy dispatches to with an optional top-level **`provider`** object on the request body (OpenRouter-style). It is a proxy-only hint: the SDK's `Client.dispatch` parses it (`selection.ExtractRoutingPreferences`) and then **strips it before sealing** (`inject.DropRoutingPreferences`) so it never reaches the node or the upstream LLM. All fields are advisory — an absent/malformed value is ignored.

```jsonc
{
  "model": "qwen3-coder",
  "messages": [/* … */],
  "provider": {
    "only":   ["1234", "5678:2"],   // allowlist of operator refs
    "ignore": ["9999:1"],            // denylist (deny wins)
    "order":  ["5678:2", "1234"],    // explicit preference order
    "allow_fallbacks": false,         // false ⇒ pin to exactly `order`/`only`
    "max_price": { "input": 2.0, "output": 6.0 }, // USD/1M ceilings
    "require_tools": true,            // hard-require advertised built-in tools
    "sort": "latency",               // price | throughput | latency
    "relay": "off"                    // per-request transport-privacy override
  }
}
```

A ref is `"<operatorId>"` (any node of that operator) or `"<operatorId>:<nodeId>"` (one exact node). Pin precedence: a `previous_response_id` continuation pin still wins (the session lives in that operator's account) — if the caller's preferences exclude that operator it's a conflict, not a silent re-route. Unsatisfiable preferences surface distinctly:

| Outcome | Status / code |
|---|---|
| `only`/`order`+`allow_fallbacks:false` matched no available operator | `400 no_pinned_operator` |
| `previous_response_id` continuation excluded by `only`/`ignore`/`order` | `400 pin_conflicts_with_continuation` |
| `max_price` excluded every operator | `400 price_ceiling_exceeded` |
| `require_tools` and no operator advertises the requested tools | `503 no_tool_capacity` |
| Every candidate unreachable through its gateway (a code-less 502/503/504/52x), retry budget spent | `503 operators_unreachable` |

**`sort`** re-weights the proxy's app-side responsiveness ordering of candidates (which by default bands them by combined expected response time = network RTT + on-chain TTFT + decode, price within band): `"latency"` orders by first-token time only (network RTT + TTFT, decode ignored); `"throughput"` orders by decode tok/s only (TTFT ignored); `"price"` skips the reband for a pure cheapest-first order; absent keeps the combined default. The affinity/continuation pin still leads in every mode. These are per-process performance signals (not the golden-vectored policy), so they don't affect which operators are *eligible*, only their order.

**`relay`** overrides transport privacy (the relay hop) for one request: `"off"` sends direct to the target (lower latency, but the target sees the proxy's IP — single-hop unlinkability is forfeited for that request); `"required"` forces a relay or fails `503 no_relay_available`; `"auto"`/absent keeps the config default (`zs.privacy`). Also settable via the `X-Zs-Relay` header (body wins). The `"off"` direction is gated by `zs.allow_privacy_override` (default `true`; set `false` to refuse caller opt-outs). It governs the single hop that carries **both** the sealed reserve and the inference forward, so `off` is consistent end-to-end.

The `only`/`ignore`/`order`/`allow_fallbacks`/`max_price`/`require_tools` fields are the shared, golden-vectored `proto/go/selection` policy (so `client/` routes identically); `provider.sort` and `provider.relay` are reserved for the app-side responsiveness/relay passes. See `proto/SPEC.md` § "Path selection". Not yet offered on `/v1/images/edits` (multipart body).

## Architecture

```
client → dispatch (tokenize → reserve → open+seal) → operator
client ← decrypt + audit ← (buffered body OR per-SSE-frame pump) ← operator
```

`../proto/go/zs/handler/transform/` defines the transformer interfaces invoked inside the dispatch loop on the plaintext body before sealing; `transform.Passthrough{}` is the no-op fallback. The handler's `Server.dispatch` (`../proto/go/zs/handler/emit.go`) hands every streamable endpoint to the SDK's `Client.dispatch` (`../proto/go/zs/dispatch.go`), which runs the two-phase admission flow:

1. **Tokenize** the request body (`../proto/go/zs/tokenize/`) to a safe upper bound on `input_tokens` plus a concrete `max_output_tokens`. Missing both → `400 max_output_required`. A caller ceiling larger than anything the pool advertises is **lowered** to the largest `max_output_tokens` declared by the operators the request could be routed to (`s.poolMaxOutput` → `inject.ClampMaxOutput`, pre-seal, so the escrow and the body the node forwards upstream carry the same number) — `max_tokens` is an upper bound on generation, not a demand, so a tool whose model card is more generous than the pool still routes instead of failing. The clamp only ever lowers, never adds an omitted field, and is skipped entirely when any serving operator declares no ceiling (undeclared = unbounded, so it can honor the original value). `routableIgnoringSize` narrows the pool to the size-independent hard excludes first — staging, banned nodes, wire generation, TEE, a signing account below the 5 ALGO spendable floor, `provider` only/ignore — because that skip rule would otherwise let a single unroutable operator disable the clamp for everyone. A clamped request answers with `X-Zs-Max-Output-Clamped`.
2. **Select operators** from the on-chain registry, filtered by sizing fit (`selection.FitsContextWindowFor`: the input bound plus `max_output` within the window, or for a node at wire 9.11 or later, which lets its backend decide fit, an input bound up to 3× its input ceiling). `tool_call_id`s in the request body are checked against the affinity cache (`proto/go/zs/affinity.go`) so multi-turn tool calls return to the same operator.
3. **Reserve** against each candidate (`POST /v1/zs/reserve`). 429 → next candidate (or fail under `affinity_policy: strict`). On success, verify the ticket signature; the reserve response also carries the operator's **pre-signed 2-tx `escrow.open` group** (`gtxn[1]` = the `open()` AppCall signed by the operator, `gtxn[0]` = a USDC-payment template for the payer to sign).
4. **Open + seal**: the wallet signs the payer's USDC-payment leg of that operator-pre-signed open group. Against a node advertising wire 9.12 or later (`wire.SubmitsOpen`), `Wallet.OpenForNode` completes the group without submitting it (`CompletePresignedOpenGroup`) and the group rides inside the sealed request as `open_group`, for the node to submit; against an older node, `Wallet.Open` submits it itself (`SubmitPresignedOpenGroup`). The per-ticket box MBR is **drawn from the payer's prepaid MBR pool** (established lazily on first serve via `EnsureDeposit`) — there is no per-request box-funding leg, and in the consensus-verified design the operator's AppCall over-fees the whole group, so the payer's own ALGO outlay at open is ~0. The resulting `open()` app-call txid (group member 1) is AAD-bound into the envelope alongside the ticket id. `wire.WrapRequestBound` produces the `age`-encrypted body, carrying `open_group` when the group is handed over.
5. **Forward** to the operator's own `BaseURL` (`forwardRawNonStream` / `forwardRawStream`, which require a per-operator base URL — the proxy has no fallback target).
6. **Decrypt + audit**: verify `commit_k == sha256(response_key)`; record any new `tool_call_id`s in the affinity cache.
7. **Receipt + settle**: validate the operator's signed `UsageReceipt`; ack-settle via `OpenHandle.AckSettle`, which signs the payer half of the node's settle group and, for a 9.12+ node (`wire.TakesSettleAck`), sends the group back on a sealed `POST /v1/zs/settle-ack` (through a 9.12+ relay when the request was relayed) for the node to submit. An older node, or no relay able to carry the ack, gets the group broadcast by the proxy; a failed ack is never followed by a broadcast of the group. On `body_hash` mismatch, drive `OpenHandle.Protest` with an evidence excerpt.

### Streaming

`../proto/go/zs/sse.go::pumpSSE` uses `bufio.Reader.ReadBytes('\n')` (not `bufio.Scanner` — its 64KB cap breaks on large tool-call frames). Every frame goes through `ResponseTransformer.TransformStreamChunk`; an empty returned frame is dropped. `http.Flusher.Flush()` runs after each frame; `X-Accel-Buffering: no` is set for nginx-friendliness. Mid-stream failures emit an OpenAI-shaped `data: {"error": ...}` then `data: [DONE]`.

`Server.WriteTimeout` and `http.Client.Timeout` are both `0` (disabled) — required for long SSE streams. Per-request cancellation comes from the inbound request context.

### Client-facing cost surface

Each admitted response carries a per-request cost breakdown for the proxy's own client (distinct from the node↔proxy wire in `proto/SPEC.md` § 5). **Non-streaming**: `X-Zs-*` response headers — `X-Zs-Inference-Amount` (operator base take), `X-Zs-Max-Price-Amount`, `X-Zs-Fees-Microalgos` (payer-paid ALGO; the settle is submitted after the response is written, so this omits the settle fee the payer pays when the node sent no usable atomic settle group, which is logged instead), and — when the proxy could read the ticket's open-time fee snapshot — `X-Zs-{Fee-Bps,Discount-Bps,Platform-Fee-Microusdc,Refund-Microusdc,Total-Debit-Microusdc}`. **Streaming**: the proxy swallows the operator-signed `zs-receipt` frame and emits an unsigned `event: zs.usage` SSE frame carrying the same data as JSON, including a nested `fee` object (`{fee_bps, discount_bps, platform_fee_microusdc, refund_microusdc, total_debit_microusdc, total_debit_usd}`) when the snapshot was readable. A receipt that fails verification gets neither: its charge is disputed rather than spent, and the proxy logs the violation.

> **The streaming `zs.usage` frame is opt-in.** Its `data:` payload is not a valid OpenAI chat chunk, and strict OpenAI clients that parse every `data:` line rather than dispatching on the SSE `event:` type (e.g. JetBrains/Koog — `kotlinx.serialization` throws `missing fields [choices, created, id, model, object]`) error on it. So the proxy stays a clean drop-in OpenAI endpoint by **default (frame suppressed)** and only emits the frame when the request carries `X-Zs-Usage-Frame: <any non-empty value>`. The TUI (`tui/`) sets it. Settlement is unaffected by the header — it runs when the proxy reads the receipt, before the frame is built.

Under the additive fee model `amount_charged` (inference) is the operator's base take; `platform_fee` rides on top and is the payer's; `total_debit = inference + platform_fee` is the payer's real USDC cost. The fee fields are **derived** (`escrow.ComputeFeeBreakdown`) from the operator-signed `amount_charged` and the effective rate `feeBps` (there is no discount mechanism, so `discountBps` is always 0). The rate is not per-payer; it is simply the contract's live `protocolFeeBps` global, so the proxy reads it via a direct global read (`escrow.Client.ProtocolFeeBpsNow`) and caches it (background-refreshed, ~60s TTL) — the response path never blocks on a chain call. It equals the per-ticket open-time snapshot the contract charges, and the on-chain snapshot remains the settlement authority. The whole `fee` object / the fee headers are omitted until the rate cache warms (distinguishing "unavailable" from a genuine 0 platform fee). These figures are advisory (unsigned); the signed authority is the `UsageReceipt` and on-chain settlement.

### Module layout

```
main.go                  4-line shim → internal/cli.Execute()
internal/cli/            cobra CLI: proxy start/stop/status/restart/logs + install-service/uninstall-service,
                         connect, wallet, fund, slots, withdraw, close-deposit, tickets, status, doctor, config, version
internal/connect/        `zs-proxy connect` integrations: opencode, openclaw, pi, hermes, aider, continue, codex, generic
internal/cli/service/    OS-service install: launchd (macOS), systemd user unit (Linux), Windows SCM
internal/app/            importable Run(ctx,cfg,opts): maps config onto zs.New, serves its handler, graceful shutdown (incl. the Windows /shutdown route)
internal/daemon/         self-managed daemon: pidfile/record, detached spawn, stop (SIGTERM unix / control channel windows)
internal/wallet/         the CLI's backend choices over the SDK's go/wallet (shared with other zerosignal programs)
internal/account/        algod-backed balance reads + USDC opt-in
internal/spend/          aggregate USDC spend guard (per-request + daily caps)
internal/paths/ build/   per-OS state/log/config dirs; ldflags-embedded network presets + version
internal/config/         YAML + env overrides + validation
internal/update/         best-effort "newer release available" notice (24h on-disk cache)
config.example.yaml      example config
```

Protocol crypto lives in `github.com/TxnLab/zerosignal/go`. The payer core (registry, discovery, reserve, wallet/escrow handle, refund watchdog, relay/latency/backoff/affinity stores) is `github.com/TxnLab/zerosignal/go/zs` (source `../proto/go/zs`), along with the dispatch loop, forwarder and SSE pump. The OpenAI-compatible HTTP surface (routes, handlers, error helpers) is its `handler` subpackage, imported as `server`; the input-token bound and the request transforms are its `tokenize` and `handler/transform` subpackages. The TEE evidence verifier and the Intel collateral cache are its `tee` subpackage.

## Configuration

`./config.yaml` by default; override with `--config` or `PROXY_CONFIG`. Env vars override YAML, named after YAML nesting with `_` separators (`PROXY_SERVER_LISTEN`, `PROXY_ZS_ESCROW_APP_ID`, etc. — see `internal/config/config.go::applyEnv` for the full list).

On Algorand, a resolved top-level `algod:` block and `zs.escrow_app_id` are required. `zs.proxy_payer_addr` is auto-derived when exactly one signing mnemonic is loaded and only needs to be set explicitly when two or more mnemonics are loaded (to disambiguate which one signs). The resolved address must be present in the loaded keystore either way. Operators are discovered on-chain at startup (filtered by `zs.operator_ids` if set, otherwise all registered). See `config.example.yaml` for the full annotated schema.

**Solana.** A `solana:` block with `cluster` (`devnet` | `localnet`) or `rpc_endpoint` pays on Solana instead (env `PROXY_SOLANA_CLUSTER`, `PROXY_SOLANA_RPC_ENDPOINT`, `PROXY_SOLANA_PROGRAM`; `program` is the base58 `zs_escrow` id, empty for the cluster's). The Algorand fields must then be empty: a config naming both rails is refused. `--network solana-devnet` / `solana-localnet` select the cluster for one run; on devnet they also reset `program` and `rpc_endpoint` to the preset's, as `--network testnet` resets the app id. The payer key is the wallet's 24-word phrase at `m/44'/501'/0'/0'` (the address Phantom shows; `zs-proxy wallet address` prints it on a Solana network), and a 25-word Algorand mnemonic, which has no Solana key, is refused. The state files take the Solana deployment's key (`ticket-journal-solana-devnet-<program>.json`, and so on).

The wallet commands work the same on Solana, in SOL where Algorand uses ALGO. `fund` airdrops SOL on devnet or a local validator (elsewhere it prints the address), creates the USDC token account, and fills the **ticket rent pool**, Solana's MBR pool: each open draws a ticket's rent from it and the ticket's ending returns it. Devnet pays in Circle's devnet USDC, from [faucet.circle.com](https://faucet.circle.com) (Solana Devnet). On a local validator, `fund --mint-authority <keypair.json>` mints 1000 test USDC with the mint's `solana-keygen` keypair. `slots` sizes the rent pool, `withdraw <amount-sol>` reclaims its free SOL, `close-deposit` closes it, and `tickets` lists, shows and recovers this wallet's ticket accounts.

**Prepaid MBR pool.** Each ticket's escrow box needs a min-balance (MBR) reserve. Rather than fund it per request, the payer keeps a **prepaid pool** in the escrow app (opted in once) sized to a working set of concurrent ticket boxes — `zs.concurrent_slots` (default 10; the box MBR is recoverable). The pool is established lazily on the first served request (`EnsureDeposit`) and proactively by `zs-proxy fund`; `zs-proxy withdraw <amount-algo>` reclaims the unreserved balance and `zs-proxy close-deposit` closes it entirely (refunding the balance + opt-in MBR). An underfunded pool surfaces a `402 wallet_unfunded` at open, the same as the USDC leg. `zs-proxy status` reports slot utilization (`N of M ticket slot(s) in use`) against the slots the deposit actually backs — `algoBalance / mbrForTicket()`, which is what `open()` admits on, not the configured working set — plus the payer's remaining free-ticket allowance when the deployed contract has that quota.

`zs-proxy slots` is the read/write face of that number: bare it prints configured target vs deposit-backed vs in use (and any shortfall or reclaimable excess); `zs-proxy slots <n>` writes `zs.concurrent_slots` to the config file **and** moves the pool to match, depositing to raise or reclaiming the unreserved excess to lower. It is the only command in this module that writes the config file, and it does so as a single-line splice (`internal/config.SetScalar`) so the surrounding comments survive. Note the two halves are separately load-bearing: the contract admits on the deposit, so a config change alone changes nothing, while a pool resize alone gets re-aimed by the next top-up.

**`zs.mbr_deposit_slots` was renamed to `zs.concurrent_slots`** (env `PROXY_ZS_MBR_DEPOSIT_SLOTS` → `PROXY_ZS_CONCURRENT_SLOTS`). The old key is no longer read at all — a config still carrying it silently falls back to the default 10, so anyone who had raised it must re-set it (`zs-proxy slots <n>`).

`zs.require_fs` (default false; `PROXY_ZS_REQUIRE_FS` env or per-request `X-Zs-Require-FS` header) is the fail-closed forward-secrecy posture — the strict analog of the TEE knobs: when on, the proxy seals only to a verified, unexpired anchor-signed ephemeral recipient (no on-chain age-key fallback, no retry-to-anchor) and skips operators lacking one, returning 503 `no_ephemeral_capacity` when none qualifies.

Signing mnemonics load from env or cloud secrets via `proto/go/keystore` — see [`go/keystore/README.md`](https://github.com/TxnLab/zerosignal/blob/main/go/keystore/README.md).

## Command-line interface (`zs-proxy`)

The proxy ships as the **`zs-proxy`** CLI (the `zerosignal` brand is retained for env
vars, the OS-keychain service name, and the data dir). A config-less first run boots on
**mainnet** (`build.DefaultNetwork`) — embedded defaults supply the escrow app id + algod
endpoint, so no `config.yaml` is required, and `--config` / `PROXY_*` still override for
self-hosters. Note what the zero-config path implies: it opens **real escrow against the
production contract**. A first run with no wallet in the keychain and no env mnemonic
prompts for one (`bootstrapWallet` — browser login, paste-import, or generate); it never
silently generates. With no TTY to prompt on (a service, a script, piped stdin) it exits
naming the setup commands instead, and `install-service` runs the same check up front so a
walletless service can't restart-loop on it. Only the keychain wallet satisfies that check: a
unit carries none of the installing shell's environment, so an env mnemonic there never
reaches the service (`--print` renders the unit for adding one by hand).

```bash
# lifecycle — `start` daemonizes by default and returns to the shell with :9376 live
zs-proxy proxy start [--port 9376]                       # background daemon
zs-proxy proxy start --allow-staging                     # route to AND advertise nodes flagged on-chain as staging (default: skip)
zs-proxy proxy start --foreground                        # serve in this process (supervisors / debug)
zs-proxy proxy stop                                      # graceful drain of in-flight streams
zs-proxy proxy status                                    # pid, uptime, listen, network, /healthz probe
zs-proxy proxy restart                                   # shares start flags (--config/--port)
zs-proxy proxy logs -f                                   # tail the daemon log (metadata only — never prompts/bodies)
zs-proxy proxy install-service | uninstall-service        # run at login/boot via the OS service manager (see below)

# wallet + funding
zs-proxy wallet show | address | new | import | login | export | opt-in
zs-proxy fund [--wait]                                   # deposit addr + QR + on-ramp links; once funded, opts into USDC + funds the prepaid MBR pool
zs-proxy slots                                           # how many tickets can be open at once: configured target vs what the deposit backs vs in use
zs-proxy slots <n> [--dry-run] [-y]                      # set it — writes zs.concurrent_slots AND resizes the pool (deposit to raise, reclaim to lower)
zs-proxy withdraw <amount-algo>                          # reclaim unreserved ALGO from the prepaid ticket-MBR pool
zs-proxy close-deposit                                   # close the MBR pool, refund the full balance, opt out of escrow
zs-proxy status                                          # one-shot wallet / funding / operator health
zs-proxy doctor                                          # diagnose config → wallet → chain → funding → ticket pool → nodes → port

# stuck escrow tickets (read from chain, so it also sees tickets opened before a restart)
zs-proxy tickets                                         # every live ticket box this wallet has USDC locked in, + what resolves each
zs-proxy tickets get <ticket-id-b64>                     # one ticket in full (the id is in the proxy log)
zs-proxy tickets recover [<id> | --all] [--dry-run]      # post the deadline call that frees a stuck ticket

# connect AI tools to the running proxy
zs-proxy connect                                        # detect installed tools → confirm → configure
zs-proxy connect opencode [--model qwen3-coder]          # configure one tool
zs-proxy connect --all                                   # configure every detected tool
zs-proxy connect --list                                  # show detection + config paths (no writes)
zs-proxy connect opencode --print                        # render the merged config, write nothing

# inspection
zs-proxy models [--tag code] [--json]                    # the running proxy's catalog: context, payer-net price, features, operator tags
zs-proxy config path | print-effective                  # resolved config path / merged effective YAML
zs-proxy version                                         # product + proto + embedded escrow app ids

# confidential mode (TEE): what a node's measured compose says, and what to allowlist
zs-proxy tee inspect <node-base-url>                     # replay its event log, gate its published app-compose, print the findings
zs-proxy tee inspect <node-base-url> --chain             # ...and verify the quote against Intel's PCK chain (network)
zs-proxy tee inspect --file docker-compose.yaml          # offline: compute the release values from a published compose
zs-proxy tee inspect <node-base-url> --json              # machine-readable, for generating a release list
```

`tee inspect` prints the two values `zs.tee.trusted_compose_skeletons` and `zs.tee.trusted_node_images`
hold; nothing else computes a compose skeleton digest. It reports rather than gates — the exit code is
0 for any finding, and only a usage error, an unreadable config, or an unreachable node exits non-zero.
**Pass `--chain` before pasting anything into an allowlist**: without it the quote's signature is never
examined, so the replay is arithmetic over bytes the node chose, and the report says `verified: false`.
Against a node it also prints the bundle's published model list (`app_models`) and whether the quote's
model binding is populated. It does not check that binding, which needs the node's on-chain id; the
list is printed so an operator chasing a model-binding refusal can see what a payer received.

**Update notice.** Before any command, `zs-proxy` prints a one-line "newer
version available" hint to **stderr** when a newer release exists on
[`txnlab/zs-proxy`](https://github.com/txnlab/zs-proxy), tailored to how it was
installed (`brew upgrade zs-proxy` / `scoop update zs-proxy` / re-running the
install script / the releases page). The script channel is identified by the
marker `scripts/install.sh` leaves in the state dir, which outranks the
path-shape guess used for the other two. It is best-effort and unobtrusive: the network check runs **at most once
a day** (cached under the state dir), only when stderr is an interactive
terminal (so it never touches piped/scripted output, the background daemon, or
service runs), and is skipped for source/`dev` builds. Disable it with
`PROXY_ZS_CHECK_UPDATES=false`.

### Connecting AI tools (`connect`)

The proxy is a drop-in OpenAI endpoint, so any OpenAI-compatible tool can use it — `connect`
writes the tool's own config for you. With **no argument** it detects which supported tools are
installed and configures them interactively; name a tool to do just one, or `--all` for every
detected one. The proxy must be **running** (so the live endpoint + model list can be read); if it
isn't, `connect` tells you to `zs-proxy proxy start` first.

Supported tools: **opencode**, **openclaw**, **pi**, **hermes**, **aider**, **continue**, **codex**,
plus **generic** (prints the `OPENAI_BASE_URL` / `OPENAI_API_KEY` env vars for anything else). For
each, it merges a provider named `zerosignal` into the existing config — **preserving** your other
settings (and comments, for JSONC like opencode's `opencode.jsonc`), backing the file up to
`<file>.bak`, and writing atomically. Re-running is idempotent. No API key is needed (the proxy
admits via the on-chain seal), so a placeholder key is written where a tool requires a non-empty one.
`--print` renders the result without touching disk; `--config-path` overrides a single tool's target
file. Most tools take one file; **pi** takes two (`~/.pi/agent/models.json` for the provider +
catalog, `settings.json` for the default model), **aider** takes two (`~/.aider.conf.yml` plus a
LiteLLM model-metadata sidecar), and **hermes** takes two (`~/.hermes/config.yaml` for the provider
block, plus `ZEROSIGNAL_API_KEY` / `ZEROSIGNAL_BASE_URL` spliced line-by-line into `~/.hermes/.env`
with every other line left as it was) — `connect` reports each file it touched. Hermes stops reading
a custom provider block once the standalone
[ZeroSignal Hermes plugin](https://github.com/TxnLab/hermes-zerosignal-provider) is installed and
takes those two variables instead, so one `connect hermes` works with or without the plugin;
`hermes model` shows which is active. Hermes' `.env` gets no `.bak` (it is a secrets file and the
splice changes only our two lines), `--config-path` puts `.env` beside the overridden config, and
`HERMES_HOME` is honored for both. `connect hermes --plugin` also installs that plugin, by running
`hermes plugins install zerosignal --enable` once both files are written, so a failed install still
leaves a Hermes that works through the provider block. The child gets the terminal (it clones and
may pip-install) and an environment scrubbed of the wallet's mnemonics and keystore passphrase —
this is the only place `zs-proxy` execs third-party code. It is opt-in and hermes-only (refused with
`--all`, `--list`, `--print`, and `--config-path`), and an already-installed plugin is skipped,
since Hermes answers a second install with "already exists".

Each model in the written catalog carries what the proxy advertises for it on `/v1/zs/details`:
context/output limits, payer-net USD/1M rates (already grossed up by the protocol fee), input and
output modalities, and the reasoning / tool-calling capability flags — as far as each tool's schema
goes. That matters because tools default the unset flags *off*: an opencode model with no
`modalities` + `attachment` block is treated as text-only (a vision model silently loses image
attachment), openclaw/pi hide the thinking display without `reasoning`, Continue won't offer Agent
mode without the `tool_use` capability, and aider warns "Unknown context window size and costs" for
every model missing from LiteLLM's catalog. Where a tool has no per-model surface at all (hermes,
codex, generic) only the provider/endpoint is written.

- **Daemon vs foreground.** `start` re-execs itself detached (pidfile + `daemon.json` under
  the state dir) and returns to the prompt; `--foreground` is the non-forking serving path
  used by supervisors, the OS service, and debugging. The interactive first-run setup (wallet
  bootstrap, keychain unlock) happens on the parent's TTY before backgrounding.
- **Graceful stop everywhere.** `stop` drains in-flight SSE streams — SIGTERM on unix; on
  Windows a loopback-only, token-guarded `POST /shutdown` (no SIGTERM) reaching the same
  shutdown path — never a hard kill.
- **Wallet.** Created in the OS keychain (macOS Keychain / Windows Credential Manager / Linux
  Secret Service) by signed releases, in an encrypted file by a build from source;
  `wallet.json` records which, so any later build opens it there. `wallet import` takes the
  24-word phrase from the zerosignal web app (Settings → Recovery), or `wallet login` signs in
  to the web client in your browser to pull it without a paste — either way the CLI and the
  [zerosignal.ai](https://zerosignal.ai) browser app share one funded account.
- **Foreground entrypoint.** Bare `zs-proxy` / `zs-proxy --config config.yaml` starts the
  proxy in the foreground.

## Run / build / test

```bash
make zs-proxy                  # dev build → out/zs-proxy   (or: go build -o zs-proxy .)
go run . --config config.yaml  # run from source against an explicit config
go test ./...
(cd ../proto/go && go test ./zs/handler -run TestHandleListModels_EmptyRegistry_ReturnsEmptyList)   # single test
go test -race ./...
make lint-sh                   # shellcheck scripts/install.sh (also a CI step)
```

Release builds (signed/notarized, Homebrew cask, Scoop) go through `goreleaser` — see
[`RELEASE.md`](./RELEASE.md). A signed release creates the wallet on the OS keychain; an
unsigned `make`/`go build` creates it on the encrypted file. Either opens an existing wallet on
the backend its `wallet.json` records (see `internal/wallet`).

`scripts/install.sh` is the public one-line installer — `curl -fsSL
https://zerosignal.ai/install.sh | sh`, or `| sh -s -- zs-node` for the node. It rides along
as a release asset (`release.extra_files`), so the copy in this tree is the only one that
exists. `scripts/install_test.go` runs it against a fake release under every shell on the
box; `make lint-sh` and CI shellcheck it.

## A second proxy on one machine

`ZEROSIGNAL_HOME=<dir>` moves the wallet, config, journals, spend tally, pidfile and logs into
`<dir>` (logs under `<dir>/logs`), and its wallet gets its own keychain item, so a local test
payer runs beside your own without touching it:

```bash
export ZEROSIGNAL_HOME=~/zs-local                                  # unquoted, so the shell expands ~
zs-proxy wallet new --network solana-localnet
zs-proxy proxy start --network solana-localnet --port 9377
```

On the localnet preset at a loopback endpoint no privacy or private-host setting is needed: the
SDK sends direct and reaches the local node by default, after checking the chain is not a public
one. The value is used as written, so a quoted `"~/zs-local"` (in a `.env` file or an IDE run
config) is a directory named `~` under the current one. In that home the lifecycle commands drive
only its own daemon, `install-service` / `uninstall-service` refuse, since the OS service runs
the default home's proxy, and `connect` writes no tool's config without `--print` or
`--config-path`, since those configs are yours rather than the home's. Each home keeps its own
spend tally, so the same wallet in two homes has two daily caps.

[`LOCALNET.md`](https://github.com/TxnLab/zerosignal/blob/main/LOCALNET.md) in `TxnLab/zerosignal`
walks through the whole local stack around such a proxy: the chain, registering and running a
node, funding the payer, and a chat, on either rail.

## Running as a service (auto-start, restart on reboot)

`zs-proxy proxy start` backgrounds a self-managed daemon (pidfile + record under the
state dir; manage with `zs-proxy proxy stop|status|restart|logs`). That daemon does
**not** survive a reboot and isn't restarted on a crash. To start at login/boot and
restart on failure, register an OS service:

```bash
zs-proxy proxy install-service                     # register + enable (current config)
zs-proxy proxy install-service --port 9376         # pin a --config/--port
zs-proxy proxy install-service --print             # render the unit / sc.exe command to stdout
zs-proxy proxy uninstall-service                   # stop + remove
```

The binary registers itself — a **launchd LaunchAgent** (macOS), a **systemd user unit**
(Linux), or a **Windows service via the SCM** — running `proxy start --foreground` (the
non-forking entrypoint) with its own resolved path + your flags baked in. Nothing to find
or hand-edit. macOS/Linux are emitted from embedded templates in `internal/cli/service/`;
Windows is registered through `golang.org/x/sys/windows/svc/mgr`.

Once a service is installed, the lifecycle commands above (`zs-proxy proxy
start|stop|restart|status|logs`) **target the service** instead of the self-managed
daemon — they route to the OS service manager under the hood (`launchctl` / `systemctl
--user` / the Windows SCM), so the same commands work whether the proxy runs as a
background daemon or an installed service. `proxy logs` tails the service's log
(`~/Library/Logs/zs-proxy.log` on macOS, the systemd journal on Linux, `proxy.log` on
Windows).

- **macOS / Linux** install **per-user** so the service runs in the login session, where
  the wallet keychain / Secret Service is reachable.
- **Windows (SCM)** runs the service **as the installing user** so it can read that user's
  Credential Manager wallet + per-user data dir (a LocalSystem service would see neither).
  `install-service` prompts for that account's Windows password (hidden — never a flag;
  `--service-account` overrides which account). It needs **administrator rights** — if you
  aren't already elevated it raises a **UAC prompt** and continues in a new window (the
  account is pinned across the elevation, so over-the-shoulder admin approval still installs
  it to run as you). It also **auto-grants the account the "Log on as a service" right**
  (via the LSA policy API), so the usual `Error 1069: logon failure` needs no manual
  `secpol.msc` step. If a logon is still rejected it's a wrong/empty/changed password — re-enter
  it in `services.msc` → zs-proxy → Properties → Log On, or reinstall. The service logs to
  **`proxy.log`** (an SCM process's stderr is discarded), so `zs-proxy proxy logs` works for it too.
- **A wallet must already exist.** A service has no TTY, so it won't prompt to import/create
  one and won't silently generate an unfunded wallet; `install-service` refuses up front
  when there is none. Run `zs-proxy wallet login` (or `wallet import` / `wallet new`), then
  install the service.
- **Wallet unlock.** A wallet in the OS keychain is read unattended — nothing to configure.
  A wallet in the encrypted **file** has no TTY to prompt on under a service: `install-service` installs it
  but won't enable it until you provide `ZEROSIGNAL_KEYSTORE_PASSPHRASE` to the service
  environment (it prints how).
