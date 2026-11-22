GO      ?= go
OUT_DIR ?= out

GOOS        := linux
GOARCH      := amd64
CGO_ENABLED := 0

LDFLAGS     := -s -w
BUILD_FLAGS := -trimpath -ldflags "$(LDFLAGS)"

GOBUILD_ENV := CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH)

# Version metadata stamped into internal/build (see internal/build/build.go).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
VERSION_LDFLAGS := \
	-X github.com/TxnLab/zs-proxy/internal/build.Version=$(VERSION) \
	-X github.com/TxnLab/zs-proxy/internal/build.Commit=$(COMMIT)

# Release archives build on the newest patch of go.mod's Go minor, not on
# whatever Go the releaser has installed: go.mod's directive is only a minimum.
# scripts/release-toolchain.sh resolves it, and goreleaser runs under that
# GOTOOLCHAIN.
GO_MINOR := $(shell awk '/^go /{sub(/\r$$/, ""); split($$2, v, "."); print v[1] "." v[2]}' go.mod)

# Local macOS code-signing identity for `make signed` (see scripts/dev-cert-macos.sh).
MACOS_SIGN_IDENTITY ?=

# Notarization (release only). NOTARY_PROFILE is the notarytool keychain profile
# created by `make notary-setup`; ARTIFACT defaults to goreleaser's universal
# darwin archive (see name_template in .goreleaser.yaml).
NOTARY_PROFILE ?= zerosignal-notary
ARTIFACT       ?= $(wildcard dist/zs-proxy_*_darwin_all.tar.gz)

.PHONY: build zs-proxy signed dev-cert lint-sh \
        notary-setup notarize release-notes release release-snapshot release-check clean

# Native dev build of the zs-proxy CLI. No version/KeyringBackend stamp, so
# the wallet uses the encrypted-file backend (no macOS Keychain auth prompts).
zs-proxy: | $(OUT_DIR)
	CGO_ENABLED=1 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT_DIR)/zs-proxy .

# Signed local build: stamps version + KeyringBackend=os (OS keychain) and
# code-signs with MACOS_SIGN_IDENTITY. Run `make dev-cert` once to create a
# self-signed identity, then:
#   MACOS_SIGN_IDENTITY="zerosignal Local Dev" SIGN_NO_TIMESTAMP=1 make signed
signed: | $(OUT_DIR)
	CGO_ENABLED=1 $(GO) build -trimpath \
	  -ldflags "$(LDFLAGS) $(VERSION_LDFLAGS) -X github.com/TxnLab/zs-proxy/internal/build.KeyringBackend=os" \
	  -o $(OUT_DIR)/zs-proxy .
	MACOS_SIGN_IDENTITY="$(MACOS_SIGN_IDENTITY)" ./scripts/sign-macos.sh $(OUT_DIR)/zs-proxy

# Lint the public installer and the release toolchain resolver. Not scripts/*.sh:
# the three macOS release scripts predate this and haven't been triaged, and a
# lint that is red on arrival gets ignored. CI runs the same command.
lint-sh:
	shellcheck scripts/install.sh scripts/release-toolchain.sh

# Create a self-signed code-signing identity for local signed builds (macOS).
dev-cert:
	./scripts/dev-cert-macos.sh

# One-time: store notarytool credentials as a keychain profile. Omits --password
# so notarytool prompts interactively (keeps the app-specific pw out of shell
# history). Needs an app-specific password from appleid.apple.com.
#   make notary-setup APPLE_ID=you@example.com TEAM_ID=TEAMID
notary-setup:
	xcrun notarytool store-credentials $(NOTARY_PROFILE) \
	  --apple-id $(APPLE_ID) --team-id $(TEAM_ID)

# Notarize + staple a built artifact (defaults to the darwin universal archive).
notarize:
	NOTARY_PROFILE=$(NOTARY_PROFILE) ./scripts/notarize-macos.sh $(ARTIFACT)

# Real cross-platform release (run on a pushed tag): goreleaser builds + signs
# the darwin universal binary (post-hook, exported MACOS_SIGN_IDENTITY) and
# publishes the release immediately to txnlab/zs-proxy (draft: false); then we
# notarize that signed binary. The binaries are unstapled, so Gatekeeper checks
# notarization online — the brief window between publish and notarytool approval
# is transient + self-healing (a copy downloaded in that window passes once Apple
# approves). `$(MAKE) notarize` re-evaluates ARTIFACT after dist/ is populated.
# Release notes: the goreleaser auto-changelog is disabled (.goreleaser.yaml) in
# favor of a curated RELEASE_NOTES.md, which keeps goreleaser's own body empty
# (GitHub then shows the repo's HEAD commit subject instead). IMPORTANT:
# `changelog.disable: true` ALSO makes goreleaser silently ignore `--release-notes`,
# so we do NOT pass it — goreleaser publishes the artifacts with an empty body, then
# we set the body ourselves via `gh release edit` (below). Without a RELEASE_NOTES.md
# the body stays empty. RELEASE_REPO mirrors .goreleaser.yaml's release.github target.
RELEASE_NOTES ?= RELEASE_NOTES.md
RELEASE_REPO  ?= txnlab/zs-proxy

