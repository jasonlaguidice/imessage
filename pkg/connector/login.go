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

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/lrhodin/imessage/imessage/mac"
)

const (
	LoginFlowIDVerify  = "verify"
	LoginStepVerify    = "fi.mau.imessage.login.verify"
	LoginStepComplete  = "fi.mau.imessage.login.complete"
)

func (c *IMConnector) GetLoginFlows() []bridgev2.LoginFlow {
	flows := []bridgev2.LoginFlow{{
		Name:        "Verify",
		Description: "Verify iMessage access on this Mac (Full Disk Access required)",
		ID:          LoginFlowIDVerify,
	}, {
		Name:        "Apple ID (rustpush)",
		Description: "Login with Apple ID via rustpush protocol (no SIP disable needed)",
		ID:          LoginFlowIDRustpush,
	}}
	return flows
}

func (c *IMConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	switch flowID {
	case LoginFlowIDVerify:
		return &IMLogin{User: user, Main: c}, nil
	case LoginFlowIDRustpush:
		return &RustpushLogin{User: user, Main: c}, nil
	default:
		return nil, fmt.Errorf("invalid login flow ID: %s", flowID)
	}
}

type IMLogin struct {
	User *bridgev2.User
	Main *IMConnector
}

var _ bridgev2.LoginProcessDisplayAndWait = (*IMLogin)(nil)

func (l *IMLogin) Cancel() {}

func (l *IMLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	// Check if iMessage database is accessible
	err := mac.CheckPermissions()
	if err != nil {
		return &bridgev2.LoginStep{
			Type:         bridgev2.LoginStepTypeDisplayAndWait,
			StepID:       LoginStepVerify,
			Instructions: fmt.Sprintf("iMessage access check failed: %v. Grant Full Disk Access to this process, then try again.", err),
			DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{
				Type: bridgev2.LoginDisplayTypeNothing,
			},
		}, nil
	}

	// Access verified — create the login
	ul, err := l.User.NewLogin(ctx, &database.UserLogin{
		ID:         makeUserLoginID(),
		RemoteName: "iMessage",
		RemoteProfile: status.RemoteProfile{
			Name: "iMessage",
		},
		Metadata: &UserLoginMetadata{},
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create user login: %w", err)
	}

	// Start the client
	client := ul.Client.(*IMClient)
	go client.Connect(context.Background())

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       LoginStepComplete,
		Instructions: "Successfully verified iMessage access. Bridge is starting.",
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}

func (l *IMLogin) Wait(ctx context.Context) (*bridgev2.LoginStep, error) {
	// If Start didn't complete the login, the user needs to fix permissions and retry
	return l.Start(ctx)
}
