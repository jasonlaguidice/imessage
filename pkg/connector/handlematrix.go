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
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/lrhodin/imessage/imessage"
)

var _ bridgev2.ReadReceiptHandlingNetworkAPI = (*IMClient)(nil)

func (c *IMClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if c.imAPI == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	chatID := string(msg.Portal.ID)
	text := msg.Content.Body

	// Check if this is a file/image message
	if msg.Content.URL != "" || msg.Content.File != nil {
		return c.handleMatrixFile(ctx, msg)
	}

	_, err := c.imAPI.SendMessage(chatID, text, "", 0, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to send iMessage: %w", err)
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        makeMessageID(fmt.Sprintf("mx_%d", time.Now().UnixNano())),
			SenderID:  makeUserID(""),
			Timestamp: time.Now(),
			Metadata:  &MessageMetadata{},
		},
	}, nil
}

func (c *IMClient) handleMatrixFile(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	chatID := string(msg.Portal.ID)

	// Download the file from Matrix
	var data []byte
	var err error
	if msg.Content.File != nil {
		data, err = c.Main.Bridge.Bot.DownloadMedia(ctx, msg.Content.File.URL, msg.Content.File)
	} else {
		data, err = c.Main.Bridge.Bot.DownloadMedia(ctx, msg.Content.URL, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to download media: %w", err)
	}

	fileName := msg.Content.Body
	if fileName == "" {
		fileName = "file"
	}

	// Write to temp file for iMessage to send
	dir, filePath, err := imessage.SendFilePrepare(fileName, data)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare file for sending: %w", err)
	}

	mimeType := ""
	if msg.Content.Info != nil {
		mimeType = msg.Content.Info.MimeType
	}

	caption := ""
	if msg.Content.FileName != "" && msg.Content.FileName != msg.Content.Body {
		caption = msg.Content.Body
	}

	_, err = c.imAPI.SendFile(chatID, caption, fileName, filePath, "", 0, mimeType, false, nil)
	if err != nil {
		c.imAPI.SendFileCleanup(dir)
		return nil, fmt.Errorf("failed to send file via iMessage: %w", err)
	}
	c.imAPI.SendFileCleanup(dir)

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        makeMessageID(fmt.Sprintf("mx_%d", time.Now().UnixNano())),
			SenderID:  makeUserID(""),
			Timestamp: time.Now(),
			Metadata:  &MessageMetadata{HasAttachments: true},
		},
	}, nil
}

func (c *IMClient) HandleMatrixReadReceipt(ctx context.Context, receipt *bridgev2.MatrixReadReceipt) error {
	if c.imAPI == nil {
		return nil
	}
	// The mac connector's SendReadReceipt is a no-op, but call it anyway for completeness
	chatID := string(receipt.Portal.ID)
	if receipt.ExactMessage != nil {
		return c.imAPI.SendReadReceipt(chatID, string(receipt.ExactMessage.ID))
	}
	return nil
}

// These are stubs since the mac connector doesn't support these features:

func (c *IMClient) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	return fmt.Errorf("editing is not supported by the iMessage mac connector")
}

func (c *IMClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	return fmt.Errorf("message deletion is not supported by the iMessage mac connector")
}

func (c *IMClient) PreHandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	return bridgev2.MatrixReactionPreResponse{
		SenderID: makeUserID(""),
		EmojiID:  networkid.EmojiID(""),
		Emoji:    msg.Content.RelatesTo.Key,
	}, nil
}

func (c *IMClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	// The mac connector's SendTapback is a no-op
	return nil, fmt.Errorf("reactions/tapbacks cannot be sent via the iMessage mac connector")
}

func (c *IMClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	return fmt.Errorf("reaction removal is not supported by the iMessage mac connector")
}
