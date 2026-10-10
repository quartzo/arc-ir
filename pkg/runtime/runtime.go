// Package runtime embeds the C support runtime that generated ARC assembly
// links against. Keeping it embedded lets the compiler emit a runnable
// program without any external files on disk.
package runtime

import _ "embed"

// Source is the full text of arc_runtime.c.
//
//go:embed src/arc_runtime.c
var Source string

// SourceName is the file name used when writing the runtime next to generated
// assembly.
const SourceName = "arc_runtime.c"
