"""ZeroSignal provider profile (through the local zs-proxy daemon).

ZeroSignal (https://zerosignal.ai) is a private, pay-per-use inference
network. ``zs-proxy`` runs on the user's machine, exposes the network as an
OpenAI-compatible API on ``http://127.0.0.1:9376/v1``, encrypts every request
end to end, and settles payment from the user's on-chain wallet. There is no
upstream API key: the proxy ignores ``Authorization``, so any non-empty
``ZEROSIGNAL_API_KEY`` satisfies the api_key auth path and never leaves the
machine.

The canonical id is ``zs-proxy`` (the daemon Hermes actually talks to), with
``zerosignal`` as an alias. ``zs-proxy connect hermes`` writes a user-declared
``providers.zerosignal`` entry into config.yaml; keeping ``zerosignal`` an
alias rather than the canonical name lets that entry keep winning over this
bundled profile, so installs configured before this plugin existed are not
shadowed.
"""

from __future__ import annotations

import json
import logging
import urllib.request
from typing import Any

from providers import register_provider
from providers.base import ProviderProfile

logger = logging.getLogger(__name__)

DEFAULT_ZEROSIGNAL_BASE_URL = "http://127.0.0.1:9376/v1"


def _serves_text(model: dict[str, Any]) -> bool:
    """True unless the catalog entry declares non-text output only.

    ``/v1/models`` lists image generators and editors next to the chat
    models (``architecture.output_modalities == ["image"]``). A chat
    session cannot use them, so they are dropped from the picker.
    """
    arch = model.get("architecture")
    if not isinstance(arch, dict):
        return True
    outputs = arch.get("output_modalities")
    if not isinstance(outputs, list) or not outputs:
        return True
    return "text" in outputs


class ZeroSignalProfile(ProviderProfile):
    """ZeroSignal via zs-proxy: live catalog minus image-only models."""

    def fetch_models(
        self,
        *,
        api_key: str | None = None,
        base_url: str | None = None,
        timeout: float = 8.0,
    ) -> list[str] | None:
        base = ((base_url or "").strip() or self.base_url).rstrip("/")
        if not base:
            return None

        from hermes_cli.urllib_security import open_credentialed_url

        req = urllib.request.Request(base + "/models")
        if api_key:
            req.add_header("Authorization", f"Bearer {api_key}")
        req.add_header("Accept", "application/json")
        try:
            with open_credentialed_url(req, timeout=timeout) as resp:
                data = json.loads(resp.read().decode())
        except Exception as exc:
            logger.debug("fetch_models(%s): %s", self.name, exc)
            return None
        items = data if isinstance(data, list) else data.get("data", [])
        return [
            m["id"]
            for m in items
            if isinstance(m, dict) and "id" in m and _serves_text(m)
        ]


zerosignal = ZeroSignalProfile(
    name="zs-proxy",
    aliases=("zerosignal", "zero-signal"),
    display_name="ZeroSignal",
    description="ZeroSignal: private pay-per-use inference through the local zs-proxy daemon",
    signup_url="https://zerosignal.ai",
    env_vars=("ZEROSIGNAL_API_KEY", "ZEROSIGNAL_BASE_URL"),
    base_url=DEFAULT_ZEROSIGNAL_BASE_URL,
    auth_type="api_key",
    # Cheapest text model on the network at the time of writing; any chat
    # model works, the proxy prices each request from the live catalog.
    default_aux_model="glm-5.3-flash",
    # Tool-calling chat models served on the network. Shown only when the
    # live /v1/models fetch fails (the proxy is not running).
    fallback_models=(
        "moonshotai/kimi-k2.7-code",
        "glm-5.3",
        "deepseek/deepseek-v4-pro",
        "openai/gpt-5.6-luna",
        "anthropic/claude-sonnet-5",
        "qwen/qwen3.8-max",
        "minimax/minimax-m3",
        "glm-5.3-flash",
    ),
)

register_provider(zerosignal)
