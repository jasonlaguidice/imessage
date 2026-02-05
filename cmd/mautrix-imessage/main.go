// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Tulir Asokan, Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/lrhodin/imessage/pkg/connector"
)

var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

var m = mxmain.BridgeMain{
	Name:        "mautrix-imessage",
	URL:         "https://github.com/lrhodin/imessage",
	Description: "A Matrix-iMessage puppeting bridge (bridgev2).",
	Version:     "0.1.0",

	Connector: &connector.IMConnector{},
}

func main() {
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
