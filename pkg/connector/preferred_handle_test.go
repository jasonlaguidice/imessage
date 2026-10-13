// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2026 Jason LaGuidice
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import "testing"

func TestChoosePreferredHandle(t *testing.T) {
	const (
		phone = "tel:+15551234567"
		email = "mailto:user@example.com"
		other = "mailto:other@example.com"
	)
	handles := []string{phone, email}

	tests := []struct {
		name     string
		metaPref string
		cfgPref  string
		handles  []string
		want     string
	}{
		{"metadata wins over config", email, phone, handles, email},
		{"metadata used when config empty", email, "", handles, email},
		{"config used when metadata empty", "", email, handles, email},
		{"config used when metadata not registered", other, email, handles, email},
		{"metadata used when config not registered", email, other, handles, email},
		{"first handle when neither registered", other, "mailto:nope@example.com", handles, phone},
		{"first handle when both empty", "", "", handles, phone},
		{"no handles", email, phone, nil, ""},
		{"no handles empty slice", email, phone, []string{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := choosePreferredHandle(tt.metaPref, tt.cfgPref, tt.handles); got != tt.want {
				t.Errorf("choosePreferredHandle(%q, %q, %v) = %q, want %q",
					tt.metaPref, tt.cfgPref, tt.handles, got, tt.want)
			}
		})
	}
}

func TestShouldPersistHandle(t *testing.T) {
	tests := []struct {
		name     string
		metaPref string
		cfgPref  string
		want     bool
	}{
		{"first-handle fallback is persisted", "", "", true},
		{"user choice is never overwritten", "mailto:user@example.com", "", false},
		{"user choice with config is never overwritten", "mailto:user@example.com", "tel:+15551234567", false},
		{"config fallback is not frozen into metadata", "", "tel:+15551234567", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPersistHandle(tt.metaPref, tt.cfgPref); got != tt.want {
				t.Errorf("shouldPersistHandle(%q, %q) = %v, want %v", tt.metaPref, tt.cfgPref, got, tt.want)
			}
		})
	}
}
