# zs-proxy

A drop-in, private, OpenAI-compatible inference proxy.

This repository hosts the **prebuilt release binaries** for `zs-proxy`. The
binaries on the [Releases](https://github.com/txnlab/zs-proxy/releases) page are
the canonical downloads for the installer script, Homebrew, and Scoop.

📖 **Full documentation: [docs.zerosignal.ai](https://docs.zerosignal.ai/using-the-proxy/overview)** —
this README is a condensed version of those pages.

## Get started

1. **Create and fund your account at [zerosignal.ai](https://zerosignal.ai).** Create a
   passkey, then add funds with a card, Apple Pay, Google Pay, or crypto. Already have a
   funded chat-app account? Skip this step.

2. **Install.** On macOS:
   ```sh
   brew install txnlab/tap/zs-proxy
   ```
   On Linux, or macOS without Homebrew:
   ```sh
   curl -fsSL https://zerosignal.ai/install.sh | sh
   ```
   If the installer says its directory isn't on your `PATH`, add the line it prints.
   Windows uses Scoop; see [Install](#install).

3. **Sign in:**
   ```sh
   zs-proxy wallet login
   ```
   Your browser opens zerosignal.ai. Note the address under **This account**, then
   approve with your passkey. Back in the terminal, answer `y` if it shows the same
   address. The
   proxy now uses the same account and balance as the chat app. Your recovery phrase goes
   straight from the browser to the CLI on your computer, never through a server.

4. **Start the proxy:**
   ```sh
   zs-proxy proxy start
   ```
   It runs in the background at `http://localhost:9376/v1`.

5. **Connect your tools:**
   ```sh
   zs-proxy connect
   ```
   This finds the AI tools you have installed and points them at the proxy. For any
   other OpenAI-compatible tool, use base URL `http://localhost:9376/v1` and any API key
   (the proxy ignores it).

6. **Check it:** `zs-proxy status` shows the account, its balance, and whether it's
   ready to pay for requests.

Setting up over SSH, on a server, or with a separate wallet for an agent? See
[Advanced: other wallet setups](#advanced-other-wallet-setups).

→ [Quick start](https://docs.zerosignal.ai/using-the-proxy/quick-start)

## How it works

`zs-proxy` is a small **localhost daemon** that serves exactly one user — you —
backed by your [ZeroSignal](https://zerosignal.ai) account. Local clients (opencode,
LM Studio, the OpenAI SDKs, coding agents, …) point at it as if it were OpenAI; every
request is sealed and routed privately to a remote node, and the response is decrypted
before it comes back. Clients can't tell they aren't talking to OpenAI.

The `Authorization` header / API key is **always ignored** — admission is via your
on-chain account, not a static key. So any non-empty API key works in your client.

→ [What is the proxy](https://docs.zerosignal.ai/using-the-proxy/overview) ·
[Privacy & security](https://docs.zerosignal.ai/for-users/privacy) ·
[Pricing](https://docs.zerosignal.ai/for-users/pricing)

## Install

### macOS / Linux (one-line installer)

```sh
curl -fsSL https://zerosignal.ai/install.sh | sh
```

Downloads the right archive for your platform, verifies it against `checksums.txt`, and
installs `zs-proxy` on your `PATH`. Pin a version with `ZS_VERSION=1.2.3` or change the
target directory with `ZS_INSTALL_DIR`.

### macOS (Homebrew)

```sh
brew install txnlab/tap/zs-proxy
```

The macOS build is a code-signed, notarized universal binary. This is a Homebrew
**cask**, so `brew services` does not apply — auto-start is handled by
`zs-proxy proxy install-service` (see [Run as a service](#run-as-a-service)).

### Windows (Scoop)

Install [Scoop](https://scoop.sh) first, then:

```powershell
scoop bucket add txnlab https://github.com/txnlab/scoop-bucket
scoop install zs-proxy
```

### Direct download

Grab the archive for your platform from the
[latest release](https://github.com/txnlab/zs-proxy/releases/latest):

| Platform | Asset |
|---|---|
| macOS (universal) | `zs-proxy_<version>_darwin_all.tar.gz` |
| Linux | `zs-proxy_<version>_linux_<arch>.tar.gz` |
| Windows | `zs-proxy_<version>_windows_<arch>.zip` |

Verify against `checksums.txt`, extract, and put `zs-proxy` on your `PATH`.

## Run the proxy

`proxy start` backgrounds a self-managed daemon and returns you to the shell with the
endpoint live on `127.0.0.1:9376`:

```sh
zs-proxy proxy start              # start in the background
zs-proxy proxy start --foreground # serve in this process (supervisors / debugging)
zs-proxy proxy stop               # graceful drain of in-flight streams
zs-proxy proxy status             # pid, uptime, listen address, network
zs-proxy proxy restart            # restart the daemon
zs-proxy proxy logs -f            # follow the daemon log (metadata only — never prompts or responses)
```

`proxy start` accepts `--port` and `--config`. The background daemon does
**not** survive a reboot and isn't restarted on a crash — for that, install it as a service.

## Run as a service

To start at login/boot and restart on failure, register an OS service:

```sh
zs-proxy proxy install-service                     # register + enable (current config)
zs-proxy proxy install-service --port 9376         # pin a --config / --port
zs-proxy proxy install-service --print             # print the unit / command instead of installing
zs-proxy proxy uninstall-service                   # stop + remove
```

The binary registers itself — a **launchd LaunchAgent** (macOS), a **systemd user unit**
(Linux), or a **Windows service via the SCM** — running `proxy start --foreground` with its
resolved path and your flags baked in. Installs are **per-user** so the service can reach
your keychain wallet. On Windows this needs administrator rights (it raises a UAC prompt if
you aren't elevated) and runs as the installing user.

> **Set up the wallet first.** A service has no terminal to prompt on, so it refuses to
> run without a wallet, and `install-service` checks before installing anything. Run
> `zs-proxy wallet login` first. (An env mnemonic in your shell doesn't count: the service
> never sees your shell's environment.)

→ [Running as a service](https://docs.zerosignal.ai/using-the-proxy/running-as-a-service)

## Connect your tools

`zs-proxy connect` autoconfigures third-party AI tools to use the running proxy:

```sh
zs-proxy connect            # detect installed tools and configure them
zs-proxy connect opencode   # configure just one
zs-proxy connect --list     # show detected tools and the config paths they'd get
zs-proxy connect --print    # render the config without writing anything
```

Known integrations: `opencode`, `openclaw`, `pi`, `hermes`, `aider`, `continue`, `codex`,
and `generic`. Step-by-step guides — including GUI apps `connect` doesn't write for
(SillyTavern, Open WebUI, LibreChat, Chatbox, Cherry Studio) — are in the
[how-to guides](https://docs.zerosignal.ai/using-the-proxy/guides).

For anything else, configure it manually as an OpenAI-compatible endpoint:

- **Base URL:** `http://localhost:9376/v1`
- **API key:** anything — it's ignored

Quick sanity checks:

```sh
curl http://localhost:9376/healthz
curl http://localhost:9376/v1/models
```

Supported routes include `/v1/chat/completions`, `/v1/completions`, `/v1/responses`,
`/v1/models`, and `/v1/images/*`.

→ [Connecting AI tools](https://docs.zerosignal.ai/using-the-proxy/connecting-tools) ·
[MCP servers](https://docs.zerosignal.ai/using-the-proxy/mcp)

## Advanced: other wallet setups

[Get started](#get-started) signs the proxy in to your zerosignal.ai account, so both
share one balance. Other ways to set up the wallet:

```sh
zs-proxy wallet login --no-browser   # SSH / headless: print the sign-in URL instead of opening it
zs-proxy wallet import               # paste the 24-word phrase (web app → Settings → Recovery)
zs-proxy wallet import --stdin       # ...or pipe it in, for scripts
zs-proxy wallet new                  # a fresh, empty wallet that isn't your web account
zs-proxy fund --wait                 # fund that wallet: deposit address + QR, then wait for it
```

- **`--no-browser`:** the sign-in finishes on a temporary `127.0.0.1` listener on the
  machine running `zs-proxy`, at the `port=` in the printed URL, and waits 3 minutes. To
  finish it from a browser on another computer, open a second SSH session from that
  computer that forwards the port (`ssh -L <port>:127.0.0.1:<port> <host>`), then open the
  URL. If you can't forward a port, use `wallet import` instead.
- **One wallet per OS user.** To give an agent its own budget, run it under a separate OS
  user or machine and use `wallet new` there, or give it an env mnemonic (below).
- **Servers and CI:** set any `*_MNEMONIC` environment variable (a 24- or 25-word phrase),
  or `ZS_MNEMONIC_URLS` to load it from AWS, GCP, or Azure secret stores. `proxy start`
  then skips wallet setup entirely. The `status`, `fund`, `slots`, and `doctor` commands
  read only the keychain wallet, so they don't work with an env mnemonic. An installed
  service doesn't inherit your shell's environment either, so `install-service` refuses
  without a keychain wallet; use `install-service --print` and add the variable to the
  unit yourself.
- **Switching accounts:** `wallet login` / `import` / `new` refuse to replace an existing
  wallet unless you pass `--force`. A running proxy keeps paying from the old wallet until
  you run `zs-proxy proxy restart`.
- **No silent wallets:** with no wallet and no terminal to ask on (a service, a script,
  piped input), the proxy exits with an error instead of creating an empty wallet.

The wallet lives in your **OS keychain** (macOS Keychain / Windows Credential Manager /
Linux Secret Service) on signed releases. Inspect it with `zs-proxy wallet show`, or reveal
the phrase with `zs-proxy wallet export`.

→ [Other ways to set up your wallet](https://docs.zerosignal.ai/using-the-proxy/wallet-and-funding#other-ways-to-set-up-your-wallet) ·
[Funding](https://docs.zerosignal.ai/using-the-proxy/wallet-and-funding#funding) ·
[Recovery](https://docs.zerosignal.ai/for-users/recovery)

## Command reference

```sh
# lifecycle
zs-proxy proxy start | stop | status | restart        # manage the background daemon
zs-proxy proxy start --foreground                      # serve in the current process
zs-proxy proxy logs [-f] [-n <lines>]                  # tail the daemon log
zs-proxy proxy install-service | uninstall-service     # run at login/boot via the OS service manager

# wallet
zs-proxy wallet login                                  # browser passkey import (no paste)
zs-proxy wallet import | new | export                  # paste-import / generate / reveal phrase
zs-proxy wallet show | address                         # inspect the loaded wallet
zs-proxy wallet opt-in                                 # opt the account in to USDC

# funding + health
zs-proxy fund [--wait]                                 # deposit address + QR + on-ramp links
zs-proxy status                                        # wallet, balance, funding status
zs-proxy doctor                                        # end-to-end diagnostics

# tools
zs-proxy connect [tool]                                # autoconfigure AI tools to use the proxy

# concurrency + the prepaid ticket pool
zs-proxy slots [n]                                     # show or set how many tickets can be open at once
zs-proxy withdraw <amount-algo>                        # reclaim unreserved ALGO from the pool
zs-proxy close-deposit                                 # close the pool and refund the full balance

# tickets
zs-proxy tickets                                       # open escrow tickets and what can resolve them
zs-proxy tickets get <ticket-id> | recover [ticket-id] # inspect / resolve one ticket

# inspection
zs-proxy config path | print-effective                 # resolved config path / merged config
zs-proxy version                                       # product + proto versions + network ids
zs-proxy --help                                        # full command tree
```

Flags are bound per command, not globally: `--config <path>` is accepted by most
commands, `--port` by the `proxy` lifecycle commands and `install-service`, and
`--foreground` only by `proxy start`.

→ [Proxy CLI reference](https://docs.zerosignal.ai/reference/cli-1) — every command and flag.

## Configuration

No `config.yaml` is required — `zs-proxy` ships with embedded per-network defaults and
boots on **mainnet** by default, listening on `127.0.0.1:9376`. Self-hosters can override
with `--config <path>` or `PROXY_*` environment variables; inspect what's actually in
effect with:

```sh
zs-proxy config path              # which config file would be loaded
zs-proxy config print-effective   # merged effective config (defaults + file + env + flags)
```

`PROXY_CONFIG` sets the config path without passing `--config`, and
`PROXY_ZS_CHECK_UPDATES=false` silences the once-daily "newer version available" notice.

→ [Configuration](https://docs.zerosignal.ai/using-the-proxy/configuration) ·
[Routing preferences](https://docs.zerosignal.ai/reference/routing-preferences)

## Documentation

| | |
|---|---|
| [What is the proxy](https://docs.zerosignal.ai/using-the-proxy/overview) | What it is, who it's for, how it relates to the chat app |
| [Quick start](https://docs.zerosignal.ai/using-the-proxy/quick-start) | Account → install → sign in → start → connect |
| [Connecting AI tools](https://docs.zerosignal.ai/using-the-proxy/connecting-tools) | The `connect` command, supported tools, manual setup |
| [How-to guides](https://docs.zerosignal.ai/using-the-proxy/guides) | Per-app setup for opencode, Codex, Aider, SillyTavern, Open WebUI, LibreChat, … |
| [MCP servers](https://docs.zerosignal.ai/using-the-proxy/mcp) | Using MCP through the proxy |
| [Wallet & funding](https://docs.zerosignal.ai/using-the-proxy/wallet-and-funding) | Every way to set up a wallet, funding, the prepaid ticket pool |
| [Configuration](https://docs.zerosignal.ai/using-the-proxy/configuration) | `config.yaml`, env overrides, spend caps, operator selection |
| [Running as a service](https://docs.zerosignal.ai/using-the-proxy/running-as-a-service) | launchd / systemd / Windows service |
| [Proxy CLI reference](https://docs.zerosignal.ai/reference/cli-1) | Every command and flag |
| [Routing preferences](https://docs.zerosignal.ai/reference/routing-preferences) | Pin operators, price ceilings, per-request relay |
| [Pricing](https://docs.zerosignal.ai/for-users/pricing) | What a request costs and how the fee breaks down |
| [Privacy & security](https://docs.zerosignal.ai/for-users/privacy) | End-to-end encryption, relays, what each party sees |
| [Troubleshooting](https://docs.zerosignal.ai/for-users/troubleshooting#using-the-proxy) | Common problems and fixes |

Running a node instead of using one? See the
[operator guide](https://docs.zerosignal.ai/for-operators/what-is-an-operator).

## License

[Apache License 2.0](./LICENSE).

---

Releases are published automatically by [goreleaser](https://goreleaser.com).
The macOS binary is a code-signed, notarized universal binary.
