# 3. Go module path

Date: 2026-11-17

## Status

Accepted

## Context

Phase 0 used the module path `github.com/relayforge/relayforge`. The repository is published at
`github.com/Thiago-tods/relayforge`. A module path that differs from its repository URL cannot be
fetched with `go get`, and the GitHub organization `relayforge` is not controlled by this project.

Only five Go files imported the old path, and nothing outside the repository depends on it yet.

## Decision

Change the module path to `github.com/Thiago-tods/relayforge` and update every import. This is a
correctness change, not a cosmetic one: the path now resolves to the real repository.

## Consequences

- The module can be fetched and versioned from the public repository.
- Renaming the repository or moving it to an organization later requires another path change.
  Doing it early, while no external consumers exist, keeps that cost minimal.
