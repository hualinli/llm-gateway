package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/console"
	"llm-gateway/internal/controlclient"
	"llm-gateway/internal/organization"
)

// controlChannel is the Console → Control AdminService wiring (ADR-0003 §2.2):
// the typed gRPC client, the key-management service the HTTP layer consumes and
// the policy-publication worker. It is built once at startup and shared by every
// request and by the background loop.
//
// The channel is optional: without CONTROL_ADDR the Console still serves IAM and
// organization traffic, the key endpoints answer 503 unavailable, and no
// publication loop runs. That is the documented local-demo posture, not a silent
// degradation: the startup log says so and production preflight requires the
// address.
type controlChannel struct {
	client      *controlclient.Client
	keys        console.KeyService
	publication *organization.PublicationWorker
}

// Close releases the gRPC connection. It is safe on the disabled channel.
func (c *controlChannel) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Close()
}

// Keys returns the key-management service, or nil when the channel is disabled
// (the HTTP layer then answers 503 instead of pretending there are no keys).
func (c *controlChannel) Keys() console.KeyService {
	if c == nil {
		return nil
	}
	return c.keys
}

// Publication returns the policy-publication worker, or nil when disabled.
func (c *controlChannel) Publication() publicationRunner {
	if c == nil || c.publication == nil {
		return nil
	}
	return c.publication
}

// buildControlChannel assembles the channel from the environment. It never
// blocks on connectivity: Control may be down while the Console serves traffic,
// and the publication worker converges with bounded retries once it returns.
func buildControlChannel(cfg config, db *sql.DB, orgs console.OrganizationService, logger *log.Logger) (*controlChannel, error) {
	if logger == nil {
		logger = log.Default()
	}
	address := strings.TrimSpace(cfg.ControlAddr)
	if address == "" {
		logger.Printf("console: CONTROL_ADDR is not set; API key management and policy publication are disabled")
		return &controlChannel{}, nil
	}
	tlsConfig, err := controlclient.TLSConfigFromEnv(appenv.EnvLookup)
	if err != nil {
		return nil, fmt.Errorf("console control channel: %w", err)
	}
	if tlsConfig == nil {
		logger.Printf("WARNING: CONTROL_TLS_* is not set; the console -> control channel at %s is plaintext (local demo only)", address)
	}
	client, err := controlclient.Dial(address, controlclient.Options{
		TLS:                tlsConfig,
		ConsoleServiceName: strings.TrimSpace(cfg.ControlConsoleServiceName),
		Logger:             logger,
	})
	if err != nil {
		return nil, fmt.Errorf("console control channel: %w", err)
	}
	publisher, err := organization.NewControlPublisher(client,
		organization.NewOrganizationPolicySource(db),
		organization.ControlPublisherOptions{Logger: logger})
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("console control channel: %w", err)
	}
	logger.Printf("console: control channel enabled addr=%s console_service=%q tls=%t",
		address, strings.TrimSpace(cfg.ControlConsoleServiceName), tlsConfig != nil)
	return &controlChannel{
		client: client,
		keys:   console.NewControlKeyService(orgs, client, console.ControlKeyServiceOptions{}),
		publication: organization.NewPublicationWorker(db, publisher, organization.PublicationOptions{
			Logger: logger,
		}),
	}, nil
}

// publicationStopTimeout bounds how long shutdown waits for an in-flight
// publication round. A round is a bounded batch of gRPC calls, each with its own
// deadline, so this only has to cover one batch.
const publicationStopTimeout = 5 * time.Second
