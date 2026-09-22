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



##@ Test Targets


GO_MOD_ID := openwrt-feed-builder
ARTIFACTS_DIR ?= artifacts
ARGS ?=

GO_GOTESTSUM_VERSION ?= 1.13.0
GO_GOTESTSUM_ARGS ?= --format testname
GO_TEST_CMD ?= go run gotest.tools/gotestsum@v$(GO_GOTESTSUM_VERSION) $(GO_GOTESTSUM_ARGS) --
GO_TEST_COVERAGE_EXCLUDE ?= (_mock\.go|\/testmocks\/|\.pb\.go)
GO_TEST_COVERAGE_THRESHOLD ?= 0
GO_TEST_REPEAT_COUNT ?= 2
# on: a random order per pass, the seed is printed on failure and can be
# passed back here to reproduce it
GO_TEST_SHUFFLE ?= on
# build tags for the tests, space or comma separated. A repeated -tags flag
# replaces the earlier one, so all tags are joined into a single flag here and
# test.coverage adds its own tag to the list instead of passing a second flag
GO_TEST_TAGS ?=
go-empty :=
go-space := $(go-empty) $(go-empty)
go-comma := ,
GO_TEST_TAGS_FLAG = $(if $(strip $(GO_TEST_TAGS)),-tags=$(subst $(go-space),$(go-comma),$(strip $(subst $(go-comma),$(go-space),$(GO_TEST_TAGS)))))

# debian: a plain golang image, where the tests needing usign / apk-tools /
# fakeroot / shellcheck skip themselves; alpine: the musl build.
GO_DOCKER_GO_VERSION ?= $(shell sed -n 's/^go \([0-9.]*\)$$/\1/p' go.mod)
GO_DOCKER_DEBIAN_VERSION ?= trixie
GO_DOCKER_ALPINE_VERSION ?= 3.24
GO_DOCKER_IMAGE ?= golang:$(GO_DOCKER_GO_VERSION)-$(GO_DOCKER_DEBIAN_VERSION)
GO_DOCKER_ALPINE_IMAGE ?= golang:$(GO_DOCKER_GO_VERSION)-alpine$(GO_DOCKER_ALPINE_VERSION)
# -race needs cgo: the alpine image lacks make and gcc; its apk-tools v3 and
# shellcheck let the apk / add.sh tests run there too (as root, no fakeroot)
GO_DOCKER_ALPINE_SETUP_CMD ?= apk add --no-cache bash make gcc musl-dev git shellcheck
# runs before make inside the container
GO_DOCKER_SETUP_CMD ?= :
# named volumes: without them every run downloads the modules and rebuilds all
GO_DOCKER_MOD_CACHE_VOLUME ?= go-mod-cache
GO_DOCKER_BUILD_CACHE_VOLUME ?= go-build-cache

$(ARTIFACTS_DIR):
	mkdir -p $@

.PHONY: test
test: ## Run tests (gotestsum, -race)
	$(GO_TEST_CMD) -v -race $(GO_TEST_TAGS_FLAG) $(ARGS) $(GO_MOD_ID)/...

# runs every test N times in one process and in a shuffled order: state that
# leaks between runs only shows up on the second pass, and order dependence
# only in a different order, both stay invisible to a plain make test
.PHONY: test.repeat
test.repeat: override ARGS := -count=$(GO_TEST_REPEAT_COUNT) -shuffle=$(GO_TEST_SHUFFLE) $(ARGS)
test.repeat: test ## Run tests GO_TEST_REPEAT_COUNT times in one process, shuffled

# override: a command line GO_TEST_TAGS= or ARGS= extends these instead of
# replacing them, otherwise the coverage tag or the coverprofile flag is lost
.PHONY: test.coverage
test.coverage: override GO_TEST_TAGS := coverage $(GO_TEST_TAGS)
test.coverage: override ARGS := -coverpkg=$(GO_MOD_ID)/... -covermode=atomic -coverprofile=$(ARTIFACTS_DIR)/coverage.out.tmp $(ARGS)
test.coverage: $(ARTIFACTS_DIR) test ## Run tests with coverage report (GO_TEST_COVERAGE_THRESHOLD)
	grep -vE "$(GO_TEST_COVERAGE_EXCLUDE)" $(ARTIFACTS_DIR)/coverage.out.tmp >| $(ARTIFACTS_DIR)/coverage.out
	go tool cover -html=$(ARTIFACTS_DIR)/coverage.out -o $(ARTIFACTS_DIR)/coverage.html
	go tool cover -func=$(ARTIFACTS_DIR)/coverage.out
	./scripts/check-coverage.sh $(ARTIFACTS_DIR)/coverage.out $(GO_TEST_COVERAGE_THRESHOLD)

# $(1): make target to run inside the container. ARGS and GO_TEST_TAGS go in
# as env vars, not command line overrides: an override would replace the
# target-specific values of test.coverage.
# Signals: sh defers a trap while a foreground command runs, so make runs in
# the background and wait picks the ctrl-c or job cancel up at once, the trap
# then forwards it to every process of the container. --init keeps a real
# pid 1 in front of sh, which reaps the orphans that leaves behind.
define go-docker-run
	docker run -t --rm --init \
		-e GOPROXY \
		-v "$$(pwd)":/app \
		-v $(GO_DOCKER_MOD_CACHE_VOLUME):/go/pkg/mod \
		-v $(GO_DOCKER_BUILD_CACHE_VOLUME):/root/.cache/go-build \
		-w /app \
		$(GO_DOCKER_IMAGE) \
		sh -c '$(GO_DOCKER_SETUP_CMD) || exit; \
			trap "chown -R $(shell id -u):$(shell id -g) $(ARTIFACTS_DIR) 2>/dev/null || :" EXIT; \
			trap "kill -s INT -- -1" INT; trap "kill -s TERM -- -1" TERM; \
			ARGS="$(ARGS)" GO_TEST_TAGS="$(GO_TEST_TAGS)" make $(1) & wait $$!; st=$$?; wait; exit $$st'