# `make release-notes` drafts the notes with a model instead of an awk one-liner.
# RELEASE_NOTES_URL must point at a RUNNING zs-proxy (`zs-proxy proxy start`) — the
# drafting call is a real, billed chat completion against your own wallet, so it needs
# a funded wallet and an operator serving the model. PRODUCT is derived from
# RELEASE_REPO so the prompt names the right binary and audience.
RELEASE_NOTES_MODEL ?= grok-4.6
RELEASE_NOTES_URL   ?= http://127.0.0.1:9376/v1
PRODUCT             ?= $(notdir $(RELEASE_REPO))

# Draft RELEASE_NOTES.md from the commits since the previous tag. Feeds each commit's
# FULL message body to a model on a running zs-proxy and asks for PUBLIC, user-facing
# notes: a curated summary (Breaking changes / Highlights / Improvements / Fixes /
# Upgrade notes) sized to the Discord embed's 60-line / 3800-byte budget, then a
# `### Details` narrative explaining what the release is about. No leading version
# heading — the GitHub release title and the Discord message_template carry it.
# The tool never sends commit hashes, author names, or file paths, and the prompt
# asks for user-facing notes only — but it is still a model, so this is a DRAFT.
# REVIEW/EDIT before `make release` (the body is published as-is).
# A failed call leaves an existing RELEASE_NOTES.md untouched; the result is leak-scanned.
# NOTE: no $(GOBUILD_ENV) prefix — GOOS/GOARCH above are for the release build and
# would cross-compile this host tool to linux/amd64.
#   make release-notes                                     # grok-4.6, local proxy
#   make release-notes RELEASE_NOTES_MODEL=claude-opus-5   # a different model
#   make release-notes ARGS=-dry-run                       # print the prompt, no spend
#   make release-notes ARGS=-offline                       # deterministic buckets, no model
#   make release-notes ARGS="-from v0.9.0"                 # override the range
release-notes:
	$(GO) run github.com/TxnLab/zerosignal/go/cmd/releasenotes \
	  -product $(PRODUCT) \
	  -url $(RELEASE_NOTES_URL) \
	  -model $(RELEASE_NOTES_MODEL) \
	  -out $(RELEASE_NOTES) $(ARGS)

release:
	@tc="$$(./scripts/release-toolchain.sh $(GO_MINOR))" || exit 1; \
	echo ">> building with $$tc"; \
	if [ -f .env.release ]; then set -a; . ./.env.release; set +a; echo ">> loaded .env.release (repo-local release env)"; fi; \
	notes="$$(awk '/^### Details/{exit} {print}' $(RELEASE_NOTES) 2>/dev/null | head -c 3800 || true)"; \
	if [ -n "$$DISCORD_WEBHOOK_ID" ] && [ -n "$$DISCORD_WEBHOOK_TOKEN" ]; then \
	  ann_skip=false; echo ">> Discord announce ON (shared #releases channel)"; \
	else \
	  ann_skip=true;  echo ">> Discord announce OFF — set DISCORD_WEBHOOK_ID + DISCORD_WEBHOOK_TOKEN to enable"; \
	fi; \
	ZS_ANNOUNCE_SKIP="$$ann_skip" ZS_RELEASE_NOTES="$$notes" GOTOOLCHAIN="$$tc" \
	  GITHUB_TOKEN="$$(gh auth token)" goreleaser release --clean
	@tag=$$(git describe --tags --abbrev=0 --match 'v*'); \
	if [ -f "$(RELEASE_NOTES)" ]; then \
	  echo ">> setting $(RELEASE_REPO) $$tag release body from $(RELEASE_NOTES)"; \
	  gh release edit "$$tag" --repo "$(RELEASE_REPO)" --notes-file "$(RELEASE_NOTES)"; \
	else \
	  echo ">> no $(RELEASE_NOTES) — leaving $$tag release body empty"; \
	fi
	$(MAKE) notarize
# Full cross-platform release dry-run (needs goreleaser + a macOS host for darwin/cgo).
release-snapshot:
	@tc="$$(./scripts/release-toolchain.sh $(GO_MINOR))" || exit 1; \
	echo ">> building with $$tc"; \
	GOTOOLCHAIN="$$tc" goreleaser release --snapshot --clean

release-check:
	goreleaser check

$(OUT_DIR):
	mkdir -p $(OUT_DIR)

clean:
	rm -rf $(OUT_DIR)
