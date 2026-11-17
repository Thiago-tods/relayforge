# Contributing to RelayForge

## Ground rules

- All code, comments, documentation, identifiers and UI text are written in English.
- The project runs on a zero-dollar budget: do not add paid services, APIs or dependencies.
- Never commit secrets. Use `deploy/.env` locally (git-ignored).

## Workflow

1. Create a feature branch from `main`.
2. Make focused changes with tests.
3. Run the checks below.
4. Open a pull request describing the change and its motivation.

Commit messages follow the imperative mood, e.g. `Add health endpoint`.

## Checks

Backend (from the repository root):

```powershell
gofmt -l .
go vet ./cmd/... ./internal/...
go test ./cmd/... ./internal/...
```

Frontend (from `web/`):

```powershell
npm run format:check
npm run lint
npm run typecheck
npm test
npm run build
```

CI runs the same checks on every pull request.

## Architectural decisions

Significant decisions are recorded as short ADRs in `docs/decisions/`.
