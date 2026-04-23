# dns-migrate

Import DNS records from **Cloudflare** into **Hetzner Cloud DNS** with a single command.

- Read-only on Cloudflare, write-only on Hetzner.
- Never deletes anything it did not itself create during the run.
- If anything fails mid-apply, every change is rolled back automatically.
- Strips Cloudflare-specific artifacts (SOA, Cloudflare nameservers, per-record comments) and clamps TTLs to a sane minimum.

## Install

Prebuilt binaries: see [Releases](https://github.com/NothingTV/cf-to-hcloud-dns/releases) (or build from source below).

### Build from source

```sh
go install github.com/NothingTV/cf-to-hcloud-dns/cmd/dns-migrate@latest
```

Or clone and build:

```sh
git clone https://github.com/NothingTV/cf-to-hcloud-dns
cd cf-to-hcloud-dns
go build -o dns-migrate ./cmd/dns-migrate
```

Cross-compile for any OS/arch supported by Go:

```sh
GOOS=linux   GOARCH=amd64 go build -o dns-migrate        ./cmd/dns-migrate
GOOS=darwin  GOARCH=arm64 go build -o dns-migrate-mac    ./cmd/dns-migrate
GOOS=windows GOARCH=amd64 go build -o dns-migrate.exe    ./cmd/dns-migrate
```

## Quick start

```sh
export CLOUDFLARE_API_TOKEN=...
export HETZNER_API_TOKEN=...

# 1. Preview the plan
dns-migrate --domain example.com --dry-run

# 2. Apply
dns-migrate --domain example.com
```

A `.env` file in the current directory is picked up automatically, or point at one explicitly with `--env-file path/to/.env`.

## Credentials

Resolution order (first non-empty wins): `--cf-token` / `--hetzner-token` flags → process env vars → `.env` file.

### Cloudflare API token

Create at <https://dash.cloudflare.com/profile/api-tokens>. Read-only is sufficient — this tool never writes to Cloudflare.

| Resource | Permission |
|---|---|
| Zone → Zone | Read |
| Zone → DNS  | Read |

Set zone resources to **Include → All zones** (simplest) or the specific zone you intend to migrate.

### Hetzner Cloud API token

This tool uses the [Hetzner Cloud API](https://docs.hetzner.cloud/reference/cloud#tag/zones) (`api.hetzner.cloud/v1`), not the older standalone DNS Console API.

Create in Hetzner Cloud Console: **Project → Security → API Tokens → Generate API token**. Permission: **Read & Write**.

The zone will be created inside the project the token belongs to.

## What the tool does

1. Fetches every DNS record from the Cloudflare zone.
2. Drops records that would conflict with Hetzner's zone management:
   - SOA (Hetzner manages its own)
   - Cloudflare's apex NS records (`*.ns.cloudflare.com`)
3. Strips per-record comments.
4. Clamps every TTL up to `--min-ttl` (default `60`). Cloudflare's "automatic" TTL of `1` becomes `60`.
5. Skips record types Hetzner Cloud DNS does not support, with a warning.
6. Groups records into RRSets by `(name, type)` — the shape Hetzner expects.
7. Creates the zone on Hetzner Cloud if it does not exist.
8. If the zone already exists, asks:
   - **`[a]dd`** — create missing RRSets; leave existing ones untouched
   - **`[o]verride`** — create missing RRSets *and* replace the content of any `(name, type)` that already exists
   - **`[c]ancel`** — abort without changes

On failure, every change made in the current run is rolled back. Pre-existing resources are never touched.

## Flags

| Flag | Default | Purpose |
|---|---|---|
| `--domain` | (required) | Zone to migrate, e.g. `example.com` |
| `--dry-run` | `false` | Print the plan; do not call Hetzner write APIs |
| `--min-ttl` | `60` | Minimum TTL enforced on every migrated record |
| `--yes` | `false` | Skip the final `Proceed?` prompt. Does **not** skip the "zone already exists" prompt |
| `--cf-token` | — | Override `CLOUDFLARE_API_TOKEN` |
| `--hetzner-token` | — | Override `HETZNER_API_TOKEN` |
| `--env-file` | `./.env` | Load env vars from this file if present |
| `-h`, `--help` | — | Show full help |

## After migration: update your nameservers

The tool does **not** change your domain's delegation at the registrar — you have to do that yourself. Once the zone is in Hetzner Cloud, point your registrar at Hetzner's nameservers:

| Name | IPv4 | IPv6 |
|---|---|---|
| `hydrogen.ns.hetzner.com.` | `213.133.100.98` | `2a01:4f8:0:1::add:1098` |
| `oxygen.ns.hetzner.com.` | `88.198.229.192` | `2a01:4f8:0:1::add:2992` |
| `helium.ns.hetzner.de.` | `193.47.99.5` | `2001:67c:192c::add:5` |

Wait for DNS propagation (up to your current NS record's TTL) before removing the Cloudflare zone.

## Limitations / non-goals

- One domain per invocation.
- Import-only — no ongoing sync.
- Cloudflare-only features (Page Rules, Workers, the orange-cloud proxy, etc.) are not migrated. Only DNS records are.
- Registrar delegation is out of scope.

## License

MIT
