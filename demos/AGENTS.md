---
created_on: 2026-10-02 14:14
last_modified: 2026-10-06 14:27
status: current
---

# Demo recordings

This directory owns the wrapper-focused VHS tape, Yazi fixtures, and local
recorder patches. Follow [the recording guide](../docs/internal/references/demo-recording.md)
for pinned builds, validation, and rendering.

- Show the wrapper's layouts, colors, and border resizing while Yazi stays inside.
- Display a genuine pressed-key overlay. Send actual browser key events for the
  Ctrl+B prefix and the 1/2/3 that follows it; do not substitute action labels,
  autoplay, synthetic terminal reports, or ambiguous key aliases in the
  published tape.
- Place the pressed-key overlay in the bottom-right corner.
- Observe actual browser key-down/key-up events: show `↓`/`↑`, highlight held
  keys, and dim released keys. Never infer event phases from chord captions.
- Keep source trees, caches, and recording diagnostics under `.tmp`; build
  recorder binaries into ignored `bin`. Recorder dependencies do not belong in
  the library's Go module or runtime.
- Keep the recorder source revisions, archive checksums, patches, and frontend
  lockfile together. Verify patches against fresh pinned source trees.
- Run the native FFmpeg encoding tests in the VHS patch and the tape's actual
  screen waits. Inspect the rendered GIF, including glyphs and both border states.
- Record new user instructions here when they concern recordings; ask before
  changing conflicting instructions. Never publish media externally, releases,
  tags, or packages without explicit user authorization.
