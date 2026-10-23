package migrations

import "embed"

// Files contains the immutable SQL migrations distributed with every binary.
//
//go:embed *.sql
var Files embed.FS
