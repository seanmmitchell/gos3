# gos3 — notes for agents

- Read `docs/ARCHITECTURE.md` first; `docs/admin-api.md` and `docs/discovery.md` are the API/protocol specs.
- Several agents work in this checkout concurrently. Only edit files your work item owns (see the ownership tables in `docs/ARCHITECTURE.md`). Contract files (package comment says CONTRACT) keep their exported names/signatures; you may replace stub bodies only if you own them. `internal/s3err` is append-only.
- Never run git commands that change state (`commit`, `add`, `stash`, `checkout`, `reset`, `clean`). The orchestrator commits.
- Go: build/test only your packages (`go test -race ./internal/<pkg>/...`); another agent's package may be mid-edit. Run `gofmt -w` and `go vet` on your packages. Don't add Go dependencies beyond those already in `go.mod`.
- Web UI lives in `web/` (pnpm, Nuxt 4, TypeScript strict). Don't run `pnpm add` outside your work item's scope without need; never commit `node_modules` or `.output`.
- Keep code idiomatic and documented at the level of the surrounding code; prefer stdlib.
