# openwrt-feed-builder

Collects `.ipk` packages from HTTP sources and builds a signed opkg custom feed
for OpenWrt (24.10 / opkg layout). Raw upstream binaries (a `type: binary`
source) are repacked into locally built `.ipk`s — e.g. the mihomo binary
gunzipped to `/opt/clash/bin/clash` with mode 0755 — so plain files from the
internet install like regular packages, both via the ImageBuilder and via an
ASU server. See `../config.example.yaml`.

## Build

```sh
cd goapp
go build -o openwrt-feed-builder ./cmd/openwrt-feed-builder
```

Single static binary. Dependencies: `gopkg.in/yaml.v3`, `github.com/ulikunitz/xz`.

## Use

```sh
./openwrt-feed-builder -c ../config.yaml build [--refresh]
./openwrt-feed-builder -c ../config.yaml serve
./openwrt-feed-builder genkey [--secret keys/secret.key] [--public keys/public.key]
./openwrt-feed-builder --version
```

See `../config.example.yaml` for the config format.

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


## Sync

```
go run cmd/openwrt-feed-builder/main.go -c config.yaml build
```