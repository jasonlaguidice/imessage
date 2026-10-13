// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2026 Jason LaGuidice
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"errors"
	"testing"

	"maunium.net/go/mautrix/event"
)

func TestShouldReuseAlbumRoom(t *testing.T) {
	member := func(m event.Membership) *event.MemberEventContent {
		return &event.MemberEventContent{Membership: m}
	}
	tests := []struct {
		name   string
		member *event.MemberEventContent
		err    error
		want   bool
	}{
		{"joined", member(event.MembershipJoin), nil, true},
		{"invite pending", member(event.MembershipInvite), nil, true},
		{"left", member(event.MembershipLeave), nil, false},
		{"banned", member(event.MembershipBan), nil, false},
		{"knocking", member(event.MembershipKnock), nil, false},
		{"unknown member", nil, nil, false},
		{"lookup failed", nil, errors.New("db down"), true},
	}
	for _, tt := range tests {
		if got := shouldReuseAlbumRoom(tt.member, tt.err); got != tt.want {
			t.Errorf("%s: shouldReuseAlbumRoom = %v, want %v", tt.name, got, tt.want)
		}
	}
}
