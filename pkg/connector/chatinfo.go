// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"context"
	"fmt"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/lrhodin/imessage/imessage"
)

func (c *IMClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	if c.imAPI == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	chatGUID := string(portal.ID)
	info, err := c.imAPI.GetChatInfo(chatGUID, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get chat info for %s: %w", chatGUID, err)
	}
	if info == nil {
		return nil, fmt.Errorf("chat %s not found", chatGUID)
	}

	return c.imChatInfoToBridgev2(info), nil
}

func (c *IMClient) imChatInfoToBridgev2(info *imessage.ChatInfo) *bridgev2.ChatInfo {
	parsed := imessage.ParseIdentifier(info.JSONChatGUID)
	if parsed.LocalID == "" {
		parsed = info.Identifier
	}

	chatInfo := &bridgev2.ChatInfo{
		Name:  &info.DisplayName,
		Topic: ptr.Ptr(""),
	}

	if parsed.IsGroup {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDefault)
		members := &bridgev2.ChatMemberList{
			IsFull: true,
			MemberMap: make(map[networkid.UserID]bridgev2.ChatMember),
		}
		// Add self
		selfSender := bridgev2.EventSender{
			IsFromMe:    true,
			SenderLogin: c.UserLogin.ID,
			Sender:      makeUserID(""),
		}
		members.MemberMap[selfSender.Sender] = bridgev2.ChatMember{
			EventSender: selfSender,
			Membership:  event.MembershipJoin,
		}
		// Add other members
		for _, memberID := range info.Members {
			userID := makeUserID(memberID)
			members.MemberMap[userID] = bridgev2.ChatMember{
				EventSender: bridgev2.EventSender{
					Sender: userID,
				},
				Membership: event.MembershipJoin,
			}
		}
		chatInfo.Members = members
	} else {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDM)
		otherUser := makeUserID(parsed.LocalID)
		selfSender := bridgev2.EventSender{
			IsFromMe:    true,
			SenderLogin: c.UserLogin.ID,
			Sender:      makeUserID(""),
		}
		members := &bridgev2.ChatMemberList{
			IsFull:      true,
			OtherUserID: otherUser,
			MemberMap: map[networkid.UserID]bridgev2.ChatMember{
				selfSender.Sender: {
					EventSender: selfSender,
					Membership:  event.MembershipJoin,
				},
				otherUser: {
					EventSender: bridgev2.EventSender{
						Sender: otherUser,
					},
					Membership: event.MembershipJoin,
				},
			},
		}
		chatInfo.Members = members
	}

	return chatInfo
}

func (c *IMClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	if c.imAPI == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	identifier := string(ghost.ID)
	if identifier == "" {
		return nil, nil
	}

	contact, err := c.imAPI.GetContactInfo(identifier)
	if err != nil {
		return nil, fmt.Errorf("failed to get contact info: %w", err)
	}

	return c.contactToUserInfo(identifier, contact), nil
}

func (c *IMClient) contactToUserInfo(identifier string, contact *imessage.Contact) *bridgev2.UserInfo {
	isBot := false
	ui := &bridgev2.UserInfo{
		IsBot:       &isBot,
		Identifiers: []string{},
	}

	if contact != nil && contact.HasName() {
		name := c.Main.Config.FormatDisplayname(DisplaynameParams{
			FirstName: contact.FirstName,
			LastName:  contact.LastName,
			Nickname:  contact.Nickname,
			ID:        identifier,
		})
		ui.Name = &name

		for _, phone := range contact.Phones {
			ui.Identifiers = append(ui.Identifiers, "tel:"+phone)
		}
		for _, email := range contact.Emails {
			ui.Identifiers = append(ui.Identifiers, "mailto:"+email)
		}

		if len(contact.Avatar) > 0 {
			ui.Avatar = &bridgev2.Avatar{
				ID: networkid.AvatarID(fmt.Sprintf("contact:%s", identifier)),
				Get: func(ctx context.Context) ([]byte, error) {
					return contact.Avatar, nil
				},
			}
		}
	} else {
		// No contact info — use the identifier as the name
		name := identifier
		if identifier[0] == '+' {
			name = c.Main.Config.FormatDisplayname(DisplaynameParams{
				Phone: identifier,
				ID:    identifier,
			})
		} else {
			name = c.Main.Config.FormatDisplayname(DisplaynameParams{
				Email: identifier,
				ID:    identifier,
			})
		}
		ui.Name = &name
		if identifier[0] == '+' {
			ui.Identifiers = append(ui.Identifiers, "tel:"+identifier)
		} else {
			ui.Identifiers = append(ui.Identifiers, "mailto:"+identifier)
		}
	}

	return ui
}
