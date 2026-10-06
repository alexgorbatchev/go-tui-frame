# Run from the repository root after the recorder setup:
#   docs/internal/references/demo-recording.md
# CaptionOn records actual browser key-down/key-up events.
Output "assets/demo.gif"
Require "./bin/tui-frame"
Require yazi
Require bash
Set Shell "bash"
Set Width 1246
Set Height 654
Set FontFamily "Menlo,Maple Mono Normal NF,DejaVu Sans Mono,monospace"
Set FontSize 18
Set Padding 18
Set Framerate 12
Set CursorBlink false
Set Theme "Catppuccin Mocha"
Set TypingSpeed 100ms
Set WaitTimeout 10s
Set CaptionFont "Menlo"
Set CaptionFontSize 22
Set CaptionMaxKeys 4
Set CaptionAlignment "bottom-right"
Set CaptionMarginRight 28
Set CaptionMarginVertical 0
Set CaptionInactivityTimer 1.5s
Env AGENT "0"
Env COLORTERM "truecolor"
Env VHS_PUBLISH "false"

Hide
# Yazi accepts absolute configuration paths. Keep caches/state in the repo too.
Type@0ms 'unset NO_COLOR; export YAZI_CONFIG_HOME="$PWD/demos/yazi" XDG_CACHE_HOME="$PWD/.tmp/vhs-yazi/cache" XDG_STATE_HOME="$PWD/.tmp/vhs-yazi/state"; ./bin/tui-frame -- yazi demos/fixtures/workspace/welcome.md'
Enter
Wait+Screen /Yazi: workspace/
Wait+Screen /The border toggles and Yazi resizes\./
Sleep 500ms
Show
CaptionOn
Sleep 1.5s
Ctrl+B
Type "1"
Wait+Screen /Layered badge/
Sleep 1.8s
Ctrl+B
Type "2"
Sleep 1.8s
Ctrl+B
Type "2"
Sleep 1.8s
Ctrl+B
Type "3"
Wait+Screen /100×23 \| border off/
Sleep 1.8s
Ctrl+B
Type "1"
Wait+Screen /Bordered card/
Sleep 1.8s
Ctrl+B
Type "3"
Wait+Screen /98×21 \| border on/
Sleep 1.8s
Ctrl+B
Type "2"
Sleep 1.8s
Ctrl+B
Type "1"
Wait+Screen /Signal bar/
Wait+Screen /The border toggles and Yazi resizes\./
Sleep 2s
Hide
CaptionOff
Ctrl+Q
Wait+Line /^>\s*$/
