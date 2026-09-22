# openwrt-feed-builder

Collects OpenWrt packages from HTTP sources and builds a signed custom feed —
opkg (`.ipk`, `Packages`) for 24.10 and older, apk (`.apk`, `packages.adb`)
for 25.12 and newer, several branches side by side in one tree (see
[OpenWrt 25.12 / apk](#openwrt-2512--apk)). Raw upstream binaries (a `type: binary`
source) are repacked into locally built packages — e.g. the mihomo binary
gunzipped to `/opt/clash/bin/clash` with mode 0755 — so plain files from the
internet install like regular packages, both via the ImageBuilder and via an
ASU server. A `type: sdk` source compiles packages from source with the
OpenWrt SDK through an [openwrt-buildroot](../openwrt-buildroot) checkout —
for things nobody ships as binaries, like the AmneziaWG kernel module. See
`config.example.yaml`.

The sdk source drives the buildroot's `make pkg.build` (or `pkg.sdk.build`
with `builder: src` for a src-built SDK): all sdk sources sharing a buildroot,
SDK flavor and release × target are batched into one docker container run,
the SDK workdir persists in a named volume between runs and compiles go
through ccache, so a warm rebuild takes seconds-to-minutes instead of the
~half-hour cold start. Requires docker and GNU make ≥ 4.4 on the build host.

Built packages route like the official mirror: kmods (exact kernel dependency)
go under their release's `<release>/targets/<t>/<st>/kmods/<kernel>/`, plain
userspace into the branch-wide `packages-<branch>/<arch>/` feed shared by all
point releases; `release_bound: true` on a source pins its userspace to the
built release too (for tools that must ship in lockstep with their kmod).

## Build

```sh
make build     # go build -> ./openwrt-feed-builder
make test              # gotestsum, -race (ARGS=... for go test flags)
make test.repeat       # every test twice in one process, shuffled order
make test.coverage     # + artifacts/coverage.{out,html}, GO_TEST_COVERAGE_THRESHOLD
make test.docker       # the same in a golang image (test.docker.coverage, ...)
make test.docker.alpine  # musl image with apk-tools + shellcheck: nothing skipped
make lint      # golangci-lint + shellcheck + pre-commit hooks (incl. gitleaks)
make lint.fix  # golangci-lint --fix
make go.format # golines + gofumpt + goimports + gci
```

Lint setup: `.golangci.yml` enables all linters (`default: all`) except a few
opinionated ones and `forbidigo` (a CLI prints with `fmt.Print*`); the
complexity limits are raised (cyclop 30, gocognit 50, funlen 150 lines / 60
statements, nestif 8) and the add.sh templates are exempt from dupword/lll. golangci-lint is pinned via `go run`
(`GOLANGCI_LINT=golangci-lint` to use a local binary), pre-commit runs through
`uvx`. The generated `add.sh` scripts are shellchecked by the test suite when
shellcheck is installed.

Single static binary. Dependencies: `github.com/spf13/cobra`, `gopkg.in/yaml.v3`,
`github.com/ulikunitz/xz`.
Feeds for 25.12+ branches additionally need apk-tools ≥ 3 and fakeroot on the
build host (see below).

## Use

```sh
./openwrt-feed-builder -c config.yaml build [--refresh] [--full] [--sign] [--reindex] [--index-script PATH] [--only TYPE_OR_NAME[,...]]
./openwrt-feed-builder -c config.yaml indexdiff [--script scripts/ipkg-make-index.sh]
./openwrt-feed-builder -c config.yaml sign     # (re)sign an existing tree in place
./openwrt-feed-builder -c config.yaml verify   # validate every signature + repo.pub
./openwrt-feed-builder -c config.yaml howto    # print how to add the feed on a router
./openwrt-feed-builder -c config.yaml serve
./openwrt-feed-builder genkey [--secret keys/secret.key] [--public keys/public.key]
./openwrt-feed-builder genkey --apk [--secret keys/apk-private.pem] [--public keys/apk-public.pem]
./openwrt-feed-builder --version
./openwrt-feed-builder help <command>    # every command has --help
```

