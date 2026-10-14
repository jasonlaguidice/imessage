// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2026 Jason LaGuidice
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"bytes"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
)

func TestApplyStockDefault(t *testing.T) {
	t.Run("int", func(t *testing.T) {
		tests := []struct {
			name           string
			current, stock int
			def, want      int
			wantChanged    bool
		}{
			{"stock value gets default", 50, 50, math.MaxInt32, math.MaxInt32, true},
			{"operator value kept", 1000, 50, math.MaxInt32, 1000, false},
			{"operator zero kept", 0, 50, math.MaxInt32, 0, false},
			{"default equal to stock is not a change", 50, 50, 50, 50, false},
		}
		for _, tt := range tests {
			got, changed := applyStockDefault(tt.current, tt.stock, tt.def)
			if got != tt.want || changed != tt.wantChanged {
				t.Errorf("%s: applyStockDefault(%d, %d, %d) = (%d, %v), want (%d, %v)",
					tt.name, tt.current, tt.stock, tt.def, got, changed, tt.want, tt.wantChanged)
			}
		}
	})
	t.Run("bool", func(t *testing.T) {
		tests := []struct {
			current, stock, def, want, wantChanged bool
		}{
			{false, false, true, true, true},
			{true, false, true, true, false},
		}
		for _, tt := range tests {
			got, changed := applyStockDefault(tt.current, tt.stock, tt.def)
			if got != tt.want || changed != tt.wantChanged {
				t.Errorf("applyStockDefault(%v, %v, %v) = (%v, %v), want (%v, %v)",
					tt.current, tt.stock, tt.def, got, changed, tt.want, tt.wantChanged)
			}
		}
	})
	t.Run("duration", func(t *testing.T) {
		tests := []struct {
			current, stock, def, want time.Duration
			wantChanged               bool
		}{
			{0, 0, 5 * time.Minute, 5 * time.Minute, true},
			{2 * time.Minute, 0, 5 * time.Minute, 2 * time.Minute, false},
			{30 * time.Second, 0, 5 * time.Minute, 30 * time.Second, false},
		}
		for _, tt := range tests {
			got, changed := applyStockDefault(tt.current, tt.stock, tt.def)
			if got != tt.want || changed != tt.wantChanged {
				t.Errorf("applyStockDefault(%s, %s, %s) = (%s, %v), want (%s, %v)",
					tt.current, tt.stock, tt.def, got, changed, tt.want, tt.wantChanged)
			}
		}
	})
}

// mautrixStockBridgeConfig returns a bridge config holding mautrix v0.31.0's
// example-config.yaml values for the keys this bridge has defaults for.
func mautrixStockBridgeConfig() *bridgeconfig.BridgeConfig {
	return &bridgeconfig.BridgeConfig{
		UnknownErrorAutoReconnect:     0, // null
		UnknownErrorMaxAutoReconnects: 10,
		PhoneNumbersInProfile:         false,
		Backfill: bridgeconfig.BackfillConfig{
			Enabled:            false,
			MaxInitialMessages: 50,
			MaxCatchupMessages: 500,
			Queue: bridgeconfig.BackfillQueueConfig{
				Enabled:    false,
				BatchSize:  100,
				MaxBatches: -1,
			},
		},
	}
}

// capsOnlyMatrix is a bridgev2.MatrixConnector that only implements
// GetCapabilities; any other method call panics on the nil embedded interface.
type capsOnlyMatrix struct {
	bridgev2.MatrixConnector
	caps bridgev2.MatrixCapabilities
}

func (m *capsOnlyMatrix) GetCapabilities() *bridgev2.MatrixCapabilities { return &m.caps }

func newDefaultsTestConnector(cfg *bridgeconfig.BridgeConfig, logOut *bytes.Buffer, batchSending bool) *IMConnector {
	return &IMConnector{Bridge: &bridgev2.Bridge{
		Config: cfg,
		Log:    zerolog.New(logOut),
		Matrix: &capsOnlyMatrix{caps: bridgev2.MatrixCapabilities{BatchSending: batchSending}},
	}}
}

func TestApplyBridgeConfigDefaultsOnStockConfig(t *testing.T) {
	cfg := mautrixStockBridgeConfig()
	var logOut bytes.Buffer
	newDefaultsTestConnector(cfg, &logOut, true).applyBridgeConfigDefaults()

	if !cfg.PhoneNumbersInProfile {
		t.Error("phone_numbers_in_profile not defaulted to true")
	}
	if cfg.UnknownErrorAutoReconnect != 5*time.Minute {
		t.Errorf("unknown_error_auto_reconnect = %s, want 5m", cfg.UnknownErrorAutoReconnect)
	}
	if cfg.UnknownErrorMaxAutoReconnects != 100 {
		t.Errorf("unknown_error_max_auto_reconnects = %d, want 100", cfg.UnknownErrorMaxAutoReconnects)
	}
	if !cfg.Backfill.Enabled {
		t.Error("backfill.enabled not defaulted to true")
	}
	if cfg.Backfill.MaxInitialMessages != math.MaxInt32 {
		t.Errorf("backfill.max_initial_messages = %d, want MaxInt32", cfg.Backfill.MaxInitialMessages)
	}
	if cfg.Backfill.MaxCatchupMessages != math.MaxInt32 {
		t.Errorf("backfill.max_catchup_messages = %d, want MaxInt32", cfg.Backfill.MaxCatchupMessages)
	}
	if !cfg.Backfill.Queue.Enabled {
		t.Error("backfill.queue.enabled not defaulted to true")
	}
	if cfg.Backfill.Queue.BatchSize != 10000 {
		t.Errorf("backfill.queue.batch_size = %d, want 10000", cfg.Backfill.Queue.BatchSize)
	}
	if cfg.Backfill.Queue.MaxBatches != -1 {
		t.Errorf("backfill.queue.max_batches = %d, want it left at -1", cfg.Backfill.Queue.MaxBatches)
	}

	// One info line per applied default.
	lines := strings.Split(strings.TrimSpace(logOut.String()), "\n")
	if len(lines) != len(bridgeConfigDefaults) {
		t.Fatalf("got %d log lines, want %d:\n%s", len(lines), len(bridgeConfigDefaults), logOut.String())
	}
	for _, line := range lines {
		if !strings.Contains(line, `"level":"info"`) || !strings.Contains(line, "is at mautrix's default (") {
			t.Errorf("unexpected log line: %s", line)
		}
	}
	if !strings.Contains(logOut.String(), "bridge.unknown_error_auto_reconnect is at mautrix's default (null); using this bridge's default 5m0s") {
		t.Errorf("auto_reconnect log line missing or misformatted:\n%s", logOut.String())
	}

	// A second pass changes nothing: the values are no longer stock.
	if applied, skipped := applyConfigDefaults(cfg, bridgeConfigDefaults, homeserverFeatures{BatchSending: true}); len(applied) != 0 || len(skipped) != 0 {
		t.Errorf("second pass applied %d and skipped %d defaults, want 0 and 0", len(applied), len(skipped))
	}
}

// On a homeserver without batch sending (e.g. Synapse) bridgev2 does not run
// the backfill queue, so the queue default is skipped; the others still apply.
func TestApplyBridgeConfigDefaultsWithoutBatchSending(t *testing.T) {
	cfg := mautrixStockBridgeConfig()
	var logOut bytes.Buffer
	newDefaultsTestConnector(cfg, &logOut, false).applyBridgeConfigDefaults()

	if cfg.Backfill.Queue.Enabled {
		t.Error("backfill.queue.enabled defaulted to true on a homeserver without batch sending")
	}
	if !cfg.PhoneNumbersInProfile || !cfg.Backfill.Enabled ||
		cfg.UnknownErrorAutoReconnect != 5*time.Minute || cfg.UnknownErrorMaxAutoReconnects != 100 ||
		cfg.Backfill.MaxInitialMessages != math.MaxInt32 || cfg.Backfill.MaxCatchupMessages != math.MaxInt32 ||
		cfg.Backfill.Queue.BatchSize != 10000 {
		t.Errorf("other stock keys not defaulted: %+v phone_numbers_in_profile=%v auto_reconnect=%s max_auto_reconnects=%d",
			cfg.Backfill, cfg.PhoneNumbersInProfile, cfg.UnknownErrorAutoReconnect, cfg.UnknownErrorMaxAutoReconnects)
	}

	// One info line per applied default, and a single debug line for the
	// skipped queue default.
	var infos, debugs []string
	for _, line := range strings.Split(strings.TrimSpace(logOut.String()), "\n") {
		switch {
		case strings.Contains(line, `"level":"info"`):
			infos = append(infos, line)
		case strings.Contains(line, `"level":"debug"`):
			debugs = append(debugs, line)
		default:
			t.Errorf("unexpected log line: %s", line)
		}
	}
	if len(infos) != len(bridgeConfigDefaults)-1 {
		t.Errorf("got %d info lines, want %d:\n%s", len(infos), len(bridgeConfigDefaults)-1, logOut.String())
	}
	for _, line := range infos {
		if strings.Contains(line, `"config_key":"backfill.queue.enabled"`) {
			t.Errorf("logged an applied queue default: %s", line)
		}
	}
	if len(debugs) != 1 || !strings.Contains(debugs[0], `"config_key":"backfill.queue.enabled"`) ||
		!strings.Contains(debugs[0], "com.beeper.batch_sending") {
		t.Errorf("want one debug line for backfill.queue.enabled mentioning com.beeper.batch_sending, got:\n%s", strings.Join(debugs, "\n"))
	}
}

// An operator's explicit backfill.queue.enabled: true is kept, and not logged,
// whether or not the homeserver supports batch sending (bridgev2 warns itself
// when it does not).
func TestApplyBridgeConfigDefaultsKeepsExplicitQueueEnabled(t *testing.T) {
	for _, batchSending := range []bool{false, true} {
		cfg := mautrixStockBridgeConfig()
		cfg.Backfill.Queue.Enabled = true
		var logOut bytes.Buffer
		newDefaultsTestConnector(cfg, &logOut, batchSending).applyBridgeConfigDefaults()

		if !cfg.Backfill.Queue.Enabled {
			t.Errorf("batch_sending=%v: explicit backfill.queue.enabled=true was changed", batchSending)
		}
		if strings.Contains(logOut.String(), `"config_key":"backfill.queue.enabled"`) {
			t.Errorf("batch_sending=%v: logged a line for operator-set backfill.queue.enabled:\n%s", batchSending, logOut.String())
		}
	}
}

func TestStockDefaultRequires(t *testing.T) {
	var queue configDefault
	for _, d := range bridgeConfigDefaults {
		if sd, ok := d.(stockDefault[bool]); ok && sd.key == "backfill.queue.enabled" {
			queue = sd
		}
	}
	if queue == nil {
		t.Fatal("backfill.queue.enabled missing from bridgeConfigDefaults")
	}
	tests := []struct {
		name         string
		current      bool
		batchSending bool
		want         bool
		wantOutcome  defaultOutcome
	}{
		{"stock, no batch sending: skipped", false, false, false, defaultSkipped},
		{"stock, batch sending: applied", false, true, true, defaultApplied},
		{"explicit true, no batch sending: kept", true, false, true, defaultKept},
		{"explicit true, batch sending: kept", true, true, true, defaultKept},
	}
	for _, tt := range tests {
		cfg := mautrixStockBridgeConfig()
		cfg.Backfill.Queue.Enabled = tt.current
		_, outcome := queue.apply(cfg, homeserverFeatures{BatchSending: tt.batchSending})
		if cfg.Backfill.Queue.Enabled != tt.want || outcome != tt.wantOutcome {
			t.Errorf("%s: got (%v, %d), want (%v, %d)", tt.name, cfg.Backfill.Queue.Enabled, outcome, tt.want, tt.wantOutcome)
		}
	}
}

func TestApplyBridgeConfigDefaultsKeepsOperatorValues(t *testing.T) {
	cfg := mautrixStockBridgeConfig()
	cfg.Backfill.MaxInitialMessages = 1000
	cfg.Backfill.Queue.BatchSize = 500
	cfg.UnknownErrorAutoReconnect = 2 * time.Minute
	cfg.UnknownErrorMaxAutoReconnects = 5
	var logOut bytes.Buffer
	newDefaultsTestConnector(cfg, &logOut, true).applyBridgeConfigDefaults()

	if cfg.Backfill.MaxInitialMessages != 1000 {
		t.Errorf("backfill.max_initial_messages = %d, want 1000", cfg.Backfill.MaxInitialMessages)
	}
	if cfg.Backfill.Queue.BatchSize != 500 {
		t.Errorf("backfill.queue.batch_size = %d, want 500", cfg.Backfill.Queue.BatchSize)
	}
	if cfg.UnknownErrorAutoReconnect != 2*time.Minute {
		t.Errorf("unknown_error_auto_reconnect = %s, want 2m", cfg.UnknownErrorAutoReconnect)
	}
	if cfg.UnknownErrorMaxAutoReconnects != 5 {
		t.Errorf("unknown_error_max_auto_reconnects = %d, want 5", cfg.UnknownErrorMaxAutoReconnects)
	}
	for _, key := range []string{
		"backfill.max_initial_messages", "backfill.queue.batch_size",
		"bridge.unknown_error_auto_reconnect", "bridge.unknown_error_max_auto_reconnects",
	} {
		if strings.Contains(logOut.String(), `"config_key":"`+key+`"`) {
			t.Errorf("logged a default for operator-set %s", key)
		}
	}
	// Keys left at stock still get the bridge default.
	if cfg.Backfill.MaxCatchupMessages != math.MaxInt32 || !cfg.Backfill.Enabled ||
		!cfg.Backfill.Queue.Enabled || !cfg.PhoneNumbersInProfile {
		t.Errorf("stock keys not defaulted: %+v phone_numbers_in_profile=%v", cfg.Backfill, cfg.PhoneNumbersInProfile)
	}
}

