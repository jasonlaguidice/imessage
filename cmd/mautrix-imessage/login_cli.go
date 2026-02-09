// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
	"maunium.net/go/mautrix/id"
)

func prompt(label string) string {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

func promptPassword(label string) string {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

// runInteractiveLogin drives the bridge's login flow from the terminal.
// It reuses the exact same CreateLogin → SubmitUserInput code path as the
// Matrix bot, but reads input from stdin instead of Matrix messages.
func runInteractiveLogin(br *mxmain.BridgeMain) {
	// Initialize the bridge (DB, connector, etc.) without starting Matrix.
	br.PreInit()
	br.Init()

	ctx := br.Log.WithContext(context.Background())

	// Find the admin user from permissions config.
	userMXID := findAdminUser(br)
	if userMXID == "" {
		fmt.Fprintln(os.Stderr, "[!] No admin user found in config permissions. Cannot log in.")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[*] Logging in as %s\n", userMXID)

	user, err := br.Bridge.GetUserByMXID(ctx, userMXID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[!] Failed to get user: %v\n", err)
		os.Exit(1)
	}

	// Pick login flow: prefer external-key on Linux, apple-id on macOS.
	flows := br.Bridge.Network.GetLoginFlows()
	var flowID string
	for _, f := range flows {
		if f.ID == "apple-id" {
			flowID = f.ID // prefer if available (macOS)
		}
	}
	if flowID == "" && len(flows) > 0 {
		flowID = flows[0].ID
	}
	if flowID == "" {
		fmt.Fprintln(os.Stderr, "[!] No login flows available")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[*] Using login flow: %s\n", flowID)

	login, err := br.Bridge.Network.CreateLogin(ctx, user, flowID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[!] Failed to create login: %v\n", err)
		os.Exit(1)
	}

	step, err := login.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[!] Failed to start login: %v\n", err)
		os.Exit(1)
	}

	// Drive the multi-step login flow interactively.
	userInput, ok := login.(bridgev2.LoginProcessUserInput)
	if !ok {
		fmt.Fprintln(os.Stderr, "[!] Login flow does not support user input")
		os.Exit(1)
	}

	for step.Type != bridgev2.LoginStepTypeComplete {
		if step.Instructions != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n\n", step.Instructions)
		}

		switch step.Type {
		case bridgev2.LoginStepTypeUserInput:
			input := make(map[string]string)
			for _, field := range step.UserInputParams.Fields {
				if field.Type == bridgev2.LoginInputFieldTypePassword {
					input[field.ID] = promptPassword(field.Name)
				} else {
					input[field.ID] = prompt(field.Name)
				}
			}
			step, err = userInput.SubmitUserInput(ctx, input)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[!] Login step failed: %v\n", err)
				os.Exit(1)
			}
		default:
			fmt.Fprintf(os.Stderr, "[!] Unsupported login step type: %s\n", step.Type)
			os.Exit(1)
		}
	}

	fmt.Fprintf(os.Stderr, "\n[+] %s\n", step.Instructions)
	fmt.Fprintf(os.Stderr, "[+] Login ID: %s\n", step.CompleteParams.UserLoginID)

	// Clean shutdown.
	os.Exit(0)
}

// findAdminUser returns the first user MXID with admin permissions.
func findAdminUser(br *mxmain.BridgeMain) id.UserID {
	for userID, perm := range br.Config.Bridge.Permissions {
		if perm.Admin {
			return id.UserID(userID)
		}
	}
	return ""
}
