# Hermes Agent provider plugin

A [Hermes Agent](https://hermes-agent.nousresearch.com/) model-provider plugin that adds
**ZeroSignal** to Hermes' provider picker, backed by the local `zs-proxy` daemon.

Compared with pointing Hermes' generic Custom Endpoint at the proxy, the plugin gives you:

- a named provider in `hermes model` and `hermes setup`, selectable with `--provider zs-proxy`
  (`zerosignal` is an alias);
- the live model catalog from `/v1/models`, with image-only models filtered out;
- a cheap default model for Hermes' auxiliary tasks (compression, titles);
- a curated fallback list of tool-calling models when the proxy is not running.

## Install

Copy the plugin into your Hermes home (`~/.hermes` by default). Hermes discovers it on the
next run; no restart of anything else is needed.

```bash
mkdir -p ~/.hermes/plugins/model-providers/zerosignal
cd ~/.hermes/plugins/model-providers/zerosignal
curl -fsSLO https://raw.githubusercontent.com/TxnLab/zs-proxy/main/integrations/hermes/zerosignal/__init__.py \
     -O https://raw.githubusercontent.com/TxnLab/zs-proxy/main/integrations/hermes/zerosignal/plugin.yaml
```

Then start the proxy and set any non-empty key. The proxy ignores the key; Hermes only needs
it to treat the provider as configured:

```bash
zs-proxy proxy start
export ZEROSIGNAL_API_KEY="zerosignal-local"   # or add it to ~/.hermes/.env
hermes model                                   # choose ZeroSignal, then a model
```

Set `ZEROSIGNAL_BASE_URL` if the proxy listens on a port other than 9376.

## `zs-proxy connect hermes`

`zs-proxy connect hermes` still works without the plugin: it writes a `providers.zerosignal`
entry into `~/.hermes/config.yaml` using Hermes' custom-provider path. If both are present,
that user-declared entry wins, because the plugin registers `zerosignal` as an alias rather
than its canonical name.

## Upstream

The same plugin is proposed as a bundled Hermes provider in
[NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent). Once merged, the
copy step above is unnecessary.
