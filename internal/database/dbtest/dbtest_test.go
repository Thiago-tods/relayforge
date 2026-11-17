package dbtest

import (
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestValidateServerURL(t *testing.T) {
	t.Setenv(EnvAllowRemote, "")

	valid := []string{
		"postgres://user:pw@127.0.0.1:5432/postgres?sslmode=disable",
		"postgresql://user:pw@localhost/postgres",
		"postgres://user@[::1]:5432/relayforge_ci?sslmode=disable&connect_timeout=5",
	}
	for _, raw := range valid {
		if err := ValidateServerURL(raw); err != nil {
			t.Errorf("%q should be accepted: %v", raw, err)
		}
	}

	invalid := map[string]string{
		"wrong scheme":      "mysql://user:pw@127.0.0.1/db",
		"no user":           "postgres://127.0.0.1/db",
		"no host":           "postgres://user:pw@/db",
		"multiple hosts":    "postgres://user:pw@127.0.0.1,10.0.0.5/db",
		"remote host":       "postgres://user:pw@db.example.com:5432/postgres",
		"prod name":         "postgres://user:pw@127.0.0.1/relayforge_production",
		"live name":         "postgres://user:pw@127.0.0.1/LIVE",
		"redirect host":     "postgres://user:pw@127.0.0.1/db?host=db.example.com",
		"redirect dbname":   "postgres://user:pw@127.0.0.1/db?dbname=prod",
		"service file":      "postgres://user:pw@127.0.0.1/db?service=prod",
		"non numeric port":  "postgres://user:pw@127.0.0.1:abc/db",
		"malformed":         "postgres://user:p w@127.0.0.1/%zz",
		"nested path":       "postgres://user:pw@127.0.0.1/a/b",
		"empty":             "",
		"not a url":         "::::",
		"unexpected option": "postgres://user:pw@127.0.0.1/db?options=-c%20role%3Dadmin",
	}
	for name, raw := range invalid {
		err := ValidateServerURL(raw)
		if err == nil {
			t.Errorf("%s: %q should be rejected", name, raw)
			continue
		}
		if strings.Contains(err.Error(), "pw") && strings.Contains(raw, ":pw@") {
			t.Errorf("%s: error leaks credentials: %v", name, err)
		}
	}
}

func TestValidateServerURLRemoteOptIn(t *testing.T) {
	const raw = "postgres://user:pw@db.internal.example:5432/postgres"
	t.Setenv(EnvAllowRemote, "")
	if ValidateServerURL(raw) == nil {
		t.Fatal("remote host must be rejected by default")
	}
	t.Setenv(EnvAllowRemote, "1")
	if err := ValidateServerURL(raw); err != nil {
		t.Fatalf("remote host must be accepted with explicit opt-in: %v", err)
	}
	// The opt-in never relaxes the production-name check.
	if ValidateServerURL("postgres://user:pw@db.internal.example/prod") == nil {
		t.Fatal("production-looking name must be rejected even with opt-in")
	}
}

func TestGeneratedNamePattern(t *testing.T) {
	for _, ok := range []string{"rf_test_0123456789ab", "rf_test_ffffffffffff"} {
		if !generatedName.MatchString(ok) {
			t.Errorf("%q should match", ok)
		}
	}
	for _, bad := range []string{
		"postgres", "relayforge", "rf_test_", "rf_test_xyz", "rf_test_0123456789abc",
		"RF_TEST_0123456789ab", "rf_test_0123456789ab; DROP DATABASE postgres", "rf_test_0123456789ab\"",
	} {
		if generatedName.MatchString(bad) {
			t.Errorf("%q must not match", bad)
		}
	}
}

func TestDroppableNameRefusesForeignDatabases(t *testing.T) {
	for _, raw := range []string{
		"postgres://u:pw@127.0.0.1/postgres",
		"postgres://u:pw@127.0.0.1/relayforge",
		"postgres://u:pw@127.0.0.1/rf_test_0123456789ab", // well-formed, but never created here
		"postgres://u:pw@127.0.0.1/",
		"postgres://u:p w@127.0.0.1/%zz",
	} {
		name, err := droppableName(raw)
		if err == nil {
			t.Errorf("%q: expected refusal, got name %q", raw, name)
			continue
		}
		if strings.Contains(err.Error(), ":pw@") {
			t.Errorf("error leaks credentials: %v", err)
		}
	}

	created.Store("rf_test_aaaaaaaaaaaa", struct{}{})
	t.Cleanup(func() { created.Delete("rf_test_aaaaaaaaaaaa") })
	if name, err := droppableName("postgres://u:pw@127.0.0.1/rf_test_aaaaaaaaaaaa"); err != nil || name != "rf_test_aaaaaaaaaaaa" {
		t.Fatalf("registered generated name must be droppable: %q %v", name, err)
	}
}

func TestRedactErrRemovesCredentials(t *testing.T) {
	const raw = "postgres://user:topsecret@127.0.0.1/db"
	got := redactErr(errString("dial "+raw+" failed: password topsecret rejected"), raw)
	if strings.Contains(got, "topsecret") {
		t.Fatalf("credentials leaked: %s", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// The following tests need a real server; they are skipped locally without one
// and fail in CI.

func TestNewDatabaseIsolatedAndDropped(t *testing.T) {
	cfg := NewDatabase(t)
	if _, err := droppableName(cfg.URL); err != nil {
		t.Fatalf("new database must be droppable by the helper: %v", err)
	}

	Drop(t, cfg)
	Drop(t, cfg) // dropping twice is a no-op, and cleanup drops it a third time
}

func TestNewDatabaseParallelNamesAreUnique(t *testing.T) {
	const n = 8
	names := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg := NewDatabase(t)
			u, _ := url.Parse(cfg.URL)
			names[i] = strings.TrimPrefix(u.Path, "/")
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, name := range names {
		if !generatedName.MatchString(name) || seen[name] {
			t.Fatalf("invalid or duplicate database name %q in %v", name, names)
		}
		seen[name] = true
	}
}
