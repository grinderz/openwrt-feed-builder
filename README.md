# openwrt-feed-builder

Collects `.ipk` packages from HTTP sources and builds a signed opkg custom feed
for OpenWrt (24.10 / opkg layout). Raw upstream binaries (a `type: binary`
source) are repacked into locally built `.ipk`s — e.g. the mihomo binary
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
make test
```

Single static binary. Dependencies: `gopkg.in/yaml.v3`, `github.com/ulikunitz/xz`.

## Use

```sh
./openwrt-feed-builder -c config.yaml build [--refresh] [--full] [--only TYPE_OR_NAME[,...]]
./openwrt-feed-builder -c config.yaml serve
./openwrt-feed-builder genkey [--secret keys/secret.key] [--public keys/public.key]
./openwrt-feed-builder --version
```

`build` is incremental by default: it merges into the existing output tree,
skips packages whose bytes are already in place (no copy / re-index / re-sign)
and never removes anything — superseded versions accumulate until a `--full`
build rebuilds the tree from scratch (staged + atomically swapped) and prunes
them. Cached downloads are validated against the metadata the source exposes
and re-fetched on mismatch. `--only` limits the run to matching sources;
combined with the incremental default the rest of the feed stays as-is.

How a cached download / build is considered up to date, per source type:

| source type  | cache validation                                                        |
|--------------|-------------------------------------------------------------------------|
| `feed`       | size + SHA256 from the upstream `Packages` index                        |
| `github`     | asset size from the Releases API                                        |
| `github_dir` | file size from the Contents API                                         |
| `ipk`        | file exists in the cache (URL is the key; no upstream metadata)         |
| `html`       | file exists in the cache (URL is the key; no upstream metadata)         |
| `binary`     | asset cached by URL; the .ipk itself is rebuilt deterministically       |
| `sdk`        | completed-build marker per source × SDK flavor × release × target       |

Everything re-fetches/rebuilds with `--refresh`. Placed feed files are compared
by SHA256 against the routed package, so an unchanged package never touches its
feed dir.

See `config.example.yaml` for the config format.

## Remote build cycle (Makefile)

Deploy/publish plumbing lives in the `Makefile`; destinations go into
`local.mk` (gitignored, see `local.mk.example`):

```sh
make deploy         # sync this repo -> DEPLOY_DEST (build host); deploy.diff = dry run
make fetch          # pull the generated releases/ <- DEPLOY_DEST; fetch.diff = dry run
make publish        # push releases/ -> PUBLISH_DEST (web server); publish.diff = dry run
```

Typical flow when sdk sources compile on a separate build host:

```sh
make deploy                                  # code + config + keys to the build host
ssh <host> 'cd .../openwrt-feed-builder && go run ./cmd/openwrt-feed-builder build'
make fetch                                   # signed feed (releases/ only) back here
make publish                                 # feed to the web server
```

`make deploy` syncs `keys/` too — the build host builds AND signs the feed,
this machine only fetches `releases/` and publishes it, so keep `DEPLOY_DEST`
private. The host's `.cache/` and `releases/` are protected from `--delete`;
`local.mk`, `config.yaml` and `keys/` sync with the laptop copies winning.

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

## Source layout

Standard Go layout: a thin `cmd/` entry point over an `internal/feedbuilder`
package.

```
cmd/openwrt-feed-builder/main.go   # process entry: calls feedbuilder.Run
internal/feedbuilder/           # all logic (package feedbuilder)
```

| Go file (in `internal/feedbuilder/`) | role                                   |
|--------------------------------------|----------------------------------------|
| `cli.go`         | CLI: build / serve / genkey            |
| `config.go`      | YAML config load + validate            |
| `sources.go`     | resolve sources to .ipk URLs           |
| `binary.go`      | type binary: repack raw files as .ipk  |
| `sdk.go`         | type sdk: build from source (buildroot)|
| `download.go`    | retrying HTTP client + on-disk cache   |
| `ipk.go`         | ar/tar parsing of .ipk + control       |
| `index.go`       | Packages/Packages.gz + usign signing   |
| `layout.go`      | output dir layout + target detection   |
| `version.go`     | Debian/opkg version comparison         |
| `repohelpers.go` | add.sh + repo.pub generation           |
| `serve.go`       | static HTTP server                     |
| `appinfo.go`     | version constant                       |

## Notes

- `usign` (signing / `genkey`) is shelled out.
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