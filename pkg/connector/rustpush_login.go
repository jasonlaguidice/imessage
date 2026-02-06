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
	"runtime"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

const (
	LoginFlowIDRustpush      = "rustpush"
	LoginStepAppleIDPassword = "fi.mau.imessage.rustpush.appleid"
	LoginStepTwoFactor       = "fi.mau.imessage.rustpush.2fa"
	LoginStepRustpushDone    = "fi.mau.imessage.rustpush.complete"
)

// RustpushLogin implements the multi-step login flow for rustpush.
type RustpushLogin struct {
	User     *bridgev2.User
	Main     *IMConnector
	username string
	useLocal bool // true = LocalMacOSConfig (no relay), false = relay
	cfg      *rustpushgo.WrappedOsConfig
	conn     *rustpushgo.WrappedApsConnection
	session  *rustpushgo.LoginSession
	deviceID string
}

var _ bridgev2.LoginProcessUserInput = (*RustpushLogin)(nil)

func (l *RustpushLogin) Cancel() {}

func (l *RustpushLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	// Initialize the Rust logger and keystore (must happen before any rustpush calls)
	rustpushgo.InitLogger()

	// On macOS, try local config first (no relay needed)
	if runtime.GOOS == "darwin" {
		cfg, err := rustpushgo.CreateLocalMacosConfig()
		if err == nil {
			l.cfg = cfg
			l.useLocal = true

			// Connect to APNs
			l.conn = rustpushgo.Connect(cfg, rustpushgo.NewWrappedApsState(nil))

			// Go directly to Apple ID step
			return &bridgev2.LoginStep{
				Type:   bridgev2.LoginStepTypeUserInput,
				StepID: LoginStepAppleIDPassword,
				Instructions: "Enter your Apple ID credentials. " +
					"Registration uses local NAC (no relay needed).",
				UserInputParams: &bridgev2.LoginUserInputParams{
					Fields: []bridgev2.LoginInputDataField{{
						Type: bridgev2.LoginInputFieldTypeEmail,
						ID:   "username",
						Name: "Apple ID",
					}, {
						Type: bridgev2.LoginInputFieldTypePassword,
						ID:   "password",
						Name: "Password",
					}},
				},
			}, nil
		}
		// Fall through to relay if local NAC failed
	}

	// Non-macOS or local NAC failed: need a relay code
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepAppleIDPassword,
		Instructions: "Enter your Apple ID credentials and a registration relay code.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				ID:          "relay_code",
				Name:        "Registration Relay Code",
				Description: "e.g., XXXX-XXXX-XXXX-XXXX",
			}, {
				Type: bridgev2.LoginInputFieldTypeEmail,
				ID:   "username",
				Name: "Apple ID",
			}, {
				Type: bridgev2.LoginInputFieldTypePassword,
				ID:   "password",
				Name: "Password",
			}},
		},
	}, nil
}

func (l *RustpushLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	// Step: Apple ID + password (with optional relay code on non-macOS)
	if l.session == nil {
		username, ok := input["username"]
		if !ok || username == "" {
			return nil, fmt.Errorf("Apple ID is required")
		}
		password, ok := input["password"]
		if !ok || password == "" {
			return nil, fmt.Errorf("Password is required")
		}
		l.username = username

		// Set up config if not already done (relay path)
		if l.cfg == nil {
			relayCode, ok := input["relay_code"]
			if !ok || relayCode == "" {
				return nil, fmt.Errorf("Registration relay code is required (not running on macOS)")
			}
			cfg, err := rustpushgo.CreateRelayConfig(relayCode)
			if err != nil {
				return nil, fmt.Errorf("invalid relay code: %w", err)
			}
			l.cfg = cfg
			l.conn = rustpushgo.Connect(cfg, rustpushgo.NewWrappedApsState(nil))
		}

		session, err := rustpushgo.LoginStart(username, password, l.cfg, l.conn)
		if err != nil {
			l.Main.Bridge.Log.Error().Err(err).Str("username", username).Msg("Rustpush login failed")
			return nil, fmt.Errorf("login failed: %w", err)
		}
		l.Main.Bridge.Log.Info().Str("username", username).Msg("Rustpush login_start succeeded, waiting for 2FA")
		l.session = session

		// Ask for 2FA code
		return &bridgev2.LoginStep{
			Type:   bridgev2.LoginStepTypeUserInput,
			StepID: LoginStepTwoFactor,
			Instructions: "Enter the 2FA code sent to your trusted device or phone.",
			UserInputParams: &bridgev2.LoginUserInputParams{
				Fields: []bridgev2.LoginInputDataField{{
					ID:   "code",
					Name: "2FA Code",
				}},
			},
		}, nil
	}

	// Step: 2FA code
	code, ok := input["code"]
	if !ok || code == "" {
		return nil, fmt.Errorf("2FA code is required")
	}

	success, err := l.session.Submit2fa(code)
	if err != nil {
		return nil, fmt.Errorf("2FA verification failed: %w", err)
	}
	if !success {
		return nil, fmt.Errorf("2FA verification failed - invalid code")
	}

	// Finish login: authenticate with IDS and register
	result, err := l.session.Finish(l.cfg, l.conn)
	if err != nil {
		return nil, fmt.Errorf("login completion failed: %w", err)
	}

	// Create the client
	client := &RustpushClient{
		Main:       l.Main,
		config:     l.cfg,
		users:      result.Users,
		identity:   result.Identity,
		connection: l.conn,
	}

	// Determine login ID from the first user ID
	loginID := networkid.UserLoginID(result.Users.LoginId(0))

	meta := &UserLoginMetadata{
		Platform:    "rustpush",
		APSState:    l.conn.State().ToString(),
		IDSUsers:    result.Users.ToString(),
		IDSIdentity: result.Identity.ToString(),
		DeviceID:    l.cfg.GetDeviceId(),
	}
	if l.useLocal {
		meta.Platform = "rustpush-local"
	}

	ul, err := l.User.NewLogin(ctx, &database.UserLogin{
		ID:         loginID,
		RemoteName: l.username,
		RemoteProfile: status.RemoteProfile{
			Name: l.username,
		},
		Metadata: meta,
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: true,
		LoadUserLogin: func(ctx context.Context, login *bridgev2.UserLogin) error {
			client.UserLogin = login
			login.Client = client
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create user login: %w", err)
	}

	// Start the client
	go client.Connect(context.Background())

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       LoginStepRustpushDone,
		Instructions: "Successfully logged in to iMessage via rustpush. Bridge is starting.",
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}