`-c/--config` (default `config.yaml`) goes anywhere on the command line.
Output is coloured yay-style (`::` sections, `==>` sources, yellow `WARNING:`,
red `ERROR:`) when stdout is a terminal; `--color` / `--no-color` force it,
`NO_COLOR` or `TERM=dumb` turn it off.
Ctrl-C / SIGTERM cancel running downloads and SDK builds and stop `serve`
gracefully.

`build` writes an UNSIGNED tree by default (an unsigned rebuild also drops the
now-stale `Packages.sig` from every dir it touches). Signing is either opt-in
per build (`--sign`) or a separate step: `sign` signs every `Packages` index
under `output_dir` and regenerates `repo.pub` / `add.sh` with the key baked
in, touching nothing else — it also covers trees built on a host without the
keys and re-signing after a key rotation. `verify` is the read-only check:
every `Packages` must have a `Packages.sig` that verifies against
`sign.public_key` and the served `repo.pub` must match that key; non-zero
exit otherwise (fits a pre-`publish` hook).

`build` is incremental by default: it merges into the existing output tree and
skips packages whose bytes are already in place (no copy / re-index / re-sign).
When a package ships a newer version, its older `.ipk` files in the same dir
are pruned and the index rebuilt; packages no longer produced by any source
stay until a `--full` build rebuilds the tree from scratch (staged +
atomically swapped). Cached downloads are validated against the metadata the source exposes
and re-fetched on mismatch. `--only` limits the run to matching sources;
combined with the incremental default the rest of the feed stays as-is.
`--reindex` regenerates the index of every feed dir, not just the touched
ones — use after an index-format change.

The `Packages` indexes are normally generated natively. For debugging there
are two escape hatches built on the official OpenWrt generator (vendored
verbatim from the `openwrt-24.10` branch as `scripts/ipkg-make-index.sh`; the
builder shims its `mkhash` / GNU `stat` host-tool dependencies, so it runs on
macOS too): `build --index-script scripts/ipkg-make-index.sh` builds the tree
with the official script instead, and `indexdiff` regenerates every index
both ways in memory and prints a unified diff per feed dir (disk untouched).
Expected deviations of the script: control fields pass through unstripped
(`Source*`, `Maintainer` — the native indexer drops them because opkg's
prefix-matching parser corrupts its package blob on `SourceName` /
`SourceDateEpoch`), no `MD5Sum`, and packages named `kernel` / `libc` are
skipped.

How a cached download / build is considered up to date, per source type:

| source type  | cache validation                                                        |
|--------------|-------------------------------------------------------------------------|
| `feed`       | size + SHA256 from `Packages`; size only from `packages.adb`            |
| `github`     | asset size from the Releases API                                        |
| `github_dir` | file size from the Contents API                                         |
| `ipk`        | file exists in the cache (URL is the key; no upstream metadata)         |
| `html`       | file exists in the cache (URL is the key; no upstream metadata)         |
| `binary`     | asset cached by URL; the .ipk/.apk is rebuilt deterministically         |
| `sdk`        | completed-build marker per source × SDK flavor × release × target       |

Everything re-fetches/rebuilds with `--refresh`. Placed feed files are compared
by SHA256 against the routed package, so an unchanged package never touches its
feed dir.

Assets whose file name already rules them out are not downloaded at all: a
name carrying an architecture outside `architectures:` or a target outside
`targets:` (`<pkg>_<ver>_<arch>[_<target>_<subtarget>]`), or an extension no
carried branch uses (`.apk` without a 25.12+ release, `.ipk` for a source
pinned to 25.12). Names without a recognizable architecture are downloaded and
filtered by their metadata as before.

The cache does not grow forever: after a build that visited every source (no
`--only`, no failed source or index) every entry the run did not use is
removed — downloads of URLs no source resolves to any more, repacked binaries
and UPX results of old versions, sdk builds of combinations no longer
configured. Only file names the builder itself creates are touched.

See `config.example.yaml` for the config format.

## Build cycle (Makefile)

Deploy/publish plumbing lives in the `Makefile`; destinations go into
`local.mk` (gitignored, see `local.mk.example`):

```sh
make feed           # build the feed (FEED_ARGS="--full --sign --only sdk" for flags)
make sign           # sign the tree in place
make verify         # validate signatures (publish runs it automatically)
make howto          # print how to add the feed on a router
make serve          # serve the feed over HTTP
make genkey         # generate the usign keypair
make genkey.apk     # generate the apk (25.12+) EC keypair
make deploy         # sync this repo -> DEPLOY_DEST (build host); deploy.diff = dry run
make fetch          # pull the generated releases/ <- DEPLOY_DEST; fetch.diff = dry run
make publish        # push releases/ -> PUBLISH_DEST (web server); publish.diff = dry run
```

