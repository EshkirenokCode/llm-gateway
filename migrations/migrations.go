// Package migrations embeds the goose migration files so the binary can bring
// its own schema, with no separate migration image to keep in sync.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
