# multirtmp

RTMP relay for vMix: receives a single RTMP publish and re-broadcasts it
byte-exact (no re-encoding) to multiple destinations, each with its own
independently configurable delay — e.g. a corporate feed at 0s, YouTube at
10-15s.

## Quick start

1. `.\multirtmp.exe install` — registers the Windows service and puts a
   **multirtmp** icon on the desktop of every user of this computer
   (one-time, as administrator). If there is no `config.json` next to the
   exe yet, it creates one: web interface reachable from the local network
   with this computer's own random token, preview on, and three
   destinations — `youtube` (15 s delay) and `company` without stream keys
   yet, and `preview`. An existing `config.json` is never touched.
2. `.\multirtmp.exe start` — starts it.
3. Double-click the **multirtmp** icon on the desktop — it opens live status
   and destination management in its own window. Destinations without a
   stream key show as **NOT CONFIGURED**: press **Enter stream key**, fill
   in the server URL and key, Save — the destination starts.

Steps 1 and 3 are one-time setup, done once when deploying to a machine. After
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
- `multirtmp.exe uninstall` — remove the service and the desktop icon.
- `multirtmp.exe open` — open the web interface (this is what the desktop
  icon runs).

## Web interface

The desktop icon opens it in Microsoft Edge as a separate app window (or in
the default browser if Edge isn't installed). If the service isn't running,
the icon shows a message saying so and how to start it, instead of a browser
error page. The address is `http://127.0.0.1:8080/` by default — on this
computer itself no token is ever needed.

To reach it from another device on the same network (e.g. a tablet next to
the scoreboard), set `api.addr` in `config.json` to this machine's LAN
address or `0.0.0.0:8080`; a token then becomes required for other devices
(the app refuses to start without one) and a ready-to-use link, with the
token included, is printed to the log at startup — copy it to the other
device, or use the "Show link for tablet" button in the interface on the
server itself.

The link uses the address on the same network as the computer's default
gateway; extra static addresses on the same adapter and VPN tunnel adapters
are left out. Only if that can't be determined (no gateway) are all
addresses listed, for you to pick the one on the tablet's network. If the
link is still wrong (or you want
the tablet to use the computer's name), set `api.public_host` in
`config.json` to the name or IP the tablet should use, e.g. `"studio-pc"`
or `"192.168.1.12"` (no `http://`, no port). Then only that one link is
shown.

Requests that arrive from this computer itself skip the token. Don't
forward the web interface port from this computer with a local proxy or
port-forwarding tool (`netsh interface portproxy`, ngrok and the like):
requests through it would also look local and skip the token.

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
from two things:

1. [MediaMTX](https://github.com/bluenviron/mediamtx), which receives an RTMP
   stream and serves it to the browser over WebRTC. Put it in a `mediamtx`
   folder next to multirtmp, together with the `mediamtx.yml` from this
   repository (`deploy/mediamtx/mediamtx.yml`, made for MediaMTX v1.21.3):

   ```
   C:\multirtmp\multirtmp.exe
   C:\multirtmp\mediamtx\mediamtx.exe
   C:\multirtmp\mediamtx\mediamtx.yml
   ```

   multirtmp starts MediaMTX by itself when `preview.enabled` is true,
   restarts it if it stops, and stops it together with the service. Its log
   goes to `mediamtx\mediamtx.log`. To use `mediamtx.exe` from another
   place, set `preview.mediamtx_exe`; `mediamtx.yml` is then taken from that
   folder. Don't register MediaMTX as a service of its own and don't start
   it by hand — the port would be taken, and the interface says so.
2. One more entry in multirtmp's own `outputs` — same as any other
   destination, just pointed at MediaMTX instead of YouTube or a corp
   server, e.g. Server URL `rtmp://127.0.0.1:19350/live`, Stream Key
   `preview`, with whatever delay you want the preview to run at.

Set `preview.url` to `http://127.0.0.1:8889`, `preview.path` to the same
stream path (`live/preview` in the example above), and `preview.enabled:
true` — the web UI embeds the player automatically. If MediaMTX isn't
running (not installed, restarting, port taken), the Preview block says so
instead of showing a browser error page; streaming to the destinations is
never affected.

**Preview on a tablet.** The player is served by multirtmp itself, under
`/preview/` on the same address and port as the web interface, so it works
on whatever address the page was opened with — `127.0.0.1` on this
computer, its LAN address or name on a tablet. It is protected by the same
token: a tablet that opened the interface with the link from "Show link for
tablet" can watch, a device without the token can't. MediaMTX's own web
server listens on this computer only. The video itself goes over UDP port
8189: `install` opens it in Windows Firewall for private networks, and
`uninstall` closes it. If the rule is missing, the interface shows the
command to create it. VPN tunnel adapters on this computer don't
interfere: MediaMTX is told to offer only the network adapter that has the
default gateway (plus this computer itself).

The bundled `mediamtx.yml` lets only this computer publish to MediaMTX,
and through multirtmp's `/preview/` nobody can publish at all — only
watch. RTSP, HLS, SRT and MoQ are switched off. When upgrading MediaMTX,
upgrade `mediamtx.yml` to the same version: MediaMTX refuses to start with
settings it doesn't know.

**Limitation:** the WebRTC preview can't show H.264 with B-frames (Main/High
profile with B-frames enabled). For the preview to work, set the vMix
encoder to the Baseline profile or B-frames = 0. This only affects the
browser preview — the RTMP destinations themselves receive the stream
unchanged either way.

Preview is entirely optional. On weaker hardware, or on a site where nobody
needs to watch a preview in the browser, leave `preview.enabled: false` (the
default) — MediaMTX isn't started then, it doesn't need to be installed at
all, and no preview output needs to exist in `outputs`. If preview is
enabled but `mediamtx.exe` is missing, multirtmp still starts and streams
normally; only the preview is unavailable.
