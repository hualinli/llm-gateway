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
	"strings"
	"time"

	"llm-gateway/internal/control"
)

func ensureMasterKey(args []string) error {
	flags := flag.NewFlagSet("ensure-master-key", flag.ContinueOnError)
	path := flags.String("file", "data/provider-credential-master-key", "persistent master key file")
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		return err
	}
	key, created, err := control.EnsureMasterKeyFile(*path)
	if err != nil {
		return err
	}
	action := "reused"
	if created {
		action = "created"
	}
	fmt.Printf("PASS master key %s key_id=%s file=%s\n", action, key.ID, *path)
	return nil
}

func credentialCreate(client *http.Client, opts options, args []string) error {
	flags := flag.NewFlagSet("credential-create", flag.ContinueOnError)
	flags.StringVar(&opts.controlURL, "control", opts.controlURL, "control HTTP base URL")
	flags.StringVar(&opts.adminToken, "admin-token", opts.adminToken, "control admin token")
	providerID := flags.String("provider", "", "provider id that will own the credential")
	secretFile := flags.String("secret-file", "", "0600 file containing the provider secret; stdin is used when empty")
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*providerID) == "" {
		return errors.New("provider is required")
	}
	secret, err := readProviderSecret(*secretFile)
	if err != nil {
		return err
	}
	var created struct {
		CredentialID  string `json:"credential_id"`
		CredentialRef string `json:"credential_ref"`
		ProviderID    string `json:"provider_id"`
		Fingerprint   string `json:"fingerprint"`
		Status        string `json:"status"`
	}
	payload := map[string]string{"provider_id": *providerID, "secret": string(secret)}
	if err := jsonRequest(client, http.MethodPost, opts.controlURL+"/provider-credentials", opts.adminToken, payload, &created); err != nil {
		return fmt.Errorf("create provider credential: %s", publicCredentialError(err))
	}
	if strings.Contains(created.CredentialRef, string(secret)) || created.CredentialRef == "" {
		return errors.New("credential service returned an unsafe or empty reference")
	}
	encoded, err := json.Marshal(created)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", encoded)
	return nil
}

func providerApply(client *http.Client, opts options, args []string) error {
	flags := flag.NewFlagSet("provider-apply", flag.ContinueOnError)
	flags.StringVar(&opts.controlURL, "control", opts.controlURL, "control HTTP base URL")
	flags.StringVar(&opts.adminToken, "admin-token", opts.adminToken, "control admin token")
	id := flags.String("id", "", "provider id")
	providerURL := flags.String("url", "", "provider base URL")
	credentialRef := flags.String("credential-ref", "", "control:// provider credential reference")
	route := flags.String("route", "default", "model route that should use this provider")
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *id == "" || *providerURL == "" {
		return errors.New("id and url are required")
	}
	var current struct {
		Providers map[string]map[string]any `json:"providers"`
		Routes    map[string][]string       `json:"routes"`
	}
	if err := jsonRequest(client, http.MethodGet, opts.controlURL+"/config", opts.adminToken, nil, &current); err != nil {
		return fmt.Errorf("read config: %s", publicCredentialError(err))
	}
	if current.Providers == nil {
		current.Providers = map[string]map[string]any{}
	}
	if current.Routes == nil {
		current.Routes = map[string][]string{}
	}
	current.Providers[*id] = map[string]any{
		"id": *id, "protocol": "openai-compatible", "url": *providerURL, "credential_ref": *credentialRef,
	}
	if *route != "" {
		current.Routes[*route] = []string{*id}
	}
	var applied struct {
		DesiredVersion int64 `json:"desired_version"`
	}
	payload := map[string]any{"providers": current.Providers, "routes": current.Routes}
	if err := jsonRequest(client, http.MethodPost, opts.controlURL+"/config", opts.adminToken, payload, &applied); err != nil {
		return fmt.Errorf("apply provider: %s", publicCredentialError(err))
	}
	if err := waitApplied(client, opts, applied.DesiredVersion, 15*time.Second); err != nil {
		return err
	}
	fmt.Printf("PASS provider applied id=%s credential_ref=%s desired_version=%d\n", *id, *credentialRef, applied.DesiredVersion)
	return nil
}

func credentialRevoke(client *http.Client, opts options, args []string) error {
	flags := flag.NewFlagSet("credential-revoke", flag.ContinueOnError)
	flags.StringVar(&opts.controlURL, "control", opts.controlURL, "control HTTP base URL")
	flags.StringVar(&opts.adminToken, "admin-token", opts.adminToken, "control admin token")
	id := flags.String("id", "", "credential id")
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("id is required")
	}
	if err := jsonRequest(client, http.MethodDelete, opts.controlURL+"/provider-credentials/"+*id, opts.adminToken, nil, nil); err != nil {
		return fmt.Errorf("revoke provider credential: %s", publicCredentialError(err))
	}
	fmt.Printf("PASS credential revoked id=%s\n", *id)
	return nil
}

func readProviderSecret(path string) ([]byte, error) {
	var secret []byte
	var err error
	if path == "" {
		secret, err = io.ReadAll(io.LimitReader(os.Stdin, 16<<10+1))
	} else {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, statErr
		}
		if info.Mode().Perm() != 0o600 {
			return nil, errors.New("secret file must be mode 0600")
		}
		secret, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	if bytes.HasSuffix(secret, []byte("\n")) && !bytes.Contains(secret[:len(secret)-1], []byte("\n")) {
		// A single trailing newline from an editor is not part of a typical
		// pasted key. The HTTP API itself never trims a secret it receives.
		secret = bytes.TrimSuffix(secret, []byte("\n"))
	}
	if len(secret) == 0 || len(secret) > 16<<10 {
		return nil, errors.New("secret must be non-empty and at most 16 KiB")
	}
	return secret, nil
}

func publicCredentialError(err error) string {
	message := err.Error()
	if strings.Contains(message, "sk-") {
		return "credential request failed"
	}
	return message
}
