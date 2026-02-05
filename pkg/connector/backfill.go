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
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/lrhodin/imessage/imessage"
)

var _ bridgev2.BackfillingNetworkAPI = (*IMClient)(nil)

func (c *IMClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	if c.imAPI == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	chatGUID := string(params.Portal.ID)
	count := params.Count
	if count <= 0 {
		count = 50
	}

	var messages []*imessage.Message
	var err error

	if params.AnchorMessage != nil {
		if params.Forward {
			messages, err = c.imAPI.GetMessagesSinceDate(chatGUID, params.AnchorMessage.Timestamp, "")
		} else {
			messages, err = c.imAPI.GetMessagesBeforeWithLimit(chatGUID, params.AnchorMessage.Timestamp, count)
		}
	} else {
		messages, err = c.imAPI.GetMessagesWithLimit(chatGUID, count, "")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to fetch messages from iMessage: %w", err)
	}

	backfillMessages := make([]*bridgev2.BackfillMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.ItemType != imessage.ItemTypeMessage {
			continue
		}
		if msg.Tapback != nil {
			// Skip tapback messages during backfill for now
			continue
		}
		sender := c.makeEventSender(msg)
		cm, err := convertIMessage(ctx, params.Portal, nil, msg)
		if err != nil {
			continue
		}

		bm := &bridgev2.BackfillMessage{
			ConvertedMessage: cm,
			Sender:           sender,
			ID:               makeMessageID(msg.GUID),
			TxnID:            networkid.TransactionID(msg.GUID),
			Timestamp:        msg.Time,
			StreamOrder:      msg.Time.UnixMilli(),
		}
		backfillMessages = append(backfillMessages, bm)

		// Also add attachment messages
		for i, att := range msg.Attachments {
			if att == nil {
				continue
			}
			attMsg := &attachmentMessage{
				Message:    msg,
				Attachment: att,
				Index:      i,
			}
			attCm, err := convertAttachment(ctx, params.Portal, nil, attMsg)
			if err != nil {
				continue
			}
			partID := fmt.Sprintf("%s_att%d", msg.GUID, i)
			backfillMessages = append(backfillMessages, &bridgev2.BackfillMessage{
				ConvertedMessage: attCm,
				Sender:           sender,
				ID:               makeMessageID(partID),
				TxnID:            networkid.TransactionID(partID),
				Timestamp:        msg.Time.Add(time.Duration(i+1) * time.Millisecond),
				StreamOrder:      msg.Time.UnixMilli() + int64(i+1),
			})
		}
	}

	return &bridgev2.FetchMessagesResponse{
		Messages: backfillMessages,
		HasMore:  len(messages) >= count,
		Forward:  params.Forward,
		MarkRead: false,
	}, nil
}