Building on this machine (the keys are here, so the build can sign right away):

```sh
make feed FEED_ARGS="--sign"   # build + sign (or: make feed && make sign)
make verify                    # every index / key / add.sh ok
make publish.diff              # what would change on the web server
make publish                   # runs verify again, then pushes releases/
make howto                     # the commands / feed lines for the routers
```

Needs usign (24.10 signing), apk-tools >= 3 (25.12 indexes and signing),
fakeroot (`binary` sources as .apk), upx (sources with `upx:`) and go; docker
and GNU make >= 4.4 only for `sdk` sources.

When sdk sources compile on a separate build host:

```sh
make deploy                                  # code + config to the build host
ssh <host> 'cd .../openwrt-feed-builder && go run ./cmd/openwrt-feed-builder build'
make fetch                                   # unsigned feed (releases/ only) back here
make sign                                    # sign it with the local keys
make verify
make publish                                 # feed to the web server
```

`make deploy` never sends `keys/` — the usign and apk secrets stay on this
machine; the build host produces an unsigned tree and `sign` runs here after
`fetch`.
The host's `.cache/`, `releases/` and `keys/` are protected from `--delete`;
`local.mk` and `config.yaml` sync with the laptop copies winning.

## Signing (usign)

`genkey` and feed signing shell out to `usign`. Install it first.

- Debian/Ubuntu: `apt install signify-openbsd` (binary may be `signify-openbsd`)
- OpenWrt itself: `opkg install usign`
- macOS (no Homebrew formula) — build from source; needs only a C compiler,
  no cmake:

  ```sh
  git clone --depth 1 https://git.openwrt.org/project/usign.git
  cd usign
  cc -O2 -std=gnu99 -o usign ed25519.c edsign.c f25519.c fprime.c sha512.c main.c base64.c
  mkdir -p ~/.local/bin && cp usign ~/.local/bin/    # ensure ~/.local/bin is on PATH
  ```

Then generate a keypair and enable signing:

```sh
openwrt-feed-builder genkey            # writes keys/secret.key, keys/public.key, prints the key id
```

```yaml
sign:
  enabled: true
  secret_key: ./keys/secret.key
  public_key: ./keys/public.key
```

The secret key can also come from a command instead of a file — its stdout is
the key, staged in a private temp file only for the build. Set either
`secret_key` or `secret_key_cmd`, not both:

```yaml
sign:
  enabled: true
  secret_key_cmd: "pass show openwrt/feed-secret-key"
  public_key: ./keys/public.key
```

The generated `add.sh` installs the public key on the router
(`/etc/opkg/keys/<key-id>`), so a signed feed passes opkg's `check_signature`
without touching the router's global setting.

## OpenWrt 25.12 / apk

25.12 replaced opkg with apk-tools v3: packages are `.apk` files in the ADB
format, every feed dir carries one `packages.adb` index with the signature
embedded, routers list feeds in `/etc/apk/repositories.d/customfeeds.list`
(one `.../packages.adb` URL per line) and trust PEM keys from
`/etc/apk/keys/`. The directory layout is the same as before
(`packages-25.12/<arch>/<feed>/`, `25.12.x/targets/<t>/<st>/{packages,kmods/<kernel>}/`).

`layout.version` may list releases of several branches; each branch gets its
own tree in its own format:

```yaml
layout:
  version: ["24.10.8", "25.12.5"]
```

Which branch a package lands in:

1. the branch of its source's release — a github tag matching a
   `layout.version` entry (`tags: [v24.10.8, v25.12.5]` routes each tag's
   assets into its release), `kmod_version:`, or an sdk build's release;
2. else the source's `openwrt: "25.12"` pin;
3. else every carried branch of the package's format: `.ipk` files go to the
   opkg branches, `.apk` files to the apk branches. A package that fits no
   carried branch is skipped with a note.

