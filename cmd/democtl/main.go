package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultKeyFile = "data/demo-api-key"

type options struct {
	controlURL     string
	gatewayURL     string
	meteringURL    string
	adminToken     string
	primaryURL     string
	backupURL      string
	tenant         string
	keyFile        string
	expectProvider string
	expectStatus   int
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: democtl configure|request|ensure-master-key|credential-create|provider-apply|credential-revoke [flags]")
	}
	command := os.Args[1]
	opts := options{
		controlURL:   "http://localhost:50052",
		gatewayURL:   "http://localhost:8090",
		meteringURL:  "http://localhost:8091",
		adminToken:   envOr("CONTROL_ADMIN_TOKEN", "demo-control-token"),
		primaryURL:   "http://host.docker.internal:18080",
		backupURL:    "http://host.docker.internal:18081",
		tenant:       "tenant-demo",
		keyFile:      defaultKeyFile,
		expectStatus: http.StatusOK,
	}
	if command == "configure" || command == "request" {
		flags := flag.NewFlagSet(command, flag.ExitOnError)
		flags.StringVar(&opts.controlURL, "control", opts.controlURL, "control HTTP base URL")
		flags.StringVar(&opts.gatewayURL, "gateway", opts.gatewayURL, "gateway HTTP base URL")
		flags.StringVar(&opts.meteringURL, "metering", opts.meteringURL, "metering HTTP base URL")
		flags.StringVar(&opts.adminToken, "admin-token", opts.adminToken, "control admin token")
		flags.StringVar(&opts.primaryURL, "primary-url", opts.primaryURL, "primary provider base URL as seen by Gateway")
		flags.StringVar(&opts.backupURL, "backup-url", opts.backupURL, "backup provider base URL as seen by Gateway")
		flags.StringVar(&opts.tenant, "tenant", opts.tenant, "tenant for the generated API key")
		flags.StringVar(&opts.keyFile, "key-file", opts.keyFile, "file used to store the one-time API key")
		flags.StringVar(&opts.expectProvider, "expect-provider", "", "response content expected from this provider")
		flags.IntVar(&opts.expectStatus, "expect-status", opts.expectStatus, "expected Gateway HTTP status")
		_ = flags.Parse(os.Args[2:])
	}

	client := &http.Client{Timeout: 15 * time.Second}
	var err error
	switch command {
	case "configure":
		err = configure(client, opts)
	case "request":
		err = request(client, opts)
	case "ensure-master-key":
		err = ensureMasterKey(os.Args[2:])
	case "credential-create":
		err = credentialCreate(client, opts, os.Args[2:])
	case "provider-apply":
		err = providerApply(client, opts, os.Args[2:])
	case "credential-revoke":
		err = credentialRevoke(client, opts, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		fatalf("%s failed: %v", command, err)
	}
}

func configure(client *http.Client, opts options) error {
	payload := map[string]any{
		"providers": map[string]any{
			"primary": map[string]any{"id": "primary", "protocol": "openai-compatible", "url": opts.primaryURL},
			"backup":  map[string]any{"id": "backup", "protocol": "openai-compatible", "url": opts.backupURL},
		},
		"routes": map[string]any{"default": []string{"primary", "backup"}},
	}
	var config struct {
		DesiredVersion int64 `json:"desired_version"`
	}
	if err := jsonRequest(client, http.MethodPost, opts.controlURL+"/config", opts.adminToken, payload, &config); err != nil {
		return fmt.Errorf("submit providers and route: %w", err)
	}
	if config.DesiredVersion == 0 {
		return errors.New("control returned desired_version=0")
	}
	if err := waitApplied(client, opts, config.DesiredVersion, 15*time.Second); err != nil {
		return err
	}

	var created struct {
		RawKey string `json:"raw_key"`
		Key    struct {
			KeyID string `json:"key_id"`
		} `json:"key"`
	}
	if err := jsonRequest(client, http.MethodPost, opts.controlURL+"/keys", opts.adminToken, map[string]string{"tenant_id": opts.tenant}, &created); err != nil {
		return fmt.Errorf("create tenant API key: %w", err)
	}
	if created.RawKey == "" {
		return errors.New("credential service returned an empty raw_key")
	}
	var afterKey struct {
		DesiredVersion int64 `json:"desired_version"`
	}
	if err := jsonRequest(client, http.MethodGet, opts.controlURL+"/config", opts.adminToken, nil, &afterKey); err != nil {
		return fmt.Errorf("read config after key creation: %w", err)
	}
	if err := waitApplied(client, opts, afterKey.DesiredVersion, 15*time.Second); err != nil {
		return err
	}
	if err := writeSecret(opts.keyFile, created.RawKey); err != nil {
		return err
	}
	if err := waitReady(client, opts.gatewayURL+"/readyz", 15*time.Second); err != nil {
		return err
	}
	fmt.Printf("PASS config applied desired_version=%d\n", afterKey.DesiredVersion)
	fmt.Printf("PASS tenant key created tenant=%s key_id=%s file=%s\n", opts.tenant, created.Key.KeyID, opts.keyFile)
	fmt.Printf("Client base URL: %s/v1\n", strings.TrimRight(opts.gatewayURL, "/"))
	return nil
}