endef

.PHONY: test.docker
test.docker: ## Run tests in a golang image, ARGS= passes go test args
	$(call go-docker-run,test)

test.docker.%: ## Run make test.$* in a golang image, e.g. test.docker.coverage
	$(call go-docker-run,test.$*)

.PHONY: test.docker.alpine
test.docker.alpine: ## Run tests in the alpine (musl) golang image
	@$(MAKE) test.docker GO_DOCKER_IMAGE="$(GO_DOCKER_ALPINE_IMAGE)" GO_DOCKER_SETUP_CMD="$(GO_DOCKER_ALPINE_SETUP_CMD)"

test.docker.alpine.%: ## Run make test.$* in the alpine (musl) golang image, e.g. test.docker.alpine.coverage
	@$(MAKE) test.docker.$* GO_DOCKER_IMAGE="$(GO_DOCKER_ALPINE_IMAGE)" GO_DOCKER_SETUP_CMD="$(GO_DOCKER_ALPINE_SETUP_CMD)"


##@ Lint Targets


# pinned; `go run` fetches the exact version on demand
GOLANGCI_LINT_VERSION ?= 2.13.2
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_LINT_VERSION)
GOLANGCI_LINT_TIMEOUT ?= 10m
PRE_COMMIT_VERSION ?= 4.6
PRE_COMMIT ?= uvx pre-commit@$(PRE_COMMIT_VERSION)
# scripts/ipkg-make-index.sh is vendored verbatim from OpenWrt: not ours to lint
SHELLCHECK_EXCLUDE := -path ./.git -o -path ./scripts/ipkg-make-index.sh -o -path ./.cache -o -path ./releases

.PHONY: lint
lint: lint.golangci lint.shellcheck lint.pre-commit ## Run all linters

.PHONY: lint.golangci
lint.golangci: ## Run golangci-lint (ARGS=... for extra flags)
	$(GOLANGCI_LINT) run --timeout=$(GOLANGCI_LINT_TIMEOUT) --show-stats $(ARGS)

.PHONY: lint.fix
lint.fix: ## Run golangci-lint with --fix
	$(GOLANGCI_LINT) run --timeout=$(GOLANGCI_LINT_TIMEOUT) --fix --show-stats $(ARGS)

.PHONY: lint.shellcheck
lint.shellcheck: ## Run shellcheck on the repo's shell scripts
	find . \( $(SHELLCHECK_EXCLUDE) \) -prune -o -type f -name '*.sh' \
		-exec shellcheck --format=gcc -s bash {} +

.PHONY: lint.pre-commit
lint.pre-commit: ## Run the pre-commit hooks on every file
	$(PRE_COMMIT) run --all-files

# formatter chain; the module prefix is spelled out for gci and gofumpt (a
# dotless module name would otherwise pass for a std import)

.PHONY: go.format
go.format: ## Format the source code (golines, gofumpt, goimports, gci)
	go run github.com/segmentio/golines@latest --max-len=120 --no-reformat-tags --ignore-generated --write-output .
	go run mvdan.cc/gofumpt@latest -l -w -modpath $(GO_MOD_ID) .
	go run golang.org/x/tools/cmd/goimports@latest -l -w .
	go run github.com/daixiang0/gci@latest write --skip-generated -s standard -s default -s 'prefix($(GO_MOD_ID))' .


##@ Feed Targets


# config the feed commands run with (override per invocation or in local.mk)
FEED_CONFIG ?= config.yaml
# extra `build` flags: make feed FEED_ARGS="--full --sign --only sdk"
FEED_ARGS ?=

.PHONY: feed
feed: ## Build the feed (incremental, unsigned; flags via FEED_ARGS)
	$(FEEDBUILDER) -c $(FEED_CONFIG) build $(FEED_ARGS)

.PHONY: sign
sign: ## Sign the feed tree in place (Packages.sig / packages.adb + repo keys + add.sh)
	$(FEEDBUILDER) -c $(FEED_CONFIG) sign

# verifies exactly the tree publish ships ($(PUBLISH_SRC)), not the config's
# output_dir — the two may diverge
.PHONY: verify
verify: ## Validate every feed signature + repo.pub + add.sh key step
	$(FEEDBUILDER) -c $(FEED_CONFIG) verify "$(PUBLISH_SRC)"

.PHONY: howto
howto: ## Print how to add the feed on a router (no build)
	$(FEEDBUILDER) -c $(FEED_CONFIG) howto

.PHONY: serve
serve: ## Serve the feed over HTTP (config `serve` section)
	$(FEEDBUILDER) -c $(FEED_CONFIG) serve

.PHONY: genkey
genkey: ## Generate a usign keypair into keys/
	$(FEEDBUILDER) genkey

.PHONY: genkey.apk
genkey.apk: ## Generate the apk (OpenWrt 25.12+) EC keypair into keys/
	$(FEEDBUILDER) genkey --apk


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
