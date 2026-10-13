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
	"maunium.net/go/mautrix/bridgev2/commands"
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

func init() {
	m.PostInit = func() {
		proc := m.Bridge.Commands.(*commands.Processor)
		for _, h := range connector.BridgeCommands() {
			proc.AddHandler(h)
		}
	}
}

func main() {
	m.InitVersion(Tag, Commit, BuildTime)

	// Raise the open-file-descriptor limit before opening any connections. On
	// macOS, launchd hands daemons a soft RLIMIT_NOFILE of just 256, which a
	// busy bridge (many portals, APNs, IDS, the appservice websocket, SQLite,
	// CardDAV) can exhaust over long uptime — after which new sockets fail with
	// "too many open files", the websocket can't reconnect, and the bridge goes
	// silent until restarted. No-op on non-macOS/Linux platforms. See rlimit_*.go.
	raiseFileLimit()

	// Cap the Go heap (GOMEMLIMIT) to a fraction of system memory so an
	// aggressive backfill can't drive the resident set past physical RAM and
	// get OOM-killed on small, swap-less hosts. Honours an explicit GOMEMLIMIT
	// and is cgroup-aware (Docker, systemd MemoryMax=). See memlimit.go.
	capMemoryLimitFromSystem()

	m.Run()
}
