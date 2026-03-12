package connector

import (
	"testing"

	"maunium.net/go/mautrix/event"
)

func TestCaps_NotNil(t *testing.T) {
	if caps == nil {
		t.Fatal("caps should not be nil")
	}
	if capsDM == nil {
		t.Fatal("capsDM should not be nil")
	}
	if generalCaps == nil {
		t.Fatal("generalCaps should not be nil")
	}
}

func TestCaps_ID(t *testing.T) {
	if caps.ID == "" {
		t.Error("caps.ID should not be empty")
	}
	if capsDM.ID == "" {
		t.Error("capsDM.ID should not be empty")
	}
	if caps.ID == capsDM.ID {
		t.Error("caps.ID and capsDM.ID should differ")
	}
}

func TestCaps_Features(t *testing.T) {
	if caps.Reply != event.CapLevelFullySupported {
		t.Errorf("caps.Reply = %v, want FullySupported", caps.Reply)
	}
	if caps.Edit != event.CapLevelFullySupported {
		t.Errorf("caps.Edit = %v, want FullySupported", caps.Edit)
	}
	if caps.Delete != event.CapLevelFullySupported {
		t.Errorf("caps.Delete = %v, want FullySupported", caps.Delete)
	}
	if caps.Reaction != event.CapLevelFullySupported {
		t.Errorf("caps.Reaction = %v, want FullySupported", caps.Reaction)
	}
	if caps.ReactionCount != 1 {
		t.Errorf("caps.ReactionCount = %d, want 1", caps.ReactionCount)
	}
	if !caps.ReadReceipts {
		t.Error("caps.ReadReceipts should be true")
	}
	if !caps.TypingNotifications {
		t.Error("caps.TypingNotifications should be true")
	}
	if !caps.DeleteChat {
		t.Error("caps.DeleteChat should be true")
	}
}

func TestCaps_FileTypes(t *testing.T) {
	for _, msgType := range []event.CapabilityMsgType{event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile, event.CapMsgGIF, event.CapMsgVoice} {
		if _, ok := caps.File[msgType]; !ok {
			t.Errorf("caps.File missing %v", msgType)
		}
	}
}

func TestCaps_Formatting(t *testing.T) {
	for _, fmt := range []event.FormattingFeature{event.FmtBold, event.FmtItalic, event.FmtUnderline, event.FmtStrikethrough} {
		if caps.Formatting[fmt] != event.CapLevelFullySupported {
			t.Errorf("caps.Formatting[%v] = %v, want FullySupported", fmt, caps.Formatting[fmt])
		}
	}
}

func TestCapsDM_NoGroupFeatures(t *testing.T) {
	// DM caps should not have room state or invite/kick
	if _, ok := capsDM.State[event.StateRoomName.Type]; ok {
		t.Error("capsDM should not have StateRoomName")
	}
	if _, ok := capsDM.State[event.StateRoomAvatar.Type]; ok {
		t.Error("capsDM should not have StateRoomAvatar")
	}
	if _, ok := capsDM.MemberActions[event.MemberActionInvite]; ok {
		t.Error("capsDM should not have MemberActionInvite")
	}
	if _, ok := capsDM.MemberActions[event.MemberActionKick]; ok {
		t.Error("capsDM should not have MemberActionKick")
	}
	// But group caps should have them
	if _, ok := caps.State[event.StateRoomName.Type]; !ok {
		t.Error("caps should have StateRoomName")
	}
	if _, ok := caps.MemberActions[event.MemberActionInvite]; !ok {
		t.Error("caps should have MemberActionInvite")
	}
}

func TestCapsDM_StillHasLeave(t *testing.T) {
	if _, ok := capsDM.MemberActions[event.MemberActionLeave]; !ok {
		t.Error("capsDM should still have MemberActionLeave")
	}
}

func TestGeneralCaps(t *testing.T) {
	if generalCaps.DisappearingMessages {
		t.Error("DisappearingMessages should be false")
	}
	if !generalCaps.AggressiveUpdateInfo {
		t.Error("AggressiveUpdateInfo should be true")
	}
}

func TestIMConnector_GetCapabilities(t *testing.T) {
	c := &IMConnector{}
	got := c.GetCapabilities()
	if got != generalCaps {
		t.Error("GetCapabilities should return generalCaps")
	}
}

func TestIMessageMaxFileSize(t *testing.T) {
	expected := 2000 * 1024 * 1024
	if iMessageMaxFileSize != expected {
		t.Errorf("iMessageMaxFileSize = %d, want %d", iMessageMaxFileSize, expected)
	}
}
