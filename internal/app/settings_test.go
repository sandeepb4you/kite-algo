package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kite-algo/internal/config"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage/sqlite"
)

// riskTestApp builds an app over a database at a fixed path, so a second call
// with the same path simulates a restart against the same data.
func riskTestApp(t *testing.T, dbPath string, defaults config.RiskConfig) *App {
	t.Helper()
	ctx := context.Background()

	cfg := &config.Config{
		Mode:    config.ModePaper,
		Kite:    config.KiteConfig{APIKey: "k", APISecret: "s"},
		Storage: config.StorageConfig{SQLitePath: dbPath},
		Web:     config.WebConfig{Addr: "127.0.0.1:0", SessionTTL: time.Hour},
		Risk:    defaults,
	}

	store, err := sqlite.New(ctx, dbPath, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	a, err := New(ctx, cfg, store, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	return a
}

var configDefaults = config.RiskConfig{
	MaxDailyLoss: 5000, MaxOrderValue: 100000,
	MaxLotsPerTrade: 1, MaxOpenPositions: 5,
}

// TestRiskLimitsSurviveARestart is the point of persisting them: limits tuned
// during a session must still be in force after a redeploy or a crash, not
// silently revert to whatever config.yaml happened to say.
func TestRiskLimitsSurviveARestart(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "risk.db")

	first := riskTestApp(t, db, configDefaults)
	if got := first.Risk.Limits().MaxDailyLoss; got != 5000 {
		t.Fatalf("initial max daily loss = %v, want the configured 5000", got)
	}
	if first.RiskOverridden() {
		t.Error("a fresh database should report the config defaults, not an override")
	}

	tightened := risk.Limits{
		MaxDailyLoss: 2500, MaxOrderValue: 50000,
		MaxLotsPerTrade: 2, MaxOpenPositions: 3,
	}
	if err := first.SaveRiskLimits(ctx, tightened); err != nil {
		t.Fatalf("SaveRiskLimits: %v", err)
	}

	// Restart against the same database.
	second := riskTestApp(t, db, configDefaults)

	got := second.Risk.Limits()
	if got != tightened {
		t.Errorf("after restart limits = %+v, want the saved %+v", got, tightened)
	}
	if !second.RiskOverridden() {
		t.Error("the restarted app does not report that the limits are an override")
	}
}

// TestResetRestoresConfigDefaults gives the operator a way back to a known state
// without remembering what the numbers originally were.
func TestResetRestoresConfigDefaults(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "reset.db")

	a := riskTestApp(t, db, configDefaults)
	if err := a.SaveRiskLimits(ctx, risk.Limits{MaxDailyLoss: 1}); err != nil {
		t.Fatal(err)
	}
	if err := a.ResetRiskLimits(ctx); err != nil {
		t.Fatalf("ResetRiskLimits: %v", err)
	}

	if got := a.Risk.Limits().MaxDailyLoss; got != 5000 {
		t.Errorf("max daily loss = %v, want the configured 5000 after a reset", got)
	}
	if a.RiskOverridden() {
		t.Error("still reporting an override after reset")
	}

	// And the reset must persist: a restart should not resurrect the override.
	restarted := riskTestApp(t, db, configDefaults)
	if got := restarted.Risk.Limits().MaxDailyLoss; got != 5000 {
		t.Errorf("after restart max daily loss = %v; the deleted override came back", got)
	}
}

// TestConfigChangesApplyWhenNotOverridden: with nothing saved, editing
// config.yaml must still take effect — otherwise the defaults would be frozen at
// whatever the first run happened to see.
func TestConfigChangesApplyWhenNotOverridden(t *testing.T) {
	db := filepath.Join(t.TempDir(), "cfg.db")

	riskTestApp(t, db, configDefaults)

	raised := configDefaults
	raised.MaxDailyLoss = 9999
	second := riskTestApp(t, db, raised)

	if got := second.Risk.Limits().MaxDailyLoss; got != 9999 {
		t.Errorf("max daily loss = %v, want the edited config value 9999", got)
	}
}

// TestSavedOverrideBeatsConfig confirms precedence in the other direction: once
// saved, editing config.yaml must NOT silently change live limits.
func TestSavedOverrideBeatsConfig(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "precedence.db")

	a := riskTestApp(t, db, configDefaults)
	if err := a.SaveRiskLimits(ctx, risk.Limits{MaxDailyLoss: 1234, MaxLotsPerTrade: 1}); err != nil {
		t.Fatal(err)
	}

	changed := configDefaults
	changed.MaxDailyLoss = 88888
	second := riskTestApp(t, db, changed)

	if got := second.Risk.Limits().MaxDailyLoss; got != 1234 {
		t.Errorf("max daily loss = %v, want the saved override 1234", got)
	}
}

// TestCorruptSettingFallsBackToConfig: a platform whose job is to be reachable
// so you can flatten must not refuse to boot over an unreadable settings row.
func TestCorruptSettingFallsBackToConfig(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "corrupt.db")

	a := riskTestApp(t, db, configDefaults)
	if err := a.Store.SetSetting(ctx, riskLimitsKey, "{not valid json"); err != nil {
		t.Fatal(err)
	}

	second := riskTestApp(t, db, configDefaults)
	if got := second.Risk.Limits().MaxDailyLoss; got != 5000 {
		t.Errorf("max daily loss = %v; a corrupt override should fall back to config", got)
	}
	if second.RiskOverridden() {
		t.Error("a corrupt override should not be reported as active")
	}
}
