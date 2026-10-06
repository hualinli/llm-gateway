package organization_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/organization"
)

func TestPlatformModelCatalogAndOrganizationScope(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	owner := createUser(t, db, "model-catalog-owner@example.com")
	org, err := svc.CreateEnterprise(ctx, owner.ID, "Model catalog account")
	if err != nil {
		t.Fatal(err)
	}

	initial, err := svc.ListPlatformModelCatalog(ctx)
	if err != nil || len(initial) != 0 {
		t.Fatalf("cold platform catalog = %+v, err=%v; want explicit empty/deny by default", initial, err)
	}

	configured, err := svc.ReplacePlatformModelCatalog(ctx, owner.ID, []organization.PlatformModel{
		{Model: "model-z", Enabled: false}, {Model: " model-a ", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCatalog := []organization.PlatformModel{{Model: "model-a", Enabled: true}, {Model: "model-z", Enabled: false}}
	if !reflect.DeepEqual(configured, wantCatalog) {
		t.Fatalf("catalog = %+v, want %+v", configured, wantCatalog)
	}
	if _, err := svc.ReplacePlatformModelCatalog(ctx, owner.ID, []organization.PlatformModel{{Model: "model-a", Enabled: true}, {Model: " model-a ", Enabled: false}}); !errors.Is(err, organization.ErrModelCatalogInvalid) {
		t.Fatalf("duplicate normalized catalog model error=%v", err)
	}
	if _, err := svc.ReplacePlatformModelCatalog(ctx, owner.ID, []organization.PlatformModel{{Model: "  ", Enabled: true}}); !errors.Is(err, organization.ErrModelCatalogInvalid) {
		t.Fatalf("empty catalog model error=%v", err)
	}

	inherited, err := svc.GetOrganizationModelAllowlist(ctx, org.ID)
	if err != nil || inherited != nil {
		t.Fatalf("initial account scope = %v, err=%v; want inherited NULL", inherited, err)
	}
	denyAll := []string{}
	stored, err := svc.SetOrganizationModelAllowlist(ctx, org.ID, &denyAll)
	if err != nil || stored == nil || len(*stored) != 0 {
		t.Fatalf("deny-all account scope = %v, err=%v", stored, err)
	}
	custom := []string{" model-a "}
	stored, err = svc.SetOrganizationModelAllowlist(ctx, org.ID, &custom)
	if err != nil || stored == nil || !reflect.DeepEqual(*stored, []string{"model-a"}) {
		t.Fatalf("canonical account scope = %v, err=%v", stored, err)
	}
	if _, err := svc.SetOrganizationModelAllowlist(ctx, org.ID, &[]string{"model-missing"}); !errors.Is(err, organization.ErrModelCatalogInvalid) {
		t.Fatalf("unknown account model error=%v", err)
	}
	if _, err := svc.SetOrganizationModelAllowlist(ctx, org.ID, &[]string{"model-z"}); !errors.Is(err, organization.ErrModelCatalogInvalid) {
		t.Fatalf("disabled account model error=%v", err)
	}
	stored, err = svc.GetOrganizationModelAllowlist(ctx, org.ID)
	if err != nil || stored == nil || !reflect.DeepEqual(*stored, []string{"model-a"}) {
		t.Fatalf("rejected scope update changed stored scope: %v err=%v", stored, err)
	}
	if _, err := svc.SetOrganizationModelAllowlist(ctx, org.ID, &[]string{"model-a", " model-a "}); !errors.Is(err, organization.ErrModelCatalogInvalid) {
		t.Fatalf("duplicate account model error=%v", err)
	}

	// Disabling a model does not erase the previously granted account scope.
	// It stops being effective through the global intersection and resumes if
	// the platform later re-enables it.
	if _, err := svc.ReplacePlatformModelCatalog(ctx, owner.ID, []organization.PlatformModel{
		{Model: "model-a", Enabled: false}, {Model: "model-z", Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	stored, err = svc.GetOrganizationModelAllowlist(ctx, org.ID)
	if err != nil || stored == nil || !reflect.DeepEqual(*stored, []string{"model-a"}) {
		t.Fatalf("global disable erased account grant: %v err=%v", stored, err)
	}
	if _, err := svc.ReplacePlatformModelCatalog(ctx, owner.ID, []organization.PlatformModel{
		{Model: "model-a", Enabled: true}, {Model: "model-z", Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	stored, err = svc.GetOrganizationModelAllowlist(ctx, org.ID)
	if err != nil || stored == nil || !reflect.DeepEqual(*stored, []string{"model-a"}) {
		t.Fatalf("global re-enable did not preserve previous account grant: %v err=%v", stored, err)
	}
	stored, err = svc.SetOrganizationModelAllowlist(ctx, org.ID, nil)
	if err != nil || stored != nil {
		t.Fatalf("inherited account scope after reset = %v, err=%v", stored, err)
	}
}

func TestPlatformModelCatalogConcurrentReplacementIsAtomic(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	owner := createUser(t, db, "model-catalog-concurrent@example.com")
	first := []organization.PlatformModel{{Model: "first-a", Enabled: true}, {Model: "first-b", Enabled: false}}
	second := []organization.PlatformModel{{Model: "second-a", Enabled: true}, {Model: "second-b", Enabled: true}}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, models := range [][]organization.PlatformModel{first, second} {
		models := models
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.ReplacePlatformModelCatalog(ctx, owner.ID, models)
			errCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := svc.ListPlatformModelCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, first) && !reflect.DeepEqual(got, second) {
		t.Fatalf("concurrent replace left mixed catalog: %+v", got)
	}
}

func TestOrganizationScopeWriteSerializesWithCatalogDisable(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	owner := createUser(t, db, "model-catalog-scope-race@example.com")
	org, err := svc.CreateEnterprise(ctx, owner.ID, "Model scope race account")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReplacePlatformModelCatalog(ctx, owner.ID, []organization.PlatformModel{{Model: "race-model", Enabled: true}}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := svc.ReplacePlatformModelCatalog(ctx, owner.ID, []organization.PlatformModel{})
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := svc.SetOrganizationModelAllowlist(ctx, org.ID, &[]string{"race-model"})
		if !errors.Is(err, organization.ErrModelCatalogInvalid) {
			errs <- err
			return
		}
		errs <- nil
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent catalog/scope update: %v", err)
		}
	}

	catalog, err := svc.ListPlatformModelCatalog(ctx)
	if err != nil || len(catalog) != 0 {
		t.Fatalf("catalog after concurrent disable = %+v, err=%v; want empty", catalog, err)
	}
	stored, err := svc.GetOrganizationModelAllowlist(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored != nil && (len(*stored) != 1 || (*stored)[0] != "race-model") {
		t.Fatalf("unexpected stored account scope after concurrent update: %v", stored)
	}
}
