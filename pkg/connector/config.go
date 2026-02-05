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
}

func (c *IMConnector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &c.Config, up.SimpleUpgrader(upgradeConfig)
}
