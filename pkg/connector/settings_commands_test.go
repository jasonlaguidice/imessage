// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2026 Jason LaGuidice
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

func boolPtrString(v *bool) string {
	switch {
	case v == nil:
		return "nil"
	case *v:
		return "true"
	default:
		return "false"
	}
}

func TestParseToggleArg(t *testing.T) {
	cases := map[string]toggleArg{
		"":         toggleQuery,
		"   ":      toggleQuery,
		"on":       toggleOn,
		" ON ":     toggleOn,
		"yes":      toggleOn,
		"enable":   toggleOn,
		"true":     toggleOn,
		"1":        toggleOn,
		"off":      toggleOff,
		"Off":      toggleOff,
		"no":       toggleOff,
		"disable":  toggleOff,
		"false":    toggleOff,
		"0":        toggleOff,
		"reset":    toggleReset,
		"default":  toggleReset,
		"unset":    toggleReset,
		"maybe":    toggleInvalid,
		"on off":   toggleInvalid,
		"topic":    toggleInvalid,
		"enabled!": toggleInvalid,
	}
	for in, want := range cases {
		if got := parseToggleArg(in); got != want {
			t.Errorf("parseToggleArg(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestToggleArgOverride(t *testing.T) {
	if got := toggleOn.override(); got == nil || !*got {
		t.Errorf("on -> %s, want true", boolPtrString(got))
	}
	if got := toggleOff.override(); got == nil || *got {
		t.Errorf("off -> %s, want false", boolPtrString(got))
	}
	if got := toggleReset.override(); got != nil {
		t.Errorf("reset -> %s, want nil", boolPtrString(got))
	}
}

// TestFaceTimeNoticesMapping pins the inverted storage: "on" (notices posted)
// stores DisableFaceTime=false, "off" stores true, reset stores nil, and the
// stored value reads back as the value the user set.
func TestFaceTimeNoticesMapping(t *testing.T) {
	cases := []struct {
		arg         string
		wantDisable *bool
	}{
		{"on", boolPtr(false)},
		{"off", boolPtr(true)},
		{"reset", nil},
	}
	for _, tc := range cases {
		notices := parseToggleArg(tc.arg).override()
		disable := faceTimeNoticesToDisableFaceTime(notices)
		if boolPtrString(disable) != boolPtrString(tc.wantDisable) {
			t.Errorf("%s: DisableFaceTime = %s, want %s", tc.arg, boolPtrString(disable), boolPtrString(tc.wantDisable))
		}
		if back := disableFaceTimeToFaceTimeNotices(disable); boolPtrString(back) != boolPtrString(notices) {
			t.Errorf("%s: reads back as %s, want %s", tc.arg, boolPtrString(back), boolPtrString(notices))
		}
	}
}

func newSettingsTestClient(cfg IMConfig, meta *UserLoginMetadata) *IMClient {
	return &IMClient{
		Main: &IMConnector{Config: cfg},
		UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{
			ID:       "login-a",
			Metadata: meta,
		}},
	}
}

// TestFaceTimeNoticesCommandDrivesDisableFaceTime runs the stored value
// through disableFaceTime(), which postFaceTimeNotice checks per call.
func TestFaceTimeNoticesCommandDrivesDisableFaceTime(t *testing.T) {
	for _, global := range []bool{false, true} {
		cases := []struct {
			arg          string
			wantSuppress bool
		}{
			{"on", false},
			{"off", true},
			{"reset", global},
		}
		for _, tc := range cases {
			meta := &UserLoginMetadata{DisableFaceTime: faceTimeNoticesToDisableFaceTime(parseToggleArg(tc.arg).override())}
			c := newSettingsTestClient(IMConfig{DisableFaceTime: global}, meta)
			if got := c.disableFaceTime(); got != tc.wantSuppress {
				t.Errorf("global disable=%v, %s: disableFaceTime() = %v, want %v", global, tc.arg, got, tc.wantSuppress)
			}
		}
	}
}

func TestParseStatusKitStyleArg(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want styleArg
	}{
		{"", true, styleArg{query: true}},
		{"topic", true, styleArg{style: "topic"}},
		{" Topic ", true, styleArg{style: "topic"}},
		{"NOTICE", true, styleArg{style: "notice"}},
		{"reset", true, styleArg{reset: true}},
		{"default", true, styleArg{reset: true}},
		{"unset", true, styleArg{reset: true}},
		{"on", false, styleArg{}},
		{"banner", false, styleArg{}},
	}
	for _, tc := range cases {
		got, ok := parseStatusKitStyleArg(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseStatusKitStyleArg(%q) = %+v, %v; want %+v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestStatusKitStyleRoundTrip checks a stored style is what
// statusKitNotificationStyle() returns, and reset falls back to config.
func TestStatusKitStyleRoundTrip(t *testing.T) {
	for _, in := range []string{"topic", "notice", "Notice"} {
		arg, _ := parseStatusKitStyleArg(in)
		style := arg.style
		c := newSettingsTestClient(IMConfig{StatusKitNotificationStyle: "topic"}, &UserLoginMetadata{StatusKitNotificationStyle: &style})
		want, _ := normalizeStatusKitNotificationStyle(in)
		if got := c.statusKitNotificationStyle(); got != want {
			t.Errorf("%q: style = %q, want %q", in, got, want)
		}
	}
	c := newSettingsTestClient(IMConfig{StatusKitNotificationStyle: "notice"}, &UserLoginMetadata{})
	if got := c.statusKitNotificationStyle(); got != "notice" {
		t.Errorf("reset: style = %q, want config's notice", got)
	}
}

func TestStatusKitToggleDrivesAccessor(t *testing.T) {
	for _, global := range []bool{false, true} {
		for arg, want := range map[string]bool{"on": true, "off": false, "reset": global} {
			c := newSettingsTestClient(IMConfig{StatusKitNotifications: global},
				&UserLoginMetadata{StatusKitNotifications: parseToggleArg(arg).override()})
			if got := c.statusKitNotifications(); got != want {
				t.Errorf("global=%v %s: statusKitNotifications() = %v, want %v", global, arg, got, want)
			}
		}
	}
}

func TestEffectiveCloudKitBackfillMatchesAccessor(t *testing.T) {
	for _, global := range []bool{false, true} {
		for _, override := range []*bool{nil, boolPtr(true), boolPtr(false)} {
			want := global && (override == nil || *override)
			if got := effectiveCloudKitBackfill(global, override); got != want {
				t.Errorf("effective(%v, %s) = %v, want %v", global, boolPtrString(override), got, want)
			}
			c := newSettingsTestClient(IMConfig{CloudKitBackfill: global, BackfillSource: "cloudkit"},
				&UserLoginMetadata{CloudKitBackfill: override})
			if got := c.useCloudKitBackfill(); got != want {
				t.Errorf("useCloudKitBackfill(global=%v, %s) = %v, want %v", global, boolPtrString(override), got, want)
			}
		}
	}
	// backfill_source: chatdb disables CloudKit regardless of the override.
	c := newSettingsTestClient(IMConfig{CloudKitBackfill: true, BackfillSource: "chatdb"},
		&UserLoginMetadata{CloudKitBackfill: boolPtr(true)})
	if c.useCloudKitBackfill() {
		t.Error("chatdb source: useCloudKitBackfill() = true, want false")
	}
}

func TestPlanCloudKitBackfillChange(t *testing.T) {
	joined := func() bool { return true }
	notJoined := func() bool { return false }
	mustNotAsk := func() bool {
		t.Helper()
		t.Error("keychain state consulted for a change that doesn't turn backfill on")
		return false
	}
	cases := []struct {
		name          string
		global        bool
		current       *bool
		requested     *bool
		keychain      func() bool
		wantVerdict   cloudKitBackfillVerdict
		wantAfter     bool
		wantReconnect bool
	}{
		{"global off, on refused", false, nil, boolPtr(true), mustNotAsk, cloudKitBackfillRefuseGlobalOff, false, false},
		{"global off, off allowed, no-op", false, nil, boolPtr(false), mustNotAsk, cloudKitBackfillAllowed, false, false},
		{"global off, reset allowed, no-op", false, boolPtr(false), nil, mustNotAsk, cloudKitBackfillAllowed, false, false},
		{"off -> on, joined, reconnect", true, boolPtr(false), boolPtr(true), joined, cloudKitBackfillAllowed, true, true},
		{"off -> on, not joined, refused", true, boolPtr(false), boolPtr(true), notJoined, cloudKitBackfillRefuseNoKeychain, true, false},
		{"off -> reset, not joined, refused", true, boolPtr(false), nil, notJoined, cloudKitBackfillRefuseNoKeychain, true, false},
		{"off -> reset, joined, reconnect", true, boolPtr(false), nil, joined, cloudKitBackfillAllowed, true, true},
		{"on -> off, reconnect", true, boolPtr(true), boolPtr(false), mustNotAsk, cloudKitBackfillAllowed, false, true},
		{"default -> off, reconnect", true, nil, boolPtr(false), mustNotAsk, cloudKitBackfillAllowed, false, true},
		{"default -> on, no change", true, nil, boolPtr(true), mustNotAsk, cloudKitBackfillAllowed, true, false},
		{"off -> off, no change", true, boolPtr(false), boolPtr(false), mustNotAsk, cloudKitBackfillAllowed, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planCloudKitBackfillChange(tc.global, tc.current, tc.requested, tc.keychain)
			if plan.verdict != tc.wantVerdict {
				t.Errorf("verdict = %d, want %d", plan.verdict, tc.wantVerdict)
			}
			if plan.after != tc.wantAfter {
				t.Errorf("after = %v, want %v", plan.after, tc.wantAfter)
			}
			if plan.reconnect() != tc.wantReconnect {
				t.Errorf("reconnect = %v, want %v", plan.reconnect(), tc.wantReconnect)
			}
		})
	}
}

func TestSettingsCommandsRegistered(t *testing.T) {
	names := map[string]bool{}
	for _, h := range BridgeCommands() {
		names[h.Name] = true
	}
	for _, want := range []string{"set-facetime-notices", "set-statuskit", "set-statuskit-style", "set-cloudkit-backfill"} {
		if !names[want] {
			t.Errorf("%s not registered in BridgeCommands()", want)
		}
	}
}
