// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

// safeLoginStart, safeSubmit2fa, and safeFinish wrap the interactive login FFI
// calls with panic recovery (ported from upstream dd691702). uniffi turns a
// Rust panic into a Go panic; without recovery an unexpected Apple response
// during one user's login would crash the process and take down every other
// user's session. With it, the panic surfaces as a failed login step the user
// can retry.
func safeLoginStart(
	username, password string,
	config *rustpushgo.WrappedOsConfig,
	conn *rustpushgo.WrappedApsConnection,
) (*rustpushgo.LoginSession, error) {
	return safeFFICall("LoginStart", func() (*rustpushgo.LoginSession, error) {
		return rustpushgo.LoginStart(username, password, config, conn)
	})
}

func safeSubmit2fa(session *rustpushgo.LoginSession, code string) (bool, error) {
	return safeFFICall("Submit2fa", func() (bool, error) {
		return session.Submit2fa(code)
	})
}

func safeFinish(
	session *rustpushgo.LoginSession,
	config *rustpushgo.WrappedOsConfig,
	conn *rustpushgo.WrappedApsConnection,
	existingIdentity **rustpushgo.WrappedIdsngmIdentity,
	existingUsers **rustpushgo.WrappedIdsUsers,
) (rustpushgo.IdsUsersWithIdentityRecord, error) {
	return safeFFICall("Finish", func() (rustpushgo.IdsUsersWithIdentityRecord, error) {
		return session.Finish(config, conn, existingIdentity, existingUsers)
	})
}
