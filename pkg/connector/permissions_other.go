//go:build !darwin

package connector

import "github.com/rs/zerolog"

func canReadChatDB(_ zerolog.Logger) bool  { return false }
func showDialogAndOpenFDA(_ zerolog.Logger) {}
func waitForFDA(_ zerolog.Logger)           {}