func TestUnknownErrorAutoReconnectTooShort(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want bool
	}{
		{0, false},
		{30 * time.Second, true},
		{59 * time.Second, true},
		{time.Minute, false},
		{5 * time.Minute, false},
	}
	for _, tt := range tests {
		if got := unknownErrorAutoReconnectTooShort(tt.d); got != tt.want {
			t.Errorf("unknownErrorAutoReconnectTooShort(%s) = %v, want %v", tt.d, got, tt.want)
		}
	}
}

// mautrixVersion returns the mautrix version this module builds against: from
// the binary's build info when it lists dependencies (test binaries usually do
// not), otherwise from the require line in the module's go.mod.
func mautrixVersion() string {
	const modPath = "maunium.net/go/mautrix"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == modPath {
				if dep.Replace != nil {
					return dep.Replace.Version
				}
				return dep.Version
			}
		}
	}
	// go test runs in the package directory, two levels below go.mod.
	goMod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(goMod), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == modPath {
			return fields[1]
		}
	}
	return ""
}

// findMautrixExampleConfig locates mautrix's example-config.yaml in the module
// cache (go env GOMODCACHE).
func findMautrixExampleConfig() (string, bool) {
	version := mautrixVersion()
	if version == "" {
		return "", false
	}
	modCache := os.Getenv("GOMODCACHE")
	if modCache == "" {
		out, err := exec.Command("go", "env", "GOMODCACHE").Output()
		if err != nil {
			return "", false
		}
		modCache = strings.TrimSpace(string(out))
	}
	path := filepath.Join(modCache, "maunium.net", "go", "mautrix@"+version,
		"bridgev2", "matrix", "mxmain", "example-config.yaml")
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}

// TestMautrixStockValuesMatch checks the mautrixStock* constants against the
// example-config.yaml of the mautrix version in use, so a mautrix upgrade that
// changes a stock value is noticed.
func TestMautrixStockValuesMatch(t *testing.T) {
	path, ok := findMautrixExampleConfig()
	if !ok {
		t.Skip("mautrix example-config.yaml not found in the module cache")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var stock struct {
		Bridge struct {
			UnknownErrorAutoReconnect     time.Duration `yaml:"unknown_error_auto_reconnect"`
			UnknownErrorMaxAutoReconnects int           `yaml:"unknown_error_max_auto_reconnects"`
			PhoneNumbersInProfile         bool          `yaml:"phone_numbers_in_profile"`
		} `yaml:"bridge"`
		Backfill bridgeconfig.BackfillConfig `yaml:"backfill"`
	}
	if err = yaml.Unmarshal(data, &stock); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	checks := []struct {
		key       string
		got, want any
	}{
		{"bridge.unknown_error_auto_reconnect", stock.Bridge.UnknownErrorAutoReconnect, mautrixStockUnknownErrorAutoReconnect},
		{"bridge.unknown_error_max_auto_reconnects", stock.Bridge.UnknownErrorMaxAutoReconnects, mautrixStockUnknownErrorMaxAutoReconnects},
		{"bridge.phone_numbers_in_profile", stock.Bridge.PhoneNumbersInProfile, mautrixStockPhoneNumbersInProfile},
		{"backfill.enabled", stock.Backfill.Enabled, mautrixStockBackfillEnabled},
		{"backfill.max_initial_messages", stock.Backfill.MaxInitialMessages, mautrixStockMaxInitialMessages},
		{"backfill.max_catchup_messages", stock.Backfill.MaxCatchupMessages, mautrixStockMaxCatchupMessages},
		{"backfill.queue.enabled", stock.Backfill.Queue.Enabled, mautrixStockBackfillQueueEnabled},
		{"backfill.queue.batch_size", stock.Backfill.Queue.BatchSize, mautrixStockBackfillQueueBatchSize},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: mautrix stock value in %s is %v, constant says %v", c.key, path, c.got, c.want)
		}
	}
}