Source file patterns (`asset_match`, `pattern`, a `github_dir` URL without a
glob) default to the formats the branches need: `*.ipk`, `*.apk` or
`*.[ai]pk`. A `feed` source reads `packages.adb` as well as `Packages[.gz]`.
`binary` sources are packaged once per format (`.apk` version
`<version>-r<revision>`; the version must be a valid apk version), `sdk`
sources harvest whatever the SDK of each release produced.

Package metadata is read natively; writing ADB shells out to apk-tools v3
(`apk_tool:` in the config, default `apk` on PATH):

| step                   | tool                                       |
|------------------------|--------------------------------------------|
| index                  | `apk mkndx` (unsigned)                     |
| signing (`sign`, `--sign`) | `apk adbsign --sign-key <apk key>`     |
| `verify`               | `apk verify` against `sign.apk_public_key` |
| `binary` sources       | `fakeroot apk mkpkg` (files owned by root) |

Get it from Arch `pacman -S apk-tools`, Alpine ≥ 3.23, or use an OpenWrt 25.12
SDK's `staging_dir/host/bin/apk`. On the build host that means apk-tools +
fakeroot next to docker; the laptop that signs needs apk-tools too.

apk signs with an ECDSA P-256 key, not usign:

```sh
openwrt-feed-builder genkey --apk      # keys/apk-private.pem + keys/apk-public.pem
```

```yaml
sign:
  enabled: true
  secret_key: ./keys/secret.key          # usign, opkg branches
  public_key: ./keys/public.key
  apk_secret_key: ./keys/apk-private.pem # EC, apk branches (or apk_secret_key_cmd)
  apk_public_key: ./keys/apk-public.pem
```

The public key is served as `repo-apk.pem`; the 25.12 `<release>/add.sh`
installs it as `/etc/apk/keys/<feed_prefix>.pem`, writes the feed URLs into
`/etc/apk/repositories.d/customfeeds.list` and runs `apk update`. An unsigned
apk feed needs `apk --allow-untrusted`.

## Source layout

Standard Go layout: a thin `cmd/` entry point, the cobra command tree in
`internal/cli`, all logic in `internal/feedbuilder`.

```
cmd/openwrt-feed-builder/main.go   # signal context, runs cli.NewRoot()
internal/cli/                      # cobra commands: flags -> feedbuilder calls
internal/feedbuilder/              # all logic (package feedbuilder)
```

| Go file (in `internal/feedbuilder/`) | role                                   |
|--------------------------------------|----------------------------------------|
| `build.go`       | the build command, one method per step |
| `commands.go`    | sign / verify / howto / serve / genkey / indexdiff + helpers |
| `errors.go`      | sentinel errors                        |
| `osutil.go`      | exec / file / cleanup helpers          |
| `config.go`      | YAML config load + validate            |
| `sources.go`     | resolve sources to .ipk/.apk URLs      |
| `binary.go`      | type binary: repack raw files as .ipk/.apk |
| `sdk.go`         | type sdk: build from source (buildroot)|
| `download.go`    | retrying HTTP client + on-disk cache   |
| `ipk.go`         | ar/tar parsing of .ipk + control       |
| `index.go`       | Packages/Packages.gz + usign signing   |
| `apk.go`         | ADB (.apk / packages.adb) reader + apk-tools wrappers |
| `layout.go`      | output dir layout + target detection   |
| `version.go`     | Debian/opkg version comparison         |
| `repohelpers.go` | add.sh + repo.pub / repo-apk.pem       |
| `serve.go`       | static HTTP server                     |
| `appinfo.go`     | version constant                       |

## Notes

- `usign` (signing / `genkey`) and apk-tools v3 (apk index, signing, verify,
  `binary` .apk packaging) are shelled out; the apk key is generated natively.
- HTTP downloads use a small retry loop in `download.go` (4 retries, exponential
  backoff, retries 429/500/502/503/504).
- Sources are kept untyped (`map[string]any`) so each resolver reads only the
  fields it needs.
- `control.tar.{gz,xz}` and uncompressed `control.tar` are all decompressed
  (xz via `github.com/ulikunitz/xz`, a pure-Go reader).
- sdk sources write their merged feeds-extra file into the buildroot under
  `<artifacts_dir>/feedbuilder/` (cleaned there with `make pkg.clean.feedbuilder`)
  and read build output from `<artifacts_dir>/pkg/<release>/tmp`;
  `artifacts_dir` defaults to `artifacts`.
