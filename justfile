# go-tui-frame justfile

# Default recipe: list available recipes
default:
    @just --list

# Build all library packages.
build:
    go build ./...

# Run all tests with the race detector.
test:
    go test -race ./...

# Run static code analysis
vet:
    go vet ./...

# Lint (alias for vet)
lint: vet

# Format Go code
fmt:
    go fmt ./...

# Run module hygiene check
tidy:
    go mod tidy -diff

# Verify module hygiene, build, static analysis, and tests in sequence.
check:
    just tidy
    just build
    just vet
    just test
