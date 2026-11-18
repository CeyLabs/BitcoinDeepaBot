// Package translations embeds the bot's message catalogues into the binary, so
// the bot (and its tests) no longer depend on a translations/ directory next to
// the working directory.
package translations

import "embed"

//go:embed *.toml
var FS embed.FS
