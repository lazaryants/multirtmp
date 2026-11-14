# multirtmp

RTMP relay for vMix: receives a single RTMP publish and re-broadcasts it
byte-exact (no re-encoding) to multiple destinations, each with its own
independently configurable delay — e.g. a corporate feed at 0s, YouTube at
10-15s.

## Quick start

1. `.\multirtmp.exe install` — registers the Windows service (one-time).
2. Edit `config.json` (created next to the exe the first time it runs) —
   set your destinations and delays.
3. `.\multirtmp.exe start` — starts it.
4. Open `http://127.0.0.1:8080/` — live status and destination management.

Steps 1-2 are one-time setup, done once when deploying to a machine. After
`install`, the service is registered to start automatically on Windows boot —
day to day, turning the computer on is enough. You only need `start` again if
the service was stopped by hand.

If you run the exe directly (not as a service) and `config.json` doesn't
exist yet, it creates a commented sample and exits — edit it, then either run
the exe again directly or use `install` to run it as a background service
instead.

## Managing the service

- `multirtmp.exe status` — is it running?
- `multirtmp.exe stop` — stop it.
- `multirtmp.exe uninstall` — remove the service entirely.

## Web interface

`http://127.0.0.1:8080/` by default — local-only, no login required.

To reach it from another device on the same network (e.g. a tablet next to
the scoreboard), set `api.addr` in `config.json` to this machine's LAN
address or `0.0.0.0:8080`; a token then becomes required (the app refuses to
start without one) and a ready-to-use link, with the token included, is
printed to the log at startup — copy it to the other device, or use the
"Show link for tablet" button in the interface once you're logged in from the
server itself.

## Configuration

`config.json`, next to the executable. A commented sample is created
automatically the first time you run the exe without one — see the comments
in the generated file for what each field does and its default.

Each destination's Server URL accepts either `rtmp://` or `rtmps://` (RTMP
over TLS, typically port 443) — use `rtmps://` for a destination that
requires or prefers an encrypted connection.

## Optional live preview

The web interface can embed a live preview player below the status section.
multirtmp itself never decodes or re-packages video for this — it's built
from two things that already exist:

1. A second, separate program — [MediaMTX](https://github.com/bluenviron/mediamtx)
   — installed alongside multirtmp on the same machine and registered as its
   own Windows service (`mediamtx.exe` has its own `--help`/install options;
   it's independent of multirtmp's own service). It listens for an RTMP
   publish and serves it back out over WebRTC, among other protocols.
2. One more entry in multirtmp's own `outputs` — same as any other
   destination, just pointed at MediaMTX instead of YouTube or a corp
   server, e.g. `rtmp://127.0.0.1:19350/preview` with whatever delay you
   want the preview to run at.

Point `preview.url`/`preview.path` in `config.json` at that MediaMTX's WebRTC
address and the same stream path, set `preview.enabled: true`, and the web UI
embeds it automatically. There's no special coupling beyond that — multirtmp
just forwards RTMP tags to MediaMTX exactly like it forwards them to any other
destination; MediaMTX does the actual decoding for the browser player.

**Limitation:** the WebRTC preview can't show H.264 with B-frames (Main/High
profile with B-frames enabled). For the preview to work, set the vMix
encoder to the Baseline profile or B-frames = 0. This only affects the
browser preview — the RTMP destinations themselves receive the stream
unchanged either way.

Preview is entirely optional. On weaker hardware, or on a site where nobody
needs to watch a preview in the browser, leave `preview.enabled: false` (the
default) — MediaMTX doesn't need to be installed or running at all in that
case, and no preview output needs to exist in `outputs`.
