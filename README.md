# Minecraft Dungeons II - Nextendo

Initial authentication scaffold for title 83156, not a complete multiplayer server. Party, character, matchmaking, QoS and websocket APIs remain unimplemented. Unknown requests return 501. Client compatibility is not yet verified.

Routes: POST /Client/LoginWithNintendoServiceAccount, POST /Authentication/GetEntityToken, GET /healthz.

NEXTENDO_SECRET_FILE points to the existing hex-encoded Nextendo identity key. Signed nx2 credentials carried in the Nintendo identity token are required; unsigned PID claims are rejected. LISTEN_ADDR defaults to loopback port 8186. Use an HTTPS reverse proxy. Do not redirect clients until routing and remaining protocol stages are verified. Account moderation integration is pending.

Run `go test ./...` and `go build ./...`. Do not commit game binaries, user credentials, keys or saves.

## Disclaimer

This server ships **no** Nintendo code, keys, or copyrighted assets. It is an independent
reimplementation for use with a community-run replacement service, not affiliated with, endorsed by,
or associated with Nintendo or the game's publisher.

## License

Released under the **[PolyForm Shield License 1.0.0](LICENSE.md)**, source-available: read, use,
modify, and self-host, but do not use it to provide a product that competes with Nextendo Network.
