# go-tui-frame justfile

set positional-arguments
set tempdir := '.tmp'

native_root := justfile_directory() / '.tmp/native'
linux_host_arch := if arch() == 'x86_64' { 'amd64' } else if arch() == 'aarch64' { 'arm64' } else { error('Native builds support amd64 and arm64 hosts.') }
linux_host_target := if linux_host_arch == 'amd64' { 'x86_64-linux-musl' } else { 'aarch64-linux-musl' }
native_target := if os() == 'linux' { linux_host_target } else if os() == 'macos' { 'native' } else { error('Native builds support Linux and macOS.') }
native_prefix_name := if os() == 'linux' { 'linux-musl' / linux_host_arch / 'prefix' } else { 'prefix' }
native_cache_name := if os() == 'linux' { if linux_host_arch == 'amd64' { 'linux-musl-cache' } else { 'linux-arm64-musl-cache' } } else { 'zig-cache' }
native_prefix := native_root / native_prefix_name
# Recipes run cgo builds through scripts/with-libghostty-cppflags, which adds
# the binding's pkg-config include flags and its archive's SHA-256 to
# CGO_CPPFLAGS, so Go's build cache keys the binding and every link by the
# native prefix and archive they use.
export CGO_ENABLED := '1'
export PKG_CONFIG_PATH := native_prefix / 'share/pkgconfig' + if env('PKG_CONFIG_PATH', '') == '' { '' } else { ':' + env('PKG_CONFIG_PATH') }
export CC := if os() == 'linux' { 'zig cc -target ' + linux_host_target } else { env('CC', 'cc') }
export TMPDIR := justfile_directory() / '.tmp'

mod example 'cmd/tui-frame/justfile'

# Default recipe: list available recipes
default:
    @just --list

# Build the pinned Ghostty archive for the current macOS or Linux host.
native:
    mkdir -p .tmp
    just _native {{ native_target }} {{ native_prefix_name }} {{ native_cache_name }}

# Build a Linux amd64 or arm64 musl archive, including from macOS.
[arg('architecture', pattern='amd64|arm64')]
native-linux architecture='amd64':
    mkdir -p .tmp
    just _native {{ if architecture == 'amd64' { 'x86_64-linux-musl' } else { 'aarch64-linux-musl' } }} {{ 'linux-musl' / architecture / 'prefix' }} {{ if architecture == 'amd64' { 'linux-musl-cache' } else { 'linux-arm64-musl-cache' } }}

[private]
_native target prefix cache:
    bash scripts/native.sh {{ quote(native_root) }} "$1" "$2" "$3"

# Build all packages and the example wrapper after native setup.
build: native
    scripts/with-libghostty-cppflags go build ./...
    scripts/with-libghostty-cppflags go build {{ if os() == 'linux' { '-ldflags="-linkmode=external -extldflags=-static"' } else { '' } }} -o bin/tui-frame ./cmd/tui-frame

# Produce a fully static Linux amd64 or arm64 example executable.
[arg('architecture', pattern='amd64|arm64')]
build-linux architecture='amd64': (native-linux architecture)
    GOOS=linux GOARCH="$1" CC={{ quote('zig cc -target ' + if architecture == 'amd64' { 'x86_64-linux-musl' } else { 'aarch64-linux-musl' }) }} PKG_CONFIG_PATH={{ quote(native_root / 'linux-musl' / architecture / 'prefix/share/pkgconfig') }} scripts/with-libghostty-cppflags go build -ldflags='-linkmode=external -extldflags=-static' -o "bin/tui-frame-linux-$1" ./cmd/tui-frame

# Audit an existing macOS or Linux executable's dynamic dependencies.
linkage artifact='bin/tui-frame':
    TUI_FRAME_ARTIFACT={{ quote(artifact) }} scripts/with-libghostty-cppflags go test -v ./cmd/tui-frame -run '^TestStandaloneBinary$$' -count=1

# Run the example while preserving each argument.
run *args='': native
    just example run "$@"

# Run the example with plain chrome and agent help.
run-ai *args='': native
    just example run-ai "$@"

# Run all tests with the race detector.
test: native
    scripts/with-libghostty-cppflags go test -race ./...

# Run static code analysis
vet: native
    scripts/with-libghostty-cppflags go vet ./...

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
