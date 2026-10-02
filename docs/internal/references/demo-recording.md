---
created_on: 2026-10-02 14:14
last_modified: 2026-10-02 14:56
status: current
---

# Record the demo with pressed keys

This guide reproduces `assets/demo.gif` for maintainers. Run commands from the
repository root. The tape sends actual browser Ctrl+1/2/3 events to the wrapper;
it does not use `--showcase`, synthetic terminal reports, or action labels.
The browser overlay observes native key-down/key-up events. For Ctrl+1, it shows
`Ctrl ↓`, `1 ↓`, `1 ↑`, and `Ctrl ↑`; held keys are highlighted and released
keys are dimmed. It does not intercept input or derive phases from chord text.

## Requirements

- Build `bin/tui-frame` using the [native build setup](native-build.md).
- Install Yazi 26.9.1, ttyd 1.7.7, Bash, Go, Bun, curl, tar, patch, and shasum.
- Install Chrome or Chromium for the recorder's native browser tests.
- Install FFmpeg for encoding. The tested macOS installation is Homebrew
  `ffmpeg-full` 9.0.2. Browser keycaps do not require libass; the PR's original
  caption mode requires the `ass` filter when `VHS_KEYCAPS` is unset.
- Install Menlo and Maple Mono Normal NF. Menlo draws the terminal and captions;
  Maple supplies Yazi's status glyphs. Different fonts can change the grid.

The recorder uses [VHS caption PR #719](https://github.com/charmbracelet/vhs/pull/719)
at `443981b2c125c35050f9fe8de15b660a9653bc84`. Its bundled terminal predates Kitty
keyboard support. The local ttyd frontend uses `@xterm/xterm` 6.1.0-beta.304 with
the native `vtExtensions.kittyKeyboard` option enabled. The VHS patch captures
the native DOM terminal screen, including its cursor and keycaps, with browser
screenshots. `VHS_KEYCAPS=true` enables the embedded browser overlay. Ctrl chords
use native browser press/release actions, spaced by the tape's `TypingSpeed`
so the recording can show each held/released phase. `CaptionOn`/`CaptionOff`
control the overlay; hiding recording also clears it.
Its normal canvas capture remains available when the environment switch is unset.

These are isolated recording dependencies. Sources and caches stay under `.tmp`,
the VHS executable stays under `bin`, and the wrapper's Go dependencies and
installed VHS/ttyd executables are unchanged.

## Build the recorder

Use a fresh `.tmp/demo-recorder` directory; do not apply patches twice to an
existing source tree. Download pinned archives and verify them before extraction:

```sh
mkdir -p .tmp/demo-recorder/vhs .tmp/demo-recorder/ttyd bin
curl -fsSL https://codeload.github.com/charmbracelet/vhs/tar.gz/443981b2c125c35050f9fe8de15b660a9653bc84 -o .tmp/demo-recorder/vhs.tar.gz
curl -fsSL https://codeload.github.com/tsl0922/ttyd/tar.gz/2922cb89f518bae4d0fcf4d757a7419638fc71fc -o .tmp/demo-recorder/ttyd.tar.gz
printf '%s\n' 'a11d1a487e92c55134821a48be26265c0e2d9f09d82b9e6dc8d68dce4430cbc3  .tmp/demo-recorder/vhs.tar.gz' 'c9d926236ae429e61927eabf5a3c6b03f48be5e8c4c414ee51952b9061bbc835  .tmp/demo-recorder/ttyd.tar.gz' | shasum -a 256 --check
tar -xzf .tmp/demo-recorder/vhs.tar.gz --strip-components=1 -C .tmp/demo-recorder/vhs
tar -xzf .tmp/demo-recorder/ttyd.tar.gz --strip-components=1 -C .tmp/demo-recorder/ttyd
patch -p1 -d .tmp/demo-recorder/vhs < demos/recorder/vhs-screen.patch
patch -p1 -d .tmp/demo-recorder/ttyd < demos/recorder/ttyd-kitty.patch
cp demos/recorder/bun.lock .tmp/demo-recorder/ttyd/html/bun.lock
```

The added encoding tests use an upstream PNG stored in Git LFS. Fetch its actual
contents rather than the pointer in the source archive:

```sh
curl -fsSL https://media.githubusercontent.com/media/charmbracelet/vhs/443981b2c125c35050f9fe8de15b660a9653bc84/examples/demo.png -o .tmp/demo-recorder/vhs/examples/demo.png
printf '%s\n' 'dba734975fb549ff46ed4329bae2e551b2560a708dce6273b654ffc9da4266f5  .tmp/demo-recorder/vhs/examples/demo.png' | shasum -a 256 --check
export TMPDIR="$PWD/.tmp"
(
  cd .tmp/demo-recorder/vhs
  go test -race ./...
  go vet ./...
  go build -trimpath -ldflags '-X main.Version=pr719-443981b-keycaps' -o ../../../bin/vhs-captions-kitty .
)
(
  cd .tmp/demo-recorder/ttyd/html
  bun install --ignore-scripts --frozen-lockfile
  bun x tsc --noEmit
  bun run inline
)
```

The frontend uses ttyd's own inline build. The pinned legacy `xterm` package
satisfies trzsz's declared types; the live terminal uses `@xterm/xterm`.
The frontend patch maps ttyd's existing Yarn dependency patch to Bun's native
`patchedDependencies`, preserving the upstream zmodem correction during install.
Upstream license notices accompany the patches in [demos/recorder](../../../demos/recorder).

## Render and verify

```sh
./bin/vhs-captions-kitty validate demos/yazi.tape
TMPDIR="$PWD/.tmp" VHS_PUBLISH=false VHS_SCREEN_CAPTURE=true VHS_KEYCAPS=true VHS_TTYD_INDEX="$PWD/.tmp/demo-recorder/ttyd/html/dist/inline.html" ./bin/vhs-captions-kitty demos/yazi.tape
ffprobe -v error -show_entries stream=width,height,nb_frames:format=duration,size -of json assets/demo.gif
```

The recording shows Signal bar, Layered badge, and Bordered card layouts;
red/navy/teal backgrounds; and border removal/restoration. Screen waits require
the child viewport to change between 98×21 and 100×23. Startup and teardown are
hidden, and Yazi stays on the sample preview throughout. Four keycaps sit in the
bottom-right corner and clear after 1.5 seconds of inactivity with no held keys.
Browser margins are relative to the captured terminal screen; the GIF also has
18 pixels of outer padding. Numeric keys are quoted in the tape because
this PR's parser accepts `Ctrl+"1"` rather than an unquoted numeric token.

Inspect representative GIF frames after rendering, including a visible key,
both border states, and the final Signal bar. Font or browser changes can affect
geometry, glyph coverage, and capture timing even when tape parsing succeeds.
The tested environment is macOS arm64; no Linux recording claim is made.
