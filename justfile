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
    #!/usr/bin/env bash
    set -euo pipefail
    revision='33da6848d63b3bba2b4f31ab1531d618f2795192'
    archive_sha='bbda18d6f6666ff05dec33974c134483b096fe321ff68a22eba9399b70354de6'
    root={{ quote(native_root) }}
    source="$root/ghostty"
    test "$(zig version)" = '0.16.0' || { printf 'Zig 0.16.0 is required.\n' >&2; exit 1; }
    command -v pkg-config >/dev/null
    mkdir -p "$root"
    if test -e "$source"; then
        test -f "$source/.frame-source-revision" && test "$(cat "$source/.frame-source-revision")" = "$revision" || { printf 'Existing native source is unmarked or uses another revision: %s\n' "$source" >&2; exit 1; }
    else
        command -v shasum >/dev/null || { printf 'shasum is required.\n' >&2; exit 1; }
        archive="$root/ghostty-$revision.tar.gz"
        url="https://codeload.github.com/ghostty-org/ghostty/tar.gz/$revision"
        # shasum --check exits 1 when the digest differs or the file cannot
        # be read. Any other failure, such as shasum ending on Ctrl+C or
        # Ctrl+\, stops the recipe with that status, so an interrupted check
        # never counts as a failed archive.
        verify_archive() {
            local status=0
            printf '%s  %s\n' "$archive_sha" "$1" | shasum -a 256 --check || status=$?
            case "$status" in
                0 | 1) return "$status" ;;
                *) exit "$status" ;;
            esac
        }
        # curl runs as a job so that remove_download, the EXIT trap below,
        # can stop and reap it before removing its file. A signal sent only
        # to this shell would otherwise leave curl running, and curl would
        # create the file again. curl_pid is cleared once wait reaps curl.
        curl_pid=''
        remove_download() {
            if test -n "$curl_pid"; then
                kill "$curl_pid" 2>/dev/null || true
                wait "$curl_pid" 2>/dev/null || true
            fi
            rm -f "$download"
        }
        # Ctrl+C and Ctrl+\ reach curl too, but a job started without job
        # control ignores SIGINT and SIGQUIT, so this trap stops curl,
        # removes the file, and re-raises the signal ($1). SIGTERM takes the
        # same path: when the whole group gets it, just forwards a second
        # one, which can kill bash before its EXIT trap runs. This happens
        # with macOS's /bin/bash 3.2, and with bash 5.2 during the check.
        # bash, except macOS's /bin/bash 3.2, ignores SIGQUIT again once the
        # trap is reset, so the shell then exits with $2, the status bash
        # gives a command that the signal ends: 128 plus its POSIX number
        # (INT 2, QUIT 3, TERM 15).
        stop_download() {
            remove_download
            trap - EXIT "$1"
            kill -s "$1" "$$"
            exit "$2"
        }
        # Only a verified download is renamed to $archive. A cached archive
        # that fails the check, such as a partial file from an older recipe,
        # is removed and downloaded again.
        if test -f "$archive" && ! verify_archive "$archive"; then
            rm -f "$archive"
            printf 'Removed cached archive %s: it failed the check against SHA-256 %s.\n' "$archive" "$archive_sha" >&2
        fi
        if ! test -f "$archive"; then
            download=$(mktemp "$archive.XXXXXX")
            trap remove_download EXIT
            trap 'stop_download INT 130' INT
            trap 'stop_download QUIT 131' QUIT
            trap 'stop_download TERM 143' TERM
            curl --fail --location --retry 3 "$url" --output "$download" &
            curl_pid=$!
            curl_status=0
            wait "$curl_pid" || curl_status=$?
            curl_pid=''
            test "$curl_status" -eq 0 || exit "$curl_status"
            verify_archive "$download" || {
                printf 'Downloaded %s does not match SHA-256 %s; %s was not saved.\n' "$url" "$archive_sha" "$archive" >&2
                exit 1
            }
            mv "$download" "$archive"
            trap - EXIT INT QUIT TERM
        fi
        extracted=$(mktemp -d "$root/source.XXXXXX")
        trap 'rm -rf "$extracted"' EXIT
        tar -xzf "$archive" --strip-components=1 -C "$extracted"
        printf '%s\n' "$revision" > "$extracted/.frame-source-revision"
        mv "$extracted" "$source"
        trap - EXIT
    fi
    cd "$source"
    # Ghostty's build runs git in its source directory to detect a version.
    # The extracted archive is not a repository. Clear the repository
    # variables a calling git process exports (hooks, rebase --exec), then
    # stop git's repository discovery at $root so it cannot find this checkout.
    git_local_env=''
    if command -v git >/dev/null; then
        git_local_env=$(git rev-parse --local-env-vars)
    fi
    (
        unset -v $git_local_env
        GIT_CEILING_DIRECTORIES="$root" zig build -Demit-lib-vt -Demit-xcframework=false -Doptimize=ReleaseFast -Dtarget="$1" --prefix "$root/$2" --cache-dir "$root/$3" --global-cache-dir "$root/zig-global-cache"
    )
    PKG_CONFIG_PATH="$root/$2/share/pkgconfig" pkg-config --static --libs --cflags libghostty-vt-static

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
