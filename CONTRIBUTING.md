# Contributing to ConOps

This is a private fork (`valthaide/conops-private`). Changes are made via commits/PRs in that repository.

## Development Setup

```bash
git clone git@github.com:valthaide/conops-private.git
cd conops-private

go run ./cmd/conops
go test ./...
go build -o bin/conops ./cmd/conops
go build -o bin/cps ./cmd/cps
```

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Write clear, self-documenting code with comments where needed
- Keep functions focused and testable

## Questions

Open an issue in [valthaide/conops-private](https://github.com/valthaide/conops-private/issues).
