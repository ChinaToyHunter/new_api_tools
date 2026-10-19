package service

import (
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/database"
)

// ============================================================================
// Selected-models authority — service contract tests
//
// These run in the explicit in-process mode (no shared store configured), which
// is the mode cache.Get() hands back when REDIS_CONN_STRING is unset. The
// shared-store ordering contract is covered in the cache package
// (manager_authority_test.go); here we assert what the service guarantees
// regardless of the backing store.
// ============================================================================

// installSQLiteForModelStatus lets NewModelStatusService() construct without
// panicking (database.Get()); the selected-models path only touches the cache.
func installSQLiteForModelStatus(t *testing.T) {
	t.Helper()
	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() { _ = db.Close() })
}

// resetSelectionState clears any selection carried over from an earlier test so
// each case starts from "not set".
func resetSelectionState(t *testing.T) {
	t.Helper()
	if err := cache.Get().SetAuthoritative(selectedModelsCacheKey, []string{}, 0); err != nil {
		t.Fatalf("reset selection: %v", err)
	}
	if got, err := NewModelStatusService().GetSelectedModels(); err != nil {
		t.Fatalf("verify reset: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("reset left %d models", len(got))
	}
}

// An empty list is a legal, storable answer. Saving [] must not silently fall
// back to any inferred list, and must read back as [] — not null, not "unset".
func TestSelectedModelsEmptyIsLegalWithoutAnyInference(t *testing.T) {
	installSQLiteForModelStatus(t)
	resetSelectionState(t)

	svc := NewModelStatusService()

	if err := svc.SetSelectedModels([]string{}); err != nil {
		t.Fatalf("empty save should be legal: %v", err)
	}

	got, err := svc.GetSelectedModels()
	if err != nil {
		t.Fatalf("read back empty: %v", err)
	}
	if got == nil {
		t.Fatal("empty selection decoded as nil; must be an empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("empty selection read back %d models: %v", len(got), got)
	}

	// nil must behave identically to [] — both mean "nothing is published".
	if err := svc.SetSelectedModels(nil); err != nil {
		t.Fatalf("nil save should be legal: %v", err)
	}
	got2, err := svc.GetSelectedModels()
	if err != nil {
		t.Fatalf("read back nil: %v", err)
	}
	if got2 == nil || len(got2) != 0 {
		t.Fatalf("nil selection read back %#v", got2)
	}
}

// Trimming and de-duplication must preserve the operator's order, since that
// order is the display order on the public page.
func TestSelectedModelsNormalizesBlanksAndDuplicatesKeepsOrder(t *testing.T) {
	installSQLiteForModelStatus(t)
	resetSelectionState(t)

	svc := NewModelStatusService()

	in := []string{" gpt-5 ", "", "claude-sonnet-4-5", "gpt-5", "   ", "claude-sonnet-4-5"}
	if err := svc.SetSelectedModels(in); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := svc.GetSelectedModels()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 || got[0] != "gpt-5" || got[1] != "claude-sonnet-4-5" {
		t.Fatalf("normalize lost order or kept junk: %v", got)
	}
}

// The admin read (GetConfig), the authority read, and the public embed read must
// all return the same list from the same source.
func TestGetConfigAndEmbedAgreeWithSelectedModels(t *testing.T) {
	installSQLiteForModelStatus(t)
	resetSelectionState(t)

	svc := NewModelStatusService()

	want := []string{"m-a", "m-b", "m-c"}
	if err := svc.SetSelectedModels(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	cfg, err := svc.GetConfig()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	fromConfig, ok := cfg["selected_models"].([]string)
	if !ok {
		t.Fatalf("config selected_models wrong type: %#v", cfg["selected_models"])
	}

	fromService, err := svc.GetSelectedModels()
	if err != nil {
		t.Fatalf("service read: %v", err)
	}

	for i := range want {
		if fromConfig[i] != want[i] || fromService[i] != want[i] {
			t.Fatalf("index %d: config=%v service=%v want=%v", i, fromConfig, fromService, want)
		}
	}

	embed, err := svc.GetEmbedConfig()
	if err != nil {
		t.Fatalf("embed config: %v", err)
	}
	fromEmbed, ok := embed["selected_models"].([]string)
	if !ok {
		t.Fatalf("embed selected_models wrong type: %#v", embed["selected_models"])
	}
	if len(fromEmbed) != len(want) || fromEmbed[0] != want[0] {
		t.Fatalf("embed list %v differs from saved %v", fromEmbed, want)
	}
	// The embed payload must keep its availability metadata.
	if embed["available_themes"] == nil || embed["available_time_windows"] == nil {
		t.Fatal("embed config lost its availability metadata")
	}
}

// A second service instance must see what the first one saved: the value is not
// tied to a single service object.
func TestSelectionIsVisibleToAnotherServiceInstance(t *testing.T) {
	installSQLiteForModelStatus(t)
	resetSelectionState(t)

	want := []string{"shared-a", "shared-b"}
	if err := NewModelStatusService().SetSelectedModels(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := NewModelStatusService().GetSelectedModels()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("selection not visible to another instance: %v", got)
	}
}

// Clearing a previously non-empty list must actually clear it, otherwise the
// public page would keep showing models the operator removed.
func TestClearingSelectionRemovesPreviouslySavedModels(t *testing.T) {
	installSQLiteForModelStatus(t)
	resetSelectionState(t)

	svc := NewModelStatusService()
	if err := svc.SetSelectedModels([]string{"keep-1", "keep-2"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.SetSelectedModels([]string{}); err != nil {
		t.Fatalf("clear: %v", err)
	}

	got, err := svc.GetSelectedModels()
	if err != nil {
		t.Fatalf("read after clear: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("clear left %v", got)
	}

	cfg, err := svc.GetConfig()
	if err != nil {
		t.Fatalf("config after clear: %v", err)
	}
	if listed, ok := cfg["selected_models"].([]string); !ok || len(listed) != 0 {
		t.Fatalf("config after clear: %#v", cfg["selected_models"])
	}
}
