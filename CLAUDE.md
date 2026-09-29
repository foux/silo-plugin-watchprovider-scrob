# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this is

A [Silo](https://github.com/Silo-Server/silo-server) plugin that connects Silo profiles to a
self-hosted [Scrob](https://github.com/ellite/scrob) instance through Silo's
`watch_sync_provider.v1` plugin contract. It is written in Go against
[`silo-plugin-sdk`](https://github.com/Silo-Server/silo-plugin-sdk) and modeled on the
first-party [Floppy watch provider](https://github.com/Silo-Server/silo-plugin-watchprovider-floppy).

The plugin talks to Scrob only through Scrob's public HTTP API, authenticated with the
profile's Scrob API key (`X-Api-Key`). It needs no change on the Scrob side.

## Language

Everything in this repository is in **English**: code, identifiers, comments, docstrings,
README and other docs, commit messages, issues and pull requests.

## Contract rules

- The watch-sync contract is defined by
  `proto/silo/plugin/v1/watch_sync_provider.proto` in `silo-plugin-sdk`. Read the comments on
  the messages you touch: they carry the semantics (at-least-once delivery, convergent
  desired-state writes, credential replacement rules, snapshot vs. incremental traversals).
- Credentials and provider config are transient request data. Never persist them in the
  plugin, never log them, and never put them in a `WatchSyncFault.safe_message`.
- `ApplyEvents` is at-least-once: a retried event must not create a second watch in Scrob.
- Only advertise a capability in `manifest.json` when the plugin implements it fully.

## Build and test

```sh
make build   # ./plugin for the host platform
make test    # go test ./...
make lint    # golangci-lint run ./...
```

## Commits

Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:` …), plain-language titles.
