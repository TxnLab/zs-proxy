# zs-proxy

A drop-in, private, OpenAI-compatible inference proxy.

This repository hosts the **prebuilt release binaries** for `zs-proxy`. The
binaries on the [Releases](https://github.com/txnlab/zs-proxy/releases) page are
the canonical downloads for Homebrew, Scoop, and direct install.

## How it works

`zs-proxy` is a small **localhost daemon** that serves exactly one user — you —
backed by your [ZeroSignal](https://zerosignal.ai) account. Local clients (opencode,
LM Studio, the OpenAI SDKs, coding agents, …) point at it as if it were OpenAI; every
request is sealed and routed privately to a remote node, and the response is decrypted
before it comes back. Clients can't tell they aren't talking to OpenAI.

The `Authorization` header / API key is **always ignored** — admission is via your
on-chain account, not a static key. So any non-empty API key works in your client.

## Quick start

1. **Create + fund an account** at [zerosignal.ai](https://zerosignal.ai).
2. **Install** `zs-proxy` (see [Install](#install)).
3. **Import your account** into the proxy:
   ```sh
   zs-proxy wallet login
   ```
   (or just run `zs-proxy` once and follow the first-run prompt)
4. **Start the proxy:**
   ```sh
   zs-proxy proxy start
   ```
5. **Point your tool at it** — OpenAI-compatible base URL `http://localhost:8080/v1`,
   with any API key (it's ignored). That's it. 😀

## Install

### macOS / Linux (Homebrew)

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

### Linux / direct download

Grab the archive for your platform from the
[latest release](https://github.com/txnlab/zs-proxy/releases/latest):

| Platform | Asset |
|---|---|
| macOS (universal) | `zs-proxy_<version>_darwin_all.tar.gz` |
| Linux | `zs-proxy_<version>_linux_<arch>.tar.gz` |
| Windows | `zs-proxy_<version>_windows_<arch>.zip` |

Verify against `checksums.txt`, extract, and put `zs-proxy` on your `PATH`.

## Set up your account

Your `zs-proxy` wallet and the [zerosignal.ai](https://zerosignal.ai) web app are the
**same account** — create and fund it once on the web, then import it into the proxy.

The easiest import is a browser sign-in (no paste):

```sh
zs-proxy wallet login
```

This opens [zerosignal.ai](https://zerosignal.ai), you approve the handoff with your
passkey, and the recovery phrase is delivered back over a one-shot `localhost` listener —
it never touches a remote server. The derived address is identical to a manual import, so
the CLI and the browser app share one funded account. On a headless/SSH host, add
`--no-browser` to print the sign-in URL instead.

Other ways to set up the wallet:

```sh
zs-proxy wallet import     # paste the 24-word recovery phrase (web app → Settings → Recovery)
zs-proxy wallet new        # generate a fresh 24-word wallet (also importable into the web app)
zs-proxy                   # bare first run: interactive wallet bootstrap on the terminal
```

The wallet lives in your **OS keychain** (macOS Keychain / Windows Credential Manager /
Linux Secret Service) on signed releases. Inspect it with `zs-proxy wallet show` /
`zs-proxy wallet address`, or reveal the phrase with `zs-proxy wallet export`.

## Funding

The simplest path is to **fund on [zerosignal.ai](https://zerosignal.ai)** — it's the same
account the proxy uses. You can also fund from the CLI:

```sh
zs-proxy fund            # testnet: auto-dispense; mainnet: deposit address + QR + on-ramp links
zs-proxy fund --wait     # poll until ALGO arrives, then opt in to USDC
```

`zs-proxy` defaults to **testnet**. Select a network with
`--network testnet|mainnet|localnet` on any command. Confirm everything is ready with:

```sh
zs-proxy status     # wallet, balance, and funding status
zs-proxy doctor     # diagnose config → wallet → chain → funding → port
```

## Run the proxy

`proxy start` backgrounds a self-managed daemon and returns you to the shell with the
endpoint live on `:8080`:

```sh
zs-proxy proxy start              # start in the background
zs-proxy proxy start --foreground # serve in this process (supervisors / debugging)
zs-proxy proxy stop               # graceful drain of in-flight streams
zs-proxy proxy status             # pid, uptime, listen address, network
zs-proxy proxy restart            # restart the daemon
zs-proxy proxy logs -f            # follow the daemon log (metadata only — never prompts or responses)
```

`proxy start` accepts `--network`, `--port`, and `--config`. The background daemon does
**not** survive a reboot and isn't restarted on a crash — for that, install it as a service.

## Run as a service

To start at login/boot and restart on failure, register an OS service:

```sh
zs-proxy proxy install-service                     # register + enable (current config)
zs-proxy proxy install-service --network mainnet   # pin a --network / --config / --port
zs-proxy proxy install-service --print             # print the unit / command instead of installing
zs-proxy proxy uninstall-service                   # stop + remove
```

The binary registers itself — a **launchd LaunchAgent** (macOS), a **systemd user unit**
(Linux), or a **Windows service via the SCM** — running `proxy start --foreground` with its
resolved path and your flags baked in. Installs are **per-user** so the service can reach
your keychain wallet. On Windows this needs administrator rights (it raises a UAC prompt if
you aren't elevated) and runs as the installing user.

> **A wallet must already exist.** A service has no terminal to prompt on, so run
> `zs-proxy wallet login` (or `zs-proxy` once interactively) before installing the service.

## Connect your tools

Configure any OpenAI-compatible client to use:

- **Base URL:** `http://localhost:8080/v1`
- **API key:** anything — it's ignored

This is a drop-in OpenAI endpoint for opencode, LM Studio, the OpenAI SDKs, coding agents,
and similar tools. Quick sanity checks:

```sh
curl http://localhost:8080/healthz
curl http://localhost:8080/v1/models
```

Supported routes include `/v1/chat/completions`, `/v1/completions`, `/v1/responses`,
`/v1/models`, and `/v1/images/*`.

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
zs-proxy fund [--wait]                                 # testnet dispenser / mainnet deposit
zs-proxy status                                        # wallet, balance, funding status
zs-proxy doctor                                        # end-to-end diagnostics

# inspection
zs-proxy config path | print-effective                 # resolved config path / merged config
zs-proxy version                                        # product + proto versions + network ids
zs-proxy --help                                         # full command tree
```

Global flags (valid on most commands): `--network testnet|mainnet|localnet`, `--port`,
`--config <path>`, `--foreground`.

## Configuration

No `config.yaml` is required — `zs-proxy` ships with embedded per-network defaults and
boots on **testnet** by default. Self-hosters can override with `--config <path>` or
`PROXY_*` environment variables; inspect what's actually in effect with:

```sh
zs-proxy config path              # which config file would be loaded
zs-proxy config print-effective   # merged effective config (defaults + file + env + flags)
```

## License

[PolyForm Noncommercial 1.0.0](./LICENSE).

---

Releases are published automatically by [goreleaser](https://goreleaser.com).
The macOS binary is a code-signed, notarized universal binary.
