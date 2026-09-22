package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"llm-gateway/internal/control"
)

func configureProviderCredentials(srv *control.Server, store control.Store, tlsConfig *control.TLSConfig) control.ProviderCredentialStore {
	logProviderCredentialTransport(tlsConfig)
	pgStore, ok := store.(*control.PGStore)
	if !ok {
		log.Printf("provider credentials unavailable: postgres is required")
		return nil
	}
	key, err := control.LoadMasterKeyFromEnv()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Fatalf("provider credential master key: %v", err)
		}
		log.Printf("provider credential master key is not loaded; credential operations fail closed")
	} else {
		log.Printf("provider credential master key loaded key_id=%s", key.ID)
	}
	credStore, err := control.NewPGProviderCredentialStore(pgStore.DB(), key)
	if err != nil {
		log.Fatalf("provider credentials: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := credStore.VerifyMasterKey(ctx); err != nil {
		log.Fatal("provider credential master key cannot decrypt active credentials")
	}
	srv.SetProviderCredentialStore(credStore)
	if err := srv.RevalidateCredentialRefs(ctx); err != nil {
		log.Fatalf("restored provider credential references: %v", err)
	}
	return credStore
}

func logProviderCredentialTransport(tlsConfig *control.TLSConfig) {
	if tlsConfig != nil && tlsConfig.RequireClientCert {
		log.Printf("provider credential resolve enabled with mTLS")
		return
	}
	if flagEnv("CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE") {
		log.Printf("provider credential resolve: CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE=true enables insecure local demo transport")
		return
	}
	log.Printf("provider credential resolve disabled until mTLS is configured; set CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE=true only for local demo")
}
