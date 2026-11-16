// Package migrations embeds the versioned SQL schema so every binary and test
// applies exactly the same files.
package migrations

import "embed"

// FS holds every *.sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS
