//go:build !agentonly

package assets

import "embed"

//go:embed agents/*
var Files embed.FS
