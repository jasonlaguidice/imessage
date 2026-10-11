// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package connector

import (
	"context"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

// FaceTime is notify-only (ported from upstream a4b512ae). The bridge stays
// registered for the FaceTime IDS service so Apple keeps delivering call
// events; Rust maps an incoming ring or a missed call to a synthetic message
// whose text starts with one of these markers, and that notice is the whole
// of what the bridge does with it. There is no join link, no answering and no
// way to place a call from Matrix: answer on an Apple device. The web-join
// implementation this replaced is in git history (before the rustpush 48bcc27
// bump) for the future media-bridging research.
const (
	faceTimeRingMarker   = "[[FACETIME_RING]]"
	faceTimeMissedMarker = "[[FACETIME_MISSED]]"
)

// faceTimeNoticeKind is which call outcome a notice reports.
type faceTimeNoticeKind int

const (
	faceTimeNoticeRing faceTimeNoticeKind = iota
	faceTimeNoticeMissed
)

// parseFaceTimeNoticeKind reports whether text is a FaceTime event from Rust
// and, if so, which kind.
func parseFaceTimeNoticeKind(text string) (faceTimeNoticeKind, bool) {
	switch {
	case strings.HasPrefix(text, faceTimeRingMarker):
		return faceTimeNoticeRing, true
	case strings.HasPrefix(text, faceTimeMissedMarker):
		return faceTimeNoticeMissed, true
	default:
		return 0, false
	}
}

// faceTimeNoticeMarkdown renders the notice body for a caller display name.
func faceTimeNoticeMarkdown(kind faceTimeNoticeKind, callerName string) string {
	if callerName == "" {
		callerName = "someone"
	}
	if kind == faceTimeNoticeMissed {
		return "📞 **Missed FaceTime call from " + callerName + ".**"
	}
	return "📞 **Incoming FaceTime call from " + callerName + ".**"
}

func (kind faceTimeNoticeKind) logLabel() string {
	if kind == faceTimeNoticeMissed {
		return "FaceTimeMissed"
	}
	return "FaceTimeRing"
}

// faceTimeCallerName resolves the caller's display name from their ghost,
// falling back to the bare handle.
func (c *IMClient) faceTimeCallerName(ctx context.Context, senderHandle string) string {
	name := stripIdentifierPrefix(senderHandle)
	if senderHandle != "" {
		ghost, err := c.Main.Bridge.GetGhostByID(ctx, makeUserID(normalizeIdentifierForPortalID(senderHandle)))
		if err == nil && ghost != nil && ghost.Name != "" {
			name = ghost.Name
		}
	}
	return name
}

// postFaceTimeNotice posts an m.notice about an incoming or missed FaceTime
// call to the caller's portal, falling back to the user's existing management
// room. It never creates a portal or recreates a management room the user
// left. Suppressed per login by DisableFaceTime (see disableFaceTime()).
func (c *IMClient) postFaceTimeNotice(log zerolog.Logger, msg rustpushgo.WrappedMessage, kind faceTimeNoticeKind) {
	if c.disableFaceTime() {
		return
	}
	ctx := context.Background()
	senderHandle := ptrStringOr(msg.Sender, "")
	label := kind.logLabel()
	markdown := faceTimeNoticeMarkdown(kind, c.faceTimeCallerName(ctx, senderHandle))

	sendNotice := func(roomID id.RoomID) error {
		content := format.RenderMarkdown(markdown, true, false)
		content.MsgType = event.MsgNotice
		content.Mentions = &event.Mentions{}
		_, err := c.Main.Bridge.Bot.SendMessage(ctx, roomID, event.EventMessage, &event.Content{
			Parsed: &content,
		}, nil)
		return err
	}

	portalKey := c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid, msg.IsSms)
	portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
	if err == nil && portal != nil && portal.MXID != "" {
		sendErr := sendNotice(portal.MXID)
		if sendErr == nil {
			log.Info().
				Str("sender", senderHandle).
				Str("portal_mxid", string(portal.MXID)).
				Msg(label + ": posted notice to portal")
			return
		}
		log.Warn().Err(sendErr).Msg(label + ": failed to send portal notice")
	}

	mgmtRoom := c.existingManagementRoom()
	if mgmtRoom == "" {
		log.Debug().Msg(label + ": no portal and no management room, dropping notice")
		return
	}
	if sendErr := sendNotice(mgmtRoom); sendErr != nil {
		log.Warn().Err(sendErr).Msg(label + ": failed to send management room notice")
		return
	}
	log.Info().
		Str("sender", senderHandle).
		Str("management_room", string(mgmtRoom)).
		Msg(label + ": posted notice to management room")
}
