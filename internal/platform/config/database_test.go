package config

import (
	"strings"
	"testing"
	"time"
)

const testURL = "postgres://user:s3cret@localhost:5432/db?sslmode=disable"

func TestLoadDatabaseFromDefaults(t *testing.T) {
	db, err := LoadDatabaseFrom(lookupFrom(map[string]string{"RELAYFORGE_DATABASE_URL": testURL}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := db.Validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}
	if db.MaxConns != 10 || db.MinConns != 0 || db.ConnectTimeout != 10*time.Second {
		t.Fatalf("unexpected defaults: %+v", db)
	}
}

func TestLoadDatabaseFromOverrides(t *testing.T) {
	db, err := LoadDatabaseFrom(lookupFrom(map[string]string{
		"RELAYFORGE_DATABASE_URL":           testURL,
		"RELAYFORGE_DB_MAX_CONNS":           "20",
		"RELAYFORGE_DB_MIN_CONNS":           "2",
		"RELAYFORGE_DB_MAX_CONN_LIFETIME":   "30m",
		"RELAYFORGE_DB_MAX_CONN_IDLE_TIME":  "90",
		"RELAYFORGE_DB_HEALTH_CHECK_PERIOD": "15s",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if db.MaxConns != 20 || db.MinConns != 2 || db.MaxConnLifetime != 30*time.Minute ||
		db.MaxConnIdleTime != 90*time.Second || db.HealthCheckPeriod != 15*time.Second {
		t.Fatalf("unexpected config: %+v", db)
	}
}

func TestLoadDatabaseFromParseErrors(t *testing.T) {
	for _, key := range []string{
		"RELAYFORGE_DB_MAX_CONNS", "RELAYFORGE_DB_MIN_CONNS", "RELAYFORGE_DB_MAX_CONN_LIFETIME",
		"RELAYFORGE_DB_MAX_CONN_IDLE_TIME", "RELAYFORGE_DB_HEALTH_CHECK_PERIOD", "RELAYFORGE_DB_CONNECT_TIMEOUT",
	} {
		if _, err := LoadDatabaseFrom(lookupFrom(map[string]string{key: "abc"})); err == nil {
			t.Errorf("%s: expected parse error", key)
		}
	}
}

func TestDatabaseValidate(t *testing.T) {
	valid := Database{
		URL: testURL, MaxConns: 5, MinConns: 1,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute,
		HealthCheckPeriod: time.Minute, ConnectTimeout: time.Second,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := map[string]func(*Database){
		"missing url":     func(d *Database) { d.URL = "" },
		"wrong scheme":    func(d *Database) { d.URL = "mysql://u:p@h/db" },
		"zero max":        func(d *Database) { d.MaxConns = 0 },
		"negative min":    func(d *Database) { d.MinConns = -1 },
		"min above max":   func(d *Database) { d.MinConns = 9 },
		"zero lifetime":   func(d *Database) { d.MaxConnLifetime = 0 },
		"negative idle":   func(d *Database) { d.MaxConnIdleTime = -time.Second },
		"zero health":     func(d *Database) { d.HealthCheckPeriod = 0 },
		"zero connect":    func(d *Database) { d.ConnectTimeout = 0 },
		"unparseable url": func(d *Database) { d.URL = "postgres://user:pa ss@host/%zz" },
	}
	for name, mutate := range cases {
		d := valid
		mutate(&d)
		err := d.Validate()
		if err == nil {
			t.Errorf("%s: expected validation error", name)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "pa ss") {
			t.Errorf("%s: error leaks credentials: %v", name, err)
		}
	}
}

func TestRedactedURL(t *testing.T) {
	got := Database{URL: testURL}.RedactedURL()
	if strings.Contains(got, "s3cret") || !strings.Contains(got, "localhost") {
		t.Fatalf("unexpected redacted URL: %s", got)
	}
	if (Database{URL: "://bad"}).RedactedURL() != "<invalid>" {
		t.Fatal("invalid URL must not be echoed")
	}
}
