MAKEFLAGS += --warn-undefined-variables

.ONESHELL:
.SHELLFLAGS := -o errtrace -o pipefail -o noclobber -o errexit -o nounset -c
SHELL := bash

# local user config (gitignored, see local.mk.example): set DEPLOY_DEST there
# instead of editing this Makefile; CLI variables still win over it
-include local.mk

FEEDBUILDER := go run ./cmd/openwrt-feed-builder

# fail with a local.mk hint when a destination variable is empty
# $(1): variable name, $(2): example value
define Check/Dest
	@if [ -z "$($(1))" ]; then
		echo " - $(1) not set, add to local.mk:"
		echo "   $(1) := $(2)"
		exit 1
	fi
endef


##@ General


.DEFAULT_GOAL := help
.PHONY: help
help: ## Display this help screen
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9\-\\.%]+:.*?##/ { printf "  \033[36m%-29s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)


##@ Dev Targets


.PHONY: build
build: ## Build the binary
	go build -o openwrt-feed-builder ./cmd/openwrt-feed-builder

.PHONY: test
test: ## Run tests
	go test ./...


##@ Feed Targets


# config the feed commands run with (override per invocation or in local.mk)
FEED_CONFIG ?= config.yaml
# extra `build` flags: make feed FEED_ARGS="--full --sign --only sdk"
FEED_ARGS ?=

.PHONY: feed
feed: ## Build the feed (incremental, unsigned; flags via FEED_ARGS)
	$(FEEDBUILDER) -c $(FEED_CONFIG) build $(FEED_ARGS)

.PHONY: sign
sign: ## Sign the feed tree in place (Packages.sig + repo.pub + add.sh)
	$(FEEDBUILDER) -c $(FEED_CONFIG) sign

# verifies exactly the tree publish ships ($(PUBLISH_SRC)), not the config's
# output_dir — the two may diverge
.PHONY: verify
verify: ## Validate every feed signature + repo.pub + add.sh key step
	$(FEEDBUILDER) -c $(FEED_CONFIG) verify "$(PUBLISH_SRC)"

.PHONY: serve
serve: ## Serve the feed over HTTP (config `serve` section)
	$(FEEDBUILDER) -c $(FEED_CONFIG) serve

.PHONY: genkey
genkey: ## Generate a usign keypair into keys/
	$(FEEDBUILDER) genkey


##@ Deploy Targets


# rsync destination (user@host:/path, no trailing slash), set in local.mk
DEPLOY_DEST ?=
# never sent to the host; under --delete (without --delete-excluded) these
# are also left alone on the receiver
# leading / anchors a pattern to the repo root (bare names match anywhere)
# keys/ (the usign secret key) never leaves this machine: the build host
# builds an UNSIGNED tree, `make fetch` pulls it here and `sign` signs it
DEPLOY_EXCLUDES := .git .claude .idea .DS_Store /openwrt-feed-builder /feedbuilder \
	/.cache /releases '/releases.*' /output '/output.*' /keys \
	'sdk-test-*' '*.log'
# host-only state that must survive even an exclude-list mistake: rsync 'P'
# filters forbid deletion regardless of --delete and exclude typos.
# local.mk and config.yaml are deliberately NOT here: the laptop copies are
# the source of truth and overwrite the host ones on deploy
DEPLOY_PROTECT := .cache releases keys

DEPLOY_RSYNC = rsync -av --delete \
	$(foreach e,$(DEPLOY_EXCLUDES),--exclude=$(e)) \
	$(foreach p,$(DEPLOY_PROTECT),--filter='P /$(p)') \
	./ "$(DEPLOY_DEST)/"

Deploy/Check = $(call Check/Dest,DEPLOY_DEST,user@buildhost:/home/user/src/openwrt-feed-builder)

.PHONY: deploy.diff
deploy.diff: ## Preview deploy (rsync dry-run: what gets sent/deleted)
	$(call Deploy/Check)
	$(DEPLOY_RSYNC) --dry-run

.PHONY: deploy
deploy: ## Deploy repo to DEPLOY_DEST (host-only paths protected from --delete)
	$(call Deploy/Check)
	$(DEPLOY_RSYNC)


##@ Publish Targets


# rsync destination for the GENERATED FEED (host:/path/, trailing slash —
# the source dir lands inside it), set in local.mk. Not to be confused with
# DEPLOY_DEST above, which syncs this repo to a build host.
PUBLISH_DEST ?=
# the feed tree produced by `openwrt-feed-builder build` (output_dir)
PUBLISH_SRC ?= releases

PUBLISH_RSYNC = rsync -aP --delete "$(PUBLISH_SRC)" "$(PUBLISH_DEST)"

define Publish/Check
	$(call Check/Dest,PUBLISH_DEST,webserver:/var/www/openwrt/)
	if [ ! -d "$(PUBLISH_SRC)" ]; then
		echo " - source directory '$(PUBLISH_SRC)' not found — run the build first"
		exit 1
	fi
endef

.PHONY: publish.diff
publish.diff: ## Preview feed publish (rsync dry-run)
	$(call Publish/Check)
	$(PUBLISH_RSYNC) --dry-run

.PHONY: publish
publish: verify ## Publish the generated feed to PUBLISH_DEST (web server; refuses unsigned/broken signatures)
	$(call Publish/Check)
	$(PUBLISH_RSYNC)

# pull the feed a remote `build` produced on the DEPLOY_DEST host back here
# (e.g. sdk sources compile on the build host, publish happens from this
# machine); the local $(PUBLISH_SRC) mirrors the host's copy, --delete included
FETCH_RSYNC = rsync -av --delete "$(DEPLOY_DEST)/$(PUBLISH_SRC)/" "./$(PUBLISH_SRC)/"

.PHONY: fetch.diff
fetch.diff: ## Preview feed fetch from the DEPLOY_DEST build host (rsync dry-run)
	$(call Deploy/Check)
	$(FETCH_RSYNC) --dry-run

.PHONY: fetch
fetch: ## Fetch the generated feed (releases/) from the DEPLOY_DEST build host
	$(call Deploy/Check)
	$(FETCH_RSYNC)
