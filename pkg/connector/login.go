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
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

const (
	LoginFlowIDAppleID       = "apple-id"
	LoginFlowIDExternalKey   = "external-key"
	LoginStepAppleIDPassword = "fi.mau.imessage.login.appleid"
	LoginStepExternalKey     = "fi.mau.imessage.login.externalkey"
	LoginStepTwoFactor       = "fi.mau.imessage.login.2fa"
	LoginStepComplete        = "fi.mau.imessage.login.complete"
)

// AppleIDLogin implements the multi-step login flow:
// Apple ID + password → 2FA code → IDS registration → connected.
type AppleIDLogin struct {
	User     *bridgev2.User
	Main     *IMConnector
	username string
	cfg      *rustpushgo.WrappedOsConfig
	conn     *rustpushgo.WrappedApsConnection
	session  *rustpushgo.LoginSession
}

var _ bridgev2.LoginProcessUserInput = (*AppleIDLogin)(nil)

func (l *AppleIDLogin) Cancel() {}

func (l *AppleIDLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	rustpushgo.InitLogger()

	cfg, err := rustpushgo.CreateLocalMacosConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize local NAC config: %w", err)
	}
	l.cfg = cfg
	l.conn = rustpushgo.Connect(cfg, rustpushgo.NewWrappedApsState(nil))

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

func (l *AppleIDLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	// Step 1: Apple ID + password
	if l.session == nil {
		username := input["username"]
		if username == "" {
			return nil, fmt.Errorf("Apple ID is required")
		}
		password := input["password"]
		if password == "" {
			return nil, fmt.Errorf("Password is required")
		}
		l.username = username

		session, err := rustpushgo.LoginStart(username, password, l.cfg, l.conn)
		if err != nil {
			l.Main.Bridge.Log.Error().Err(err).Str("username", username).Msg("Login failed")
			return nil, fmt.Errorf("login failed: %w", err)
		}
		l.session = session

		if session.Needs2fa() {
			l.Main.Bridge.Log.Info().Str("username", username).Msg("Login succeeded, waiting for 2FA")
			return &bridgev2.LoginStep{
				Type:   bridgev2.LoginStepTypeUserInput,
				StepID: LoginStepTwoFactor,
				Instructions: "Enter your Apple ID verification code.\n\n" +
					"You may see a notification on your trusted Apple devices. " +
					"If not, you can generate a code manually:\n" +
					"• iPhone/iPad: Settings → [Your Name] → Sign-In & Security → Two-Factor Authentication → Get Verification Code\n" +
					"• Mac: System Settings → [Your Name] → Sign-In & Security → Two-Factor Authentication → Get Verification Code",
				UserInputParams: &bridgev2.LoginUserInputParams{
					Fields: []bridgev2.LoginInputDataField{{
						ID:   "code",
						Name: "2FA Code",
					}},
				},
			}, nil
		}

		// No 2FA needed — skip straight to IDS registration
		l.Main.Bridge.Log.Info().Str("username", username).Msg("Login succeeded without 2FA, finishing registration")
		return l.finishLogin(ctx)
	}

	// Step 2: 2FA code
	code := input["code"]
	if code == "" {
		return nil, fmt.Errorf("2FA code is required")
	}

	success, err := l.session.Submit2fa(code)
	if err != nil {
		return nil, fmt.Errorf("2FA verification failed: %w", err)
	}
	if !success {
		return nil, fmt.Errorf("2FA verification failed — invalid code")
	}

	return l.finishLogin(ctx)
}

func (l *AppleIDLogin) finishLogin(ctx context.Context) (*bridgev2.LoginStep, error) {
	result, err := l.session.Finish(l.cfg, l.conn)
	if err != nil {
		return nil, fmt.Errorf("login completion failed: %w", err)
	}

	client := &IMClient{
		Main:          l.Main,
		config:        l.cfg,
		users:         result.Users,
		identity:      result.Identity,
		connection:    l.conn,
		recentUnsends: make(map[string]time.Time),
		smsPortals:    make(map[string]bool),
	}

	loginID := networkid.UserLoginID(result.Users.LoginId(0))

	meta := &UserLoginMetadata{
		Platform:    "rustpush-local",
		APSState:    l.conn.State().ToString(),
		IDSUsers:    result.Users.ToString(),
		IDSIdentity: result.Identity.ToString(),
		DeviceID:    l.cfg.GetDeviceId(),
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

	go client.Connect(context.Background())

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       LoginStepComplete,
		Instructions: "Successfully logged in to iMessage. Bridge is starting.",
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}

// ============================================================================
// External Key Login (cross-platform)
// ============================================================================

// ExternalKeyLogin implements the multi-step login flow for non-macOS platforms:
// Hardware key → Apple ID + password → 2FA code → IDS registration → connected.
type ExternalKeyLogin struct {
	User        *bridgev2.User
	Main        *IMConnector
	hardwareKey string
	username    string
	cfg         *rustpushgo.WrappedOsConfig
	conn        *rustpushgo.WrappedApsConnection
	session     *rustpushgo.LoginSession
}

var _ bridgev2.LoginProcessUserInput = (*ExternalKeyLogin)(nil)

func (l *ExternalKeyLogin) Cancel() {}

func (l *ExternalKeyLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepExternalKey,
		Instructions: "Enter your hardware key (base64-encoded JSON).\n\n" +
			"This is extracted once from a real Mac using the key extraction tool.\n" +
			"It contains hardware identifiers needed for iMessage registration.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type: bridgev2.LoginInputFieldTypePassword,
				ID:   "hardware_key",
				Name: "Hardware Key (base64)",
			}},
		},
	}, nil
}

