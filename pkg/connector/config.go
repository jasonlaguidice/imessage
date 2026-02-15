// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	_ "embed"
	"strings"
	"text/template"

	up "go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"
)

//go:embed example-config.yaml
var ExampleConfig string

type IMConfig struct {
	DisplaynameTemplate string `yaml:"displayname_template"`
	displaynameTemplate *template.Template

	// InitialSyncDays is how far back to look for chats during initial sync.
	// Default is 365 (1 year).
	InitialSyncDays int `yaml:"initial_sync_days"`

	// PreferredHandle overrides the outgoing iMessage identity.
	// Use the full URI format: "tel:+15551234567" or "mailto:user@example.com".
	// If empty, the handle chosen during login is used.
	PreferredHandle string `yaml:"preferred_handle"`

	// CardDAV is an external CardDAV server for contact name resolution.
	// When configured, this is used instead of iCloud CardDAV contacts.
	CardDAV CardDAVConfig `yaml:"carddav"`
}

// CardDAVConfig configures an external CardDAV server for contact name resolution.
// Supports Google (with app passwords), Nextcloud, Radicale, Fastmail, etc.
type CardDAVConfig struct {
	// Email address used for CardDAV auto-discovery (RFC 6764 .well-known/carddav).
	// Also used as the username if Username is empty.
	Email string `yaml:"email"`

	// URL is the CardDAV server URL. Leave empty to auto-discover from Email.
	// Example: https://www.googleapis.com/carddav/v1/principals/you@gmail.com/lists/default/
	URL string `yaml:"url"`

	// Username for HTTP Basic authentication. Defaults to Email if empty.
	Username string `yaml:"username"`

	// PasswordEncrypted is the AES-256-GCM encrypted app password (base64).
	// Set by the install script via the carddav-setup subcommand.
	PasswordEncrypted string `yaml:"password_encrypted"`
}

// IsConfigured returns true if the CardDAV config has enough info to connect.
func (c *CardDAVConfig) IsConfigured() bool {
	return c.Email != "" && c.PasswordEncrypted != ""
}

// GetUsername returns the effective username (falls back to Email).
func (c *CardDAVConfig) GetUsername() string {
	if c.Username != "" {
		return c.Username
	}
	return c.Email
}

type umIMConfig IMConfig

func (c *IMConfig) UnmarshalYAML(node *yaml.Node) error {
	err := node.Decode((*umIMConfig)(c))
	if err != nil {
		return err
	}
	return c.PostProcess()
}

func (c *IMConfig) PostProcess() error {
	var err error
	c.displaynameTemplate, err = template.New("displayname").Parse(c.DisplaynameTemplate)
	return err
}

type DisplaynameParams struct {
	FirstName string
	LastName  string
	Nickname  string
	Phone     string
	Email     string
	ID        string
}

func (c *IMConfig) FormatDisplayname(params DisplaynameParams) string {
	var buf strings.Builder
	err := c.displaynameTemplate.Execute(&buf, &params)
	if err != nil {
		return params.ID
	}
	name := strings.TrimSpace(buf.String())
	if name == "" {
		return params.ID
	}
	return name
}

func upgradeConfig(helper up.Helper) {
	helper.Copy(up.Str, "displayname_template")
	helper.Copy(up.Int, "initial_sync_days")
	helper.Copy(up.Str, "preferred_handle")
	helper.Copy(up.Str, "carddav", "email")
	helper.Copy(up.Str, "carddav", "url")
	helper.Copy(up.Str, "carddav", "username")
	helper.Copy(up.Str, "carddav", "password_encrypted")
}

// GetInitialSyncDays returns the configured initial sync window in days,
// defaulting to 365 (1 year) if not set.
func (c *IMConfig) GetInitialSyncDays() int {
	if c.InitialSyncDays <= 0 {
		return 365
	}
	return c.InitialSyncDays
}

func (c *IMConnector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &c.Config, up.SimpleUpgrader(upgradeConfig)
}
