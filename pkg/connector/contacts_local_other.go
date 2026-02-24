//go:build !darwin

package connector

import "github.com/rs/zerolog"

func newLocalContactSource(_ zerolog.Logger) contactSource { return nil }
