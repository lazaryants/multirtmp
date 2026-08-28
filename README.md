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
