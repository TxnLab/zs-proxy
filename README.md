# zs-proxy

A drop-in, private, OpenAI-compatible inference proxy.

This repository hosts the **prebuilt release binaries** for `zs-proxy`. The
binaries on the [Releases](https://github.com/txnlab/zs-proxy/releases) page are
the canonical downloads for Homebrew, Scoop, and direct install.

## Install

### macOS / Linux (Homebrew)

```sh
brew install txnlab/tap/zs-proxy
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

## Usage

```sh
zs-proxy --help
zs-proxy version
```

## License

[PolyForm Noncommercial 1.0.0](./LICENSE).

---

Releases are published automatically by [goreleaser](https://goreleaser.com).
The macOS binary is a code-signed, notarized universal binary.
