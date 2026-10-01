package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/billing"
	"llm-gateway/internal/console"
	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"
	"llm-gateway/migrations"
)

// The console has two entry points: `serve` (the default) and `bootstrap`.
func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	profile, err := appenv.FromEnv()
	if err != nil {
		log.Fatalf("runtime profile: %v", err)
	}
	appenv.Log(profile)

	checks := consoleChecks(appenv.EnvLookup)
	if err := appenv.Validate(profile, checks); err != nil {
		log.Fatalf("console production preflight failed:\n%v", err)
	}
	if !profile.IsProduction() {
		warnFailedChecks(checks)
	}

	command, args := splitCommand(os.Args[1:])
	switch command {
	case "serve":
		err = runServe(profile, args)
	case "bootstrap":
		err = runBootstrap(profile, args)
	default:
		log.Fatalf("console: unknown command %q (want serve or bootstrap)", command)
	}
	if err != nil {
		log.Fatalf("console %s: %v", command, err)
	}
}

// splitCommand treats a leading non-flag argument as the subcommand. Running
// `console` with no argument serves; running `console --help` still lands in
// serve, whose flag set explains the options.
func splitCommand(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "serve", args
	}
	return args[0], args[1:]
}

// runServe starts the HTTP service: own pool, schema check, Hertz engine on the
// business listener, optional separate diagnostics listener, graceful shutdown.
func runServe(profile appenv.Profile, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	cfg, err := loadConfig(profile)
	if err != nil {
		return err
	}
	db, err := openDB(cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// Never auto-migrate: an out-of-date schema must be fixed explicitly with
	// `migrate up` (migrations.Check is read-only).
	if err := migrations.Check(db); err != nil {
		return err
	}

	users := iam.NewService(db, iam.Options{
		SessionTTL:         cfg.SessionTTL,
		LoginRatePerMinute: cfg.LoginRate,
		LoginBurst:         cfg.LoginBurst,
	})
	orgs := organization.NewService(db)

	// T03: mutations are audited into console_audit_events, and GET
	// /api/v1/orgs/{org}/audit reads them back. One sink implements both roles.
	audit := console.NewPGSink(db)
	audit.Logger = log.Default()

	// T03: the Console → Control AdminService channel (keys + policy
	// publication). It is optional — without CONTROL_ADDR key endpoints answer
	// 503 and no publication loop runs — but it never blocks startup: the loop
	// converges once Control returns.
	channel, err := buildControlChannel(cfg, db, orgs, log.Default())
	if err != nil {
		return err
	}
	defer func() { _ = channel.Close() }()

	// T05 (ADR-0004): one billing store serves both the platform price surface
	// and the organization account/ledger surface. It shares the Console pool.
	billingStore := billing.NewPGStore(db)

	// T07C: the monthly statement is a live aggregation over
	// billing_entries.posted_at, and the money-exception queue is the durable
	// PostgreSQL table migration 000011 adds (the Redis DLQ stays a diagnostic
	// cache). Both share the Console pool.
	return serve(serveOptions{
		config:              cfg,
		profile:             profile,
		iam:                 users,
		orgs:                orgs,
		keys:                channel.Keys(),
		usage:               console.NewPGUsageStore(db),
		prices:              billingStore,
		accounts:            billingStore,
		statements:          billing.NewPGStatementStore(db),
		exceptions:          billing.NewPGExceptionStore(db),
		publication:         channel.Publication(),
		publicationInterval: cfg.PublicationInterval,
		audit:               audit,
		ready:               db.PingContext,
		logger:              log.Default(),
	})
}

// auditService is the Console audit dependency: the write side (AuditSink) and
// the read side (AuditStore). console.PGSink implements both.
type auditService interface {
	console.AuditSink
	console.AuditStore
}

// publicationRunner is the background policy-publication loop started by serve.
// *organization.PublicationWorker implements it; tests use a fake to observe the
// start/stop lifecycle without PostgreSQL.
type publicationRunner interface {
	Run(ctx context.Context, interval time.Duration)
}

// serveOptions carries the wired dependencies into serve(). The service fields
// are the console interfaces, so the wiring is testable with fakes.
type serveOptions struct {
	config  config
	profile appenv.Profile
	iam     console.IAMService
	orgs    console.OrganizationService
	// keys is optional: nil (no CONTROL_ADDR) makes the key endpoints answer 503
	// instead of pretending an organization has no keys.
	keys console.KeyService
	// usage backs GET /api/v1/orgs/{org}/usage. nil answers 500.
	usage console.UsageStore
	// prices and accounts back the T05 billing surface (ADR-0004). nil answers
	// 503 instead of pretending the platform has no prices and no ledger.
	prices   console.PriceService
	accounts console.BillingAccountService
	// statements backs GET /api/v1/orgs/{org}/billing/statements and exceptions
	// backs the durable money-exception queue (T07C). nil answers 503.
	statements console.StatementService
	exceptions console.MoneyExceptionService
	// publication is optional: nil disables the policy-publication loop.
	publication         publicationRunner
	publicationInterval time.Duration
	// audit is optional: nil disables the audit trail and makes
	// GET /api/v1/orgs/{org}/audit answer 500 instead of pretending there is no
	// audit history.
	audit  auditService
	ready  func(ctx context.Context) error
	logger *log.Logger
}