func (l *ExternalKeyLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	// Step 1: Hardware key
	if l.cfg == nil {
		hwKey := input["hardware_key"]
		if hwKey == "" {
			return nil, fmt.Errorf("hardware key is required")
		}
		l.hardwareKey = hwKey

		rustpushgo.InitLogger()

		cfg, err := rustpushgo.CreateConfigFromHardwareKey(hwKey)
		if err != nil {
			return nil, fmt.Errorf("invalid hardware key: %w", err)
		}
		l.cfg = cfg
		l.conn = rustpushgo.Connect(cfg, rustpushgo.NewWrappedApsState(nil))

		return &bridgev2.LoginStep{
			Type:   bridgev2.LoginStepTypeUserInput,
			StepID: LoginStepAppleIDPassword,
			Instructions: "Enter your Apple ID credentials.\n" +
				"Registration uses the hardware key for NAC validation (no Mac needed at runtime).",
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

	// Step 2: Apple ID + password
	if l.session == nil {
		username := input["username"]
		if username == "" {
			return nil, fmt.Errorf("Apple ID is required")
		}
		password := input["password"]
		if password == "" {
			return nil, fmt.Errorf("Password is required")
		}
		l.username = username

		session, err := rustpushgo.LoginStart(username, password, l.cfg, l.conn)
		if err != nil {
			l.Main.Bridge.Log.Error().Err(err).Str("username", username).Msg("Login failed")
			return nil, fmt.Errorf("login failed: %w", err)
		}
		l.session = session

		if session.Needs2fa() {
			l.Main.Bridge.Log.Info().Str("username", username).Msg("Login succeeded, waiting for 2FA")
			return &bridgev2.LoginStep{
				Type:   bridgev2.LoginStepTypeUserInput,
				StepID: LoginStepTwoFactor,
				Instructions: "Enter your Apple ID verification code.\n\n" +
					"You may see a notification on your trusted Apple devices.",
				UserInputParams: &bridgev2.LoginUserInputParams{
					Fields: []bridgev2.LoginInputDataField{{
						ID:   "code",
						Name: "2FA Code",
					}},
				},
			}, nil
		}

		l.Main.Bridge.Log.Info().Str("username", username).Msg("Login succeeded without 2FA")
		return l.finishLogin(ctx)
	}

	// Step 3: 2FA code
	code := input["code"]
	if code == "" {
		return nil, fmt.Errorf("2FA code is required")
	}

	success, err := l.session.Submit2fa(code)
	if err != nil {
		return nil, fmt.Errorf("2FA verification failed: %w", err)
	}
	if !success {
		return nil, fmt.Errorf("2FA verification failed — invalid code")
	}

	return l.finishLogin(ctx)
}

func (l *ExternalKeyLogin) finishLogin(ctx context.Context) (*bridgev2.LoginStep, error) {
	result, err := l.session.Finish(l.cfg, l.conn)
	if err != nil {
		return nil, fmt.Errorf("login completion failed: %w", err)
	}

	client := &IMClient{
		Main:          l.Main,
		config:        l.cfg,
		users:         result.Users,
		identity:      result.Identity,
		connection:    l.conn,
		recentUnsends: make(map[string]time.Time),
		smsPortals:    make(map[string]bool),
	}

	loginID := networkid.UserLoginID(result.Users.LoginId(0))

	meta := &UserLoginMetadata{
		Platform:    "rustpush-external-key",
		APSState:    l.conn.State().ToString(),
		IDSUsers:    result.Users.ToString(),
		IDSIdentity: result.Identity.ToString(),
		DeviceID:    l.cfg.GetDeviceId(),
		HardwareKey: l.hardwareKey,
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

	go client.Connect(context.Background())

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       LoginStepComplete,
		Instructions: "Successfully logged in to iMessage via external key. Bridge is starting.",
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}
