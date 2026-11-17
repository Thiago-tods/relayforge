// Package natstest provides a NATS JetStream server for tests.
//
// By default it starts an embedded server (the same code the nats-server binary
// runs) on a random loopback port with a temporary store, so no Docker or
// installed server is needed. If RELAYFORGE_TEST_NATS_URL is set, tests use that
// server instead (CI does, with a NATS service container). Tests use unique stream
// and consumer names, so they can share a server.
package natstest

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/Thiago-tods/relayforge/internal/messaging"
)

// EnvURL names the variable that selects an external server.
const EnvURL = "RELAYFORGE_TEST_NATS_URL"

// URL returns the URL of a JetStream server for the test.
func URL(t testing.TB) string {
	t.Helper()
	if u := os.Getenv(EnvURL); u != "" {
		return u
	}
	if os.Getenv("CI") == "true" && os.Getenv("RELAYFORGE_REQUIRE_NATS") == "1" {
		t.Fatalf("%s must be set: NATS integration tests are required in this environment", EnvURL)
	}
	opts := &natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
	}
	s, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("create embedded NATS server: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(15 * time.Second) {
		t.Fatal("embedded NATS server did not become ready")
	}
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	return s.ClientURL()
}

// Config returns a messaging configuration with names unique to this test and
// short timings suitable for tests.
func Config(t testing.TB) messaging.Config {
	t.Helper()
	suffix := make([]byte, 5)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	cfg := messaging.DefaultConfig(URL(t))
	cfg.Stream = "T_" + hex.EncodeToString(suffix)
	cfg.Consumer = "c_" + hex.EncodeToString(suffix)
	cfg.AckWait = 2 * time.Second
	cfg.BackOff = []time.Duration{3 * time.Second, 3 * time.Second}
	cfg.MaxDeliver = 4
	cfg.FetchBatch = 8
	cfg.FetchWait = 300 * time.Millisecond
	return cfg
}
