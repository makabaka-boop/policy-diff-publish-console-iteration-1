package main

import "embed"

// distFS embeds the built Vue app (web/dist). The placeholder file is
// replaced by `npm run build` in web/.
//
//go:embed web/dist
var distFS embed.FS
