package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/NothingTV/cf-to-hcloud-dns/internal/cloudflare"
	"github.com/NothingTV/cf-to-hcloud-dns/internal/dotenv"
	"github.com/NothingTV/cf-to-hcloud-dns/internal/hetzner"
	"github.com/NothingTV/cf-to-hcloud-dns/internal/migrate"
)

const helpText = `dns-migrate — import DNS records from Cloudflare into Hetzner DNS

USAGE
  dns-migrate --domain <domain> [--dry-run] [--min-ttl 60] [--yes]

FLAGS
  --domain         Zone name to migrate, e.g. example.com (required)
  --dry-run        Print the plan; do not call Hetzner write APIs
  --min-ttl        Minimum TTL for every migrated record (default 60).
                   Cloudflare's "automatic" TTL of 1 is clamped up to this value.
  --yes            Skip the final "Proceed?" confirmation. Does NOT skip
                   the "zone already exists" prompt — that one always asks.
  --cf-token       Overrides the CLOUDFLARE_API_TOKEN env var
  --hetzner-token  Overrides the HETZNER_API_TOKEN env var
  --env-file       Path to a .env file to load before reading env vars
                   (default: ".env" in the current directory, if present)
  -h, --help       Show this help and exit

CREDENTIALS
  Resolution order (highest wins):
    1. CLI flags (--cf-token, --hetzner-token)
    2. Process environment variables
    3. Values from --env-file (or ./.env)

  Variables:
    CLOUDFLARE_API_TOKEN   Cloudflare API token (see permissions below)
    HETZNER_API_TOKEN      Hetzner DNS API token

  Real env vars override .env; .env never overrides an already-set variable.

CLOUDFLARE TOKEN PERMISSIONS
  Create the token at https://dash.cloudflare.com/profile/api-tokens.
  Required permissions (read-only is sufficient — this tool never writes
  to Cloudflare):

    Zone  →  Zone   →  Read
    Zone  →  DNS    →  Read

  Zone resources: "Include — All zones from an account" (simplest) or
  the specific zone you intend to migrate.

HETZNER CLOUD TOKEN
  This tool uses the Hetzner Cloud API (api.hetzner.cloud/v1) and its
  Zones / RRSets endpoints. Create the token in Hetzner Cloud Console:

    Project  →  Security  →  API Tokens  →  Generate API token

  Required permission: Read & Write.
  The token is project-scoped — the zone will be created inside the
  project the token belongs to.

WHAT THIS TOOL DOES
  • Fetches every DNS record from the Cloudflare zone.
  • Strips the Cloudflare SOA record — Hetzner manages its own.
  • Strips Cloudflare's apex NS records (*.ns.cloudflare.com).
  • Strips per-record Cloudflare comments.
  • Clamps every TTL up to --min-ttl (default 60).
  • Skips record types Hetzner does not support, with a warning.
  • Creates the zone on Hetzner if it does not exist.
  • If the zone already exists on Hetzner, asks whether to [a]dd only
    new records, [o]verride matching (name, type) pairs, or [c]ancel.
  • Never deletes anything it did not itself create during the run.
  • If anything fails during apply, rolls back every change made in
    this run before exiting.

EXAMPLES
  # Dry run to preview the plan
  export CLOUDFLARE_API_TOKEN=...
  export HETZNER_API_TOKEN=...
  dns-migrate --domain example.com --dry-run

  # Apply with explicit confirmation
  dns-migrate --domain example.com

  # Unattended apply (still prompts if the zone already exists)
  dns-migrate --domain example.com --yes
`

type stdPrompter struct {
	in  *bufio.Reader
	out *os.File
}

func (p *stdPrompter) ExistingZoneChoice(zone string, n int) (migrate.Choice, error) {
	fmt.Fprintf(p.out, `
Zone %s already exists on Hetzner with %d record(s).

  [a]dd       — create records missing from Hetzner; skip exact duplicates; do not touch anything else
  [o]verride  — create missing records AND overwrite content of records matching on (name, type); leave unrelated records alone
  [c]ancel    — abort without changes

`, zone, n)
	for {
		fmt.Fprint(p.out, "Choice [a/o/c]: ")
		line, err := p.in.ReadString('\n')
		if err != nil {
			return 0, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "a", "add":
			return migrate.ChoiceAdd, nil
		case "o", "override":
			return migrate.ChoiceOverride, nil
		case "c", "cancel", "":
			return migrate.ChoiceCancel, nil
		}
		fmt.Fprintln(p.out, "Please answer a, o, or c.")
	}
}

func (p *stdPrompter) Confirm(prompt string) (bool, error) {
	fmt.Fprintf(p.out, "%s [y/N]: ", prompt)
	line, err := p.in.ReadString('\n')
	if err != nil {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

func main() {
	fs := flag.NewFlagSet("dns-migrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		domain    = fs.String("domain", "", "domain to migrate")
		dryRun    = fs.Bool("dry-run", false, "print the plan without applying")
		minTTL    = fs.Int("min-ttl", 60, "minimum TTL")
		yes       = fs.Bool("yes", false, "skip the Proceed? prompt")
		cfToken   = fs.String("cf-token", "", "Cloudflare API token (overrides env)")
		hzToken   = fs.String("hetzner-token", "", "Hetzner API token (overrides env)")
		envFile   = fs.String("env-file", "", "path to a .env file (default: ./.env if present)")
		showHelp  = fs.Bool("help", false, "show help")
		showHelpH = fs.Bool("h", false, "show help")
	)
	fs.Usage = func() { fmt.Fprint(os.Stderr, helpText) }

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *showHelp || *showHelpH {
		fmt.Print(helpText)
		return
	}
	envPath := *envFile
	envRequired := envPath != ""
	if envPath == "" {
		envPath = ".env"
	}
	if err := dotenv.Load(envPath, envRequired); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	if *domain == "" {
		fmt.Fprintln(os.Stderr, "error: --domain is required")
		fmt.Fprintln(os.Stderr, "run with --help for usage.")
		os.Exit(2)
	}

	cfTok := firstNonEmpty(*cfToken, os.Getenv("CLOUDFLARE_API_TOKEN"))
	hzTok := firstNonEmpty(*hzToken, os.Getenv("HETZNER_API_TOKEN"))
	if cfTok == "" {
		fmt.Fprintln(os.Stderr, "error: Cloudflare token missing (set CLOUDFLARE_API_TOKEN or pass --cf-token)")
		os.Exit(2)
	}
	if hzTok == "" {
		fmt.Fprintln(os.Stderr, "error: Hetzner token missing (set HETZNER_API_TOKEN or pass --hetzner-token)")
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	runner := &migrate.Runner{
		CF:       cloudflare.New(cfTok),
		HZ:       hetzner.New(hzTok),
		Prompter: &stdPrompter{in: bufio.NewReader(os.Stdin), out: os.Stdout},
		Out:      os.Stdout,
		Opts: migrate.Options{
			Domain: *domain,
			MinTTL: *minTTL,
			DryRun: *dryRun,
			Yes:    *yes,
		},
	}

	if err := runner.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
