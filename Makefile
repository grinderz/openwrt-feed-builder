MAKEFLAGS += --warn-undefined-variables

.ONESHELL:
.SHELLFLAGS := -o errtrace -o pipefail -o noclobber -o errexit -o nounset -c
SHELL := bash

# local user config (gitignored, see local.mk.example): set DEPLOY_DEST there
# instead of editing this Makefile; CLI variables still win over it
-include local.mk


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


##@ Deploy Targets


# rsync destination (user@host:/path, no trailing slash), set in local.mk
DEPLOY_DEST ?=
# never sent to the host; under --delete (without --delete-excluded) these
# are also left alone on the receiver
# leading / anchors a pattern to the repo root (bare names match anywhere)
# keys/ (the usign secret key) DOES sync: the build host builds and signs the
# feed, the laptop only fetches the result — keep DEPLOY_DEST private
DEPLOY_EXCLUDES := .git .claude .idea .DS_Store /openwrt-feed-builder /feedbuilder \
	/.cache /releases '/releases.*' /output '/output.*' \
	'sdk-test-*' '*.log'
# host-only state that must survive even an exclude-list mistake: rsync 'P'
# filters forbid deletion regardless of --delete and exclude typos.
# local.mk, config.yaml and keys are deliberately NOT here: the laptop copies
# are the source of truth and overwrite the host ones on deploy
DEPLOY_PROTECT := .cache releases

DEPLOY_RSYNC = rsync -av --delete \
	$(foreach e,$(DEPLOY_EXCLUDES),--exclude=$(e)) \
	$(foreach p,$(DEPLOY_PROTECT),--filter='P /$(p)') \
	./ "$(DEPLOY_DEST)/"

define Deploy/Check
	@if [ -z "$(DEPLOY_DEST)" ]; then
		echo " - DEPLOY_DEST not set, add to local.mk:"
		echo "   DEPLOY_DEST := user@buildhost:/home/user/src/openwrt-feed-builder"
		exit 1
	fi
endef

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
	@if [ -z "$(PUBLISH_DEST)" ]; then
		echo " - PUBLISH_DEST not set, add to local.mk:"
		echo "   PUBLISH_DEST := webserver:/var/www/openwrt/"
		exit 1
	fi
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
publish: ## Publish the generated feed to PUBLISH_DEST (web server)
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
