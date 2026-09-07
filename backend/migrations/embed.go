// Package migrations carries the schema as data, so a deployed binary holds the exact
// migrations it was built with.
//
// Embedding rather than mounting a directory is the point: a container that migrates from
// a volume can be pointed at the wrong revision of the schema, and the failure looks like
// a missing column at runtime rather than like a deployment mistake. Here the binary and
// the schema ship as one artifact and cannot disagree.
package migrations

import "embed"

// FS holds every goose migration in this directory. goose reads it through
// goose.SetBaseFS, so the same files drive `make migrate` from a checkout and
// `plimsollctl migrate` from the image.
//
//go:embed *.sql
var FS embed.FS