func request(client *http.Client, opts options) error {
	rawKey, err := os.ReadFile(opts.keyFile)
	if err != nil {
		return fmt.Errorf("read API key %s (run demo-config first): %w", opts.keyFile, err)
	}
	key := strings.TrimSpace(string(rawKey))
	before, err := ledgerCount(client, opts.meteringURL)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"model": "default", "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "reply from the configured provider"}},
	})
	req, err := http.NewRequest(http.MethodPost, opts.gatewayURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("chat request: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != opts.expectStatus {
		return fmt.Errorf("status=%d want=%d body=%s", resp.StatusCode, opts.expectStatus, strings.TrimSpace(string(responseBody)))
	}
	if opts.expectStatus != http.StatusOK {
		fmt.Printf("PASS request failed explicitly status=%d body=%s\n", resp.StatusCode, strings.TrimSpace(string(responseBody)))
		return nil
	}
	if !bytes.Contains(responseBody, []byte("data: [DONE]")) {
		return errors.New("successful response ended without [DONE]")
	}
	if opts.expectProvider != "" && !bytes.Contains(responseBody, []byte(opts.expectProvider)) {
		return fmt.Errorf("response did not contain provider %q", opts.expectProvider)
	}
	after, err := waitLedger(client, opts.meteringURL, before, 10*time.Second)
	if err != nil {
		return err
	}
	fmt.Printf("PASS stream complete provider=%s request_id=%s ledger_count=%d->%d\n", opts.expectProvider, resp.Header.Get("X-Request-ID"), before, after)
	return nil
}

func waitApplied(client *http.Client, opts options, version int64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status struct {
			Gateways []struct {
				State          string `json:"state"`
				AppliedVersion int64  `json:"applied_version"`
				RejectReason   string `json:"reject_reason"`
			} `json:"gateways"`
		}
		if err := jsonRequest(client, http.MethodGet, opts.controlURL+"/gateways", opts.adminToken, nil, &status); err == nil {
			for _, gateway := range status.Gateways {
				if gateway.State == "rejected" && gateway.RejectReason != "" {
					return fmt.Errorf("Gateway rejected desired version %d: %s", version, gateway.RejectReason)
				}
				if gateway.State == "applied" && gateway.AppliedVersion >= version {
					return nil
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("Gateway did not report applied version %d within %s", version, timeout)
}

func waitReady(client *http.Client, endpoint string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(endpoint)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("%s did not become ready within %s", endpoint, timeout)
}

func ledgerCount(client *http.Client, baseURL string) (int, error) {
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/ledger/count")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("ledger count status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var result struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}
	return result.Count, nil
}

func waitLedger(client *http.Client, baseURL string, before int, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		count, err := ledgerCount(client, baseURL)
		if err == nil && count > before {
			return count, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return before, fmt.Errorf("ledger did not advance from %d within %s", before, timeout)
}

func jsonRequest(client *http.Client, method, endpoint, token string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("%s %s: status=%d body=%s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(message)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func writeSecret(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		return fmt.Errorf("write API key: %w", err)
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "democtl: "+format+"\n", args...)
	os.Exit(1)
}
