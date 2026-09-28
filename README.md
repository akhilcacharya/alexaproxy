# alexaproxy

Control your Amazon Echo devices with `curl`, from anywhere on your
[Tailscale](https://tailscale.com) network, or from an AI assistant like
Meta Muse.

```console
$ curl homeserver:8787/ask -d device=Kitchen --data-urlencode "text=what's the weather"
{
  "device": "Kitchen",
  "ok": true,
  "question": "what's the weather",
  "response": "Right now it's 56 degrees with cloudy skies. Expect rain today, with a high of 60."
}
```

- **One small Go binary.** It runs as a systemd service on any Linux box and
  also works on macOS.
- **Browser sign-in from any device.** Start a login with one `curl`, then sign
  in to Amazon from your phone or laptop. The token is saved and reused across
  restarts.
- **Self-documenting.** `GET /` is a curl cheat sheet, `GET /agent` is a guide
  written for AI assistants, and `GET /openapi.json` is a full OpenAPI 3.1 spec.
  All three are generated from the same route table, so they can't drift.
- **Tells you when the login breaks.** The login is re-checked in the
  background, and every response carries an `X-Alexa-Auth` header. Calls fail
  with a clear `login_required` error that explains how to fix it.
- **Private by default.** It answers only your tailnet, which is also how
  Muse reaches it, through Muse's Tailscale integration. For assistants that
  can't join a tailnet, you can publish it with Tailscale Funnel plus an API key.

> **Unofficial.** This uses the same private API as the Alexa app, through
> [alexa-cli](https://github.com/buddyh/alexa-cli). It isn't affiliated with
> Amazon, and it may break if Amazon changes that API.

## Contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Signing in to Amazon](#signing-in-to-amazon)
- [Using the API](#using-the-api)
- [Recipes](#recipes)
- [Using it from AI assistants](#using-it-from-ai-assistants), including the [Meta Muse setup](#meta-muse)
- [Tailscale configuration](#tailscale-configuration)
- [Access and security model](#access-and-security-model)
- [Running as a service](#running-as-a-service)
- [Configuration reference](#configuration-reference)
- [Troubleshooting](#troubleshooting)

## How it works

```
 laptop / phone / script ──┐
 local AI assistants ──────┤  tailnet (100.x)         ┌──────────────────────────┐
 Muse (Tailscale) ─────────┼────────────────────────► │ alexaproxy  :8787        │ ──► Amazon Alexa API ──► your Echos
                           │                          │  credentials.json (0600) │
 other cloud assistants ───┘  Tailscale Funnel        │  login helper :8788      │
       (optional)             https://….ts.net + key  │  (only while signing in) │
                                                      └──────────────────────────┘
```

alexaproxy swaps an Amazon refresh token for session cookies and calls the
Alexa endpoints the mobile app uses. To get that token, it briefly runs
[alexa-cookie-cli](https://github.com/adn77/alexa-cookie-cli). That helper
serves Amazon's real sign-in page through a proxy bound to your Tailscale IP
and captures the token when you finish.

## Quick start

**You need:** a Linux or macOS machine that's always on and runs
[Tailscale](https://tailscale.com/download), plus Go 1.25+ to build. The
browser sign-in helper is downloaded automatically on x86-64 Linux and on
macOS. For ARM Linux (e.g. a Raspberry Pi), see
[Logging in from another machine](#logging-in-from-another-machine).

**1. Install**

```bash
go install github.com/akhilcacharya/alexaproxy/cmd/alexaproxy@latest
# or: git clone https://github.com/akhilcacharya/alexaproxy && cd alexaproxy && make build
```

**2. Run it**

```bash
alexaproxy serve
# listening on :8787
# tailnet URL: http://100.101.102.103:8787/
```

**3. Sign in** from any device on your tailnet (replace `homeserver` with
the machine's Tailscale name):

```bash
curl -X POST homeserver:8787/auth/login
# { "state": "preparing", "login_url": "http://100.101.102.103:8788/", ... }
```

Open `login_url` in a browser and sign in to Amazon. Type your email by
hand, and choose to sign in with a password if Amazon offers a passkey (see
[why](#signing-in-to-amazon)). Then confirm:

```bash
curl "homeserver:8787/auth/login?wait=300"    # returns once you're signed in
curl homeserver:8787/devices
```

**4. Try it**

```bash
curl homeserver:8787/speak -d device=Kitchen --data-urlencode 'text=Hello from the tailnet'
curl homeserver:8787/settings/default-device -d device=Kitchen    # optional: no more device=...
curl homeserver:8787/ask --data-urlencode 'text=what time is it'
```

**5. Keep it running** with [systemd](#running-as-a-service):
`make install-user`.

## Signing in to Amazon

`POST /auth/login` starts a sign-in session for up to 10 minutes. The login
page listens on port 8788 of the server's Tailscale IP, so any device on your
tailnet can open it. The page is never exposed through Funnel. Once you've
signed in, the token is checked with Amazon and saved to `credentials.json`.

Tips:

- **Type the email by hand.** Amazon's page has a hidden second email field,
  and autofill or a password manager can fill the wrong one. That shows up as
  *"Enter a valid email or mobile number"*. A private window avoids it.
- **Use your password, not a passkey.** Passkeys only work on amazon.com
  itself over HTTPS, not through the sign-in proxy. Choose "sign in another
  way" or "use password" if prompted. One-time codes (2FA) work fine.
- **Open the exact `login_url`, IP address included.** Cookies are tied to
  that address, so switching to a hostname partway through breaks the sign-in.
- **Outside the US**, pass your marketplace:
  `curl -X POST homeserver:8787/auth/login -d country=amazon.co.uk`
  (`amazon.de`, `amazon.it`, …; for Japan use `-d domain=amazon.co.jp`).
- Amazon may list a new device named "alexaproxy" in your account. That's
  this sign-in.

Other ways to sign in:

| How | Command |
| --- | --- |
| Shell on the server | `alexaproxy login` (prints the URL, waits, saves) |
| Token you already have | `curl homeserver:8787/auth/token --data-urlencode 'refresh_token=Atnr\|...'` |
| Reuse [alexa-cli](https://github.com/buddyh/alexa-cli) | `alexaproxy login --import-alexa-cli` |

Check, re-verify, or clear the login: `alexaproxy status --verify`,
`GET /status?verify=true`, `alexaproxy logout` / `POST /auth/logout`.

### Logging in from another machine

The sign-in helper ships only for x86-64. On ARM Linux, run the sign-in on
any x86-64 Linux machine or any Mac, then hand the token to the server:

```bash
alexaproxy login --login-host 127.0.0.1 --state-dir /tmp/axp    # opens on this machine only
curl homeserver:8787/auth/token --data-urlencode \
  "refresh_token=$(jq -r .refresh_token /tmp/axp/credentials.json)"
rm -r /tmp/axp
```

## Using the API

Pass parameters however is easiest: as a query string, a form body
(`curl -d`, or `--data-urlencode` for free text), or a JSON body. Responses
are JSON with `"ok": true|false`.

| Endpoint | What it does |
| --- | --- |
| `GET /devices` | List Echo devices. Refer to one by name, any unique part of the name, or serial. |
| `POST /speak` `text` `device` | Say exact words on one device. |
| `POST /announce` `text` | Announce on every device. |
| `POST /command` `text` `device` | Voice command, as if spoken: lights, music, timers, routines… |
| `POST /ask` `text` `device` | Voice command that returns Alexa's spoken reply as text. |
| `GET /history` `limit` | Recent voice activity. |
| `GET /settings`, `POST`/`DELETE /settings/default-device` | Default device used when `device` is omitted. Persists across restarts. |
| `GET /status` | Login state, default device, device list. |
| `POST /auth/login`, `GET /auth/login?wait=N` | Browser sign-in (see above). |
| `GET /`, `/agent`, `/openapi.json` | Docs for humans, AI agents, and tools. |

The live reference is always `curl homeserver:8787/`.

### Knowing when the login breaks

- `GET /status` returns `auth.state`, which is one of:
  - `ok`
  - `unchecked`: saved but not verified yet
  - `not_logged_in`
  - `invalid`: Amazon rejected the stored login
- The login is re-checked every 15 minutes (`--check-interval`).
- Every response has an `X-Alexa-Auth` header with the same value.
- An Alexa call made without a working login returns HTTP 401:

  ```json
  { "ok": false, "login_required": true, "auth": { "state": "invalid", ... },
    "how_to_fix": "The Alexa login is missing or expired and a human must sign in. POST /auth/login, ..." }
  ```

## Recipes

Set a shell helper once:

```bash
alexa() { curl -s "homeserver:8787/$1" "${@:2}"; }
```

**Tell me when a long job finishes**

```bash
make release && alexa speak --data-urlencode 'text=The release build finished' \
             || alexa speak --data-urlencode 'text=The release build failed'
```

**Dinner's ready, everywhere**

```bash
alexa announce --data-urlencode "text=Dinner's ready!"
```

**Music**

```bash
alexa command -d device="Living Room" --data-urlencode 'text=play lo-fi beats on Spotify'
alexa command -d device="Living Room" --data-urlencode 'text=volume 4'
alexa command -d device="Living Room" --data-urlencode 'text=stop'
```

**Smart home.** Anything you'd say out loud works:

```bash
alexa command --data-urlencode 'text=turn off all the lights'
alexa command --data-urlencode 'text=set the thermostat to 68'
alexa ask     --data-urlencode 'text=is the front door locked'
```

**Timers, reminders, routines**

```bash
alexa command --data-urlencode 'text=set a timer for 12 minutes'
alexa command --data-urlencode 'text=remind me to take the bins out at 8pm'
alexa command --data-urlencode 'text=good night'        # runs your "good night" routine
```

**Morning briefing** from cron (7:00 on weekdays):

```cron
0 7 * * 1-5  curl -s homeserver:8787/command -d device=Bedroom --data-urlencode 'text=what is my day like'
```

**Get answers back into a script**

```bash
alexa ask --data-urlencode "text=what's on my calendar today" | jq -r .response
```

**With an AI assistant.** Once it's [set up](#using-it-from-ai-assistants),
just ask:

- "Announce on all the Echos that we're leaving in 10 minutes."
- "Play some jazz on the bedroom Echo, volume 3."
- "Is the front door locked? If not, lock it."
- "From now on use the kitchen Echo by default." (the assistant calls `POST /settings/default-device`)
- "Check my calendar and announce my first meeting on the Echo." (combines another connector with this one)
- "What did I last ask Alexa?"

## Using it from AI assistants

Any assistant that can make HTTP requests can use alexaproxy. Point it at
`/agent`: a Markdown guide that includes the live login status, your device
names, which endpoint to use when, and how to handle each error. Tools that
import APIs can use `/openapi.json` instead.

### Assistants on your tailnet

This covers Claude Code, local agents, and scripts running on a tailnet
machine. They need no extra setup. Tell the assistant:

> You can control my Amazon Alexa devices through an HTTP API at
> `http://homeserver:8787`. Before doing anything, fetch
> `http://homeserver:8787/agent` and follow it. Use `/ask` when I want
> information and `/command` when I want something done. If a call returns
> `login_required`, stop and tell me.

### Meta Muse

[Muse](https://ai.meta.com/muse/) can join your tailnet through its
Tailscale integration. Once connected, it reaches alexaproxy like any other
tailnet device, so no API key or Funnel is needed.

**1. Connect Muse to your tailnet.** Tailscale is one of Muse's built-in
connectors:

1. In Muse, open **Settings → Connectors** and choose **Tailscale**.
2. Sign in with the Tailscale account that owns the tailnet alexaproxy runs
   on, and approve the access Muse asks for.
3. In the [Tailscale admin console](https://login.tailscale.com/admin/machines),
   check that a machine for Muse has appeared. If your tailnet has
   [device approval](https://tailscale.com/kb/1099/device-approval) turned on,
   approve it there.
4. If your [access policy](#tailscale-configuration) restricts who can reach
   the server, allow Muse's machine `tcp:8787`. It doesn't need `8788`: you
   do the Amazon sign-in in your own browser.

**Check that Muse can reach the server** before going further. Ask Muse:

> Fetch http://homeserver:8787/status and tell me the values of `you_came_from` and `auth.state`.

`you_came_from: "tailnet"` means Muse is connected over Tailscale.
`auth.state: "ok"` means the Amazon login works. If Muse can't connect:

- Try the full MagicDNS name (`homeserver.your-tailnet.ts.net`) or the
  server's Tailscale IP.
- Check that Muse's machine is approved and hasn't expired in the admin
  console.
- Check that your access policy allows it to reach `tcp:8787`.

**2. Tell Muse about it**, for example:

> Add a connector called "Alexa" for my alexaproxy server on my tailnet.
> The usage guide is at http://homeserver:8787/agent and the OpenAPI spec is
> at http://homeserver:8787/openapi.json. No authentication is needed.
> Use `/ask` when I want information, `/command` when I want something done,
> and `/speak` to say exact words. If a call returns `login_required`, stop
> and tell me.

**3. Test it:** "List my Alexa devices", then "Say hello on the kitchen
Echo". To choose a default Echo, say "use the bedroom Echo by default from
now on". Muse sets it with `POST /settings/default-device`.

**4. Set approvals.** In Muse's settings, consider requiring approval for
`announce` (it plays in every room) and for `/auth/*` actions.

Anything on your tailnet is trusted with full access to the API. To give
Muse's machine only this server and port, use a grant like the one in
[Tailscale configuration](#tailscale-configuration).

### Cloud assistants that can't join a tailnet

Publish the API with [Tailscale Funnel](https://tailscale.com/kb/1223/funnel)
and protect it with an API key.

**1. Create an API key** on the server. A running server picks it up
immediately.

```bash
alexaproxy apikey          # prints axp_…; keep it secret
```

**2. Publish port 8787 with Funnel.** Only this port is published; the login
page on 8788 stays private.

```bash
tailscale funnel --bg 8787
tailscale funnel status    # shows https://homeserver.your-tailnet.ts.net
```

The first time, the CLI prints a link to enable Funnel (and HTTPS
certificates) for your tailnet.

**3. Check it from outside the tailnet**, e.g. on a phone with Tailscale off:

```bash
URL=https://homeserver.your-tailnet.ts.net
curl $URL/openapi.json                                   # public: 200
curl $URL/status                                         # 401, api_key_required
curl $URL/status -H "Authorization: Bearer axp_…"        # 200
```

**4. Give the assistant** the spec URL (`$URL/openapi.json`) and guide
(`$URL/agent`), with the key as a bearer token (`Authorization: Bearer …`,
or `X-API-Key: …`). Enter the key in the assistant's credential or secret
settings, never in the chat.

To revoke access, run `alexaproxy apikey --rotate` or unpublish with
`tailscale funnel reset`. Tailnet access keeps working either way.

## Tailscale configuration

**Names.** With [MagicDNS](https://tailscale.com/kb/1081/magicdns) on, use
`http://homeserver:8787` from any device. Without it, use the Tailscale IP
from `tailscale ip -4`.

**Who can reach it.** The default policy lets every device on your tailnet
reach every other. To limit ports 8787 and 8788 to yourself, and port 8787
to Muse's machine, adapt your [access policy](https://tailscale.com/kb/1018/acls):

```jsonc
{
  "hosts": { "alexa": "100.101.102.103" },   // the server's Tailscale IP
  "grants": [
    { "src": ["you@example.com"], "dst": ["alexa"], "ip": ["tcp:8787", "tcp:8788"] },
    // Muse: use how its machine appears in your admin console (a tag, user, or IP)
    { "src": ["tag:muse"], "dst": ["alexa"], "ip": ["tcp:8787"] }
    // ...your other grants
  ]
}
```

**HTTPS inside the tailnet** (optional): `tailscale serve --bg 8787` serves
`https://homeserver.your-tailnet.ts.net` to tailnet devices only.
alexaproxy sees the Tailscale identity headers and treats these requests as
tailnet requests.

**Funnel, for cloud assistants that can't join your tailnet** (optional). Funnel needs MagicDNS, HTTPS
certificates, and the `funnel` node attribute in your policy. The first
`tailscale funnel` run links you to the admin page that turns these on.
Then:

- `tailscale funnel --bg 8787` publishes the API.
- `tailscale funnel status` shows what's published.
- `tailscale funnel reset` unpublishes.

Funnel requests always need the API key. If you publish without creating
one, public requests are refused.

## Access and security model

| Request comes from | Without an API key configured | With an API key configured |
| --- | --- | --- |
| Tailnet device (direct, or `tailscale serve`) | allowed | allowed (no key needed) |
| Public internet via Funnel | **refused** (403) | key required |
| This machine / another local reverse proxy | allowed | key required |
| Anything else (LAN, …) | refused (403) | refused, unless `--allow-all` (then key required) |

- **Public docs.** `/`, `/agent`, `/llms.txt`, `/openapi.json`,
  `/docs.json`, and `/healthz` are readable without a key, so tools can fetch
  the spec. Device names, serials, and the default device are left out unless
  the caller is trusted.
- **Sending the key.** Use `Authorization: Bearer <key>` or `X-API-Key: <key>`.
  Keys are 256-bit random values, compared in constant time.
  `alexaproxy apikey --rotate` takes effect without a restart.
- **Treat `credentials.json` like a password.** It controls your Amazon
  account's Alexa. It's written with mode 0600.
- **The sign-in helper** only runs during a sign-in. It's bound to the
  Tailscale IP and exits on success, failure, cancel, or timeout.

## Running as a service

**As your user** (no root needed):

```bash
make install-user                # ~/.local/bin/alexaproxy + ~/.config/systemd/user/alexaproxy.service
loginctl enable-linger $USER     # once: start at boot without a login session
journalctl --user -u alexaproxy -f
```

**System-wide**, as a sandboxed `DynamicUser` with state in `/var/lib/alexaproxy`:

```bash
make install
journalctl -u alexaproxy -f
```

The system unit reads optional settings from `/etc/alexaproxy.env`. To set
an API key there (for Funnel), use `ALEXAPROXY_API_KEY`:

```bash
echo "ALEXAPROXY_API_KEY=axp_$(openssl rand -hex 32)" | sudo install -m 600 /dev/stdin /etc/alexaproxy.env
sudo systemctl restart alexaproxy
sudo cat /etc/alexaproxy.env    # the key to give your assistant
```

After installing, sign in over HTTP as in the [quick start](#quick-start).
Upgrade by pulling and re-running the same `make` target.

## Configuration reference

`alexaproxy serve` flags:

| Flag | Default | |
| --- | --- | --- |
| `--addr` | `:8787` | Listen address. Non-tailnet clients are refused regardless (see above). |
| `--state-dir` | see below | Where credentials, settings, and the API key live. |
| `--default-device` | `$ALEXAPROXY_DEFAULT_DEVICE` | Fallback default device. A device set via `/settings` wins. |
| `--check-interval` | `15m` | How often to re-verify the Amazon login. |
| `--login-host` | `auto` (Tailscale IP) | Address browsers use to reach the sign-in page. |
| `--login-port` | `8788` | Sign-in page port. |
| `--login-timeout` | `10m` | How long a sign-in may take. |
| `--cookie-cli` | auto-download | Path to your own `alexa-cookie-cli` binary. |
| `--allow-all` | off | Accept non-tailnet clients (the API key is then required for them). |

Other commands: `alexaproxy login | status | logout | apikey | version`.
Each takes `-h`.

**State directory.** The first of these that's set: `--state-dir`,
`$ALEXAPROXY_STATE_DIR`, `$STATE_DIRECTORY` (set by systemd), or
`~/.config/alexaproxy`. Contents:

| File | |
| --- | --- |
| `credentials.json` | Amazon refresh token and marketplace (0600) |
| `settings.json` | Default device (0600) |
| `api_key` | API key, if created (0600). `$ALEXAPROXY_API_KEY` overrides it. |
| `bin/` | Downloaded sign-in helper |

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| "Enter a valid email or mobile number" on the Amazon page | Autofill filled the hidden field. Type the email by hand, ideally in a private window. |
| Passkey prompt fails, or sign-in loops | Choose "sign in another way" / password. Passkeys can't work through the proxy. |
| `login_url` doesn't load | Open it from a device on the tailnet, using the exact IP:8788 URL. Check that your ACLs allow port 8788. |
| `401 login_required` | The Amazon login is missing or expired. Sign in again (`POST /auth/login`). |
| `401 api_key_required` | Send `Authorization: Bearer <key>`. Get the key with `alexaproxy apikey`. |
| `403 only reachable from the tailnet` | You're calling from outside the tailnet. Use a tailnet device, or Funnel + an API key. |
| `409 matches several devices` | Use a longer part of the name, or the serial from `/devices`. |
| `/ask` answered, but out loud too | Expected. It's a real voice command; the reply is read back from history. |
| Browser sign-in unavailable on ARM | See [Logging in from another machine](#logging-in-from-another-machine). |

Logs: `journalctl --user -u alexaproxy -f` (or without `--user` for the
system unit).

## Development

```bash
make build   # bin/alexaproxy
make test    # go test ./...
make run     # build + serve
```

The code is laid out as follows:

- `internal/server`: HTTP layer. `routes.go` holds the route table that also
  generates the docs.
- `internal/login`: browser sign-in.
- `internal/store`: state files.
- `internal/api`: vendored Alexa client.

## Credits

`internal/api` is vendored from
[buddyh/alexa-cli](https://github.com/buddyh/alexa-cli) v0.6.0 (MIT, see
`internal/api/LICENSE`). Upstream keeps it in an `internal/` package, which
another module can't import. Browser sign-in uses
[adn77/alexa-cookie-cli](https://github.com/adn77/alexa-cookie-cli), built
on [alexa-cookie2](https://github.com/Apollon77/alexa-cookie2). Thanks to
those projects and to
[alexa_media_player](https://github.com/alandtse/alexa_media_player) for
mapping the API.

## License

[MIT](LICENSE). The vendored `internal/api` package keeps its own MIT
license from [alexa-cli](https://github.com/buddyh/alexa-cli).
