package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/database/dbtest"
)

func lookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func execute(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	code = run(ctx, args, lookup(env), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestInvalidArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"up", "extra"}, {"down"}} {
		code, _, stderr := execute(t, nil, args...)
		if code != 2 || !strings.Contains(stderr, "usage: migrate") {
			t.Errorf("args %v: code=%d stderr=%q, want code 2 and usage", args, code, stderr)
		}
	}
}

func TestMissingDatabaseConfiguration(t *testing.T) {
	code, _, stderr := execute(t, nil, "status")
	if code != 2 || !strings.Contains(stderr, "RELAYFORGE_DATABASE_URL") {
		t.Fatalf("code=%d stderr=%q, want code 2 naming the variable", code, stderr)
	}
}

func TestInvalidDatabaseConfiguration(t *testing.T) {
	code, _, stderr := execute(t, map[string]string{
		"RELAYFORGE_DATABASE_URL": "postgres://user:topsecret@127.0.0.1/db",
		"RELAYFORGE_DB_MAX_CONNS": "-3",
	}, "up")
	if code != 2 || strings.Contains(stderr, "topsecret") {
		t.Fatalf("code=%d stderr=%q, want code 2 without credentials", code, stderr)
	}
}

func TestDatabaseConnectionFailure(t *testing.T) {
	// Reserve and release a port so nothing is listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	for _, cmd := range []string{"up", "status"} {
		code, _, stderr := execute(t, map[string]string{
			"RELAYFORGE_DATABASE_URL":       "postgres://user:topsecret@" + addr + "/db?sslmode=disable",
			"RELAYFORGE_DB_CONNECT_TIMEOUT": "3s",
		}, cmd)
		if code != 1 || strings.Contains(stderr, "topsecret") {
			t.Errorf("%s: code=%d stderr=%q, want code 1 without credentials", cmd, code, stderr)
		}
	}
}

func TestStatusAndUpAgainstRealDatabase(t *testing.T) {
	env := map[string]string{"RELAYFORGE_DATABASE_URL": dbtest.NewDatabase(t).URL}

	code, stdout, stderr := execute(t, env, "status")
	if code != 0 || !strings.Contains(stdout, "pending") || strings.Contains(stdout, "applied") {
		t.Fatalf("initial status: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, stderr = execute(t, env, "up")
	if code != 0 || !strings.Contains(stdout, "applied 00001_") {
		t.Fatalf("up: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, stderr = execute(t, env, "up")
	if code != 0 || !strings.Contains(stdout, "no pending migrations") {
		t.Fatalf("second up must be a no-op: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, stderr = execute(t, env, "status")
	if code != 0 || !strings.Contains(stdout, "applied") || strings.Contains(stdout, "pending") {
		t.Fatalf("final status: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
