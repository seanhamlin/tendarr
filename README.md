<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
    <img src="assets/logo-light.svg" alt="Tendarr" width="312" height="64">
  </picture>
</p>

Tend your Plex library: prune what nobody watches and shape what stays
into better files. Tendarr finds files worth upgrading and content nobody
 watches and hands the actual work to Radarr and Sonarr.

| Feature | What it does | Talks to |
|---|---|---|
| **Movie upgrade** | Large movies below their quality profile's optimal custom format score. Check indexers for a better release, then have Radarr search and grab. | Radarr |
| **Movie reap** | Movies unwatched for a long time, largest first. Delete through Radarr, or tag keepers so they never show up again. | Plex, Tautulli, Radarr |
| **TV episode upgrade** | The same as movie upgrade, per episode file. | Sonarr |
| **TV reap** | Whole series nobody watches, largest first. | Plex, Tautulli, Sonarr |

It is a single Go binary with an embedded SQLite database, shipped as a ~25 MB
multi-arch container image (amd64 and arm64).

## Running it

### Docker Compose / Synology Container Manager

Copy [`compose.example.yaml`](compose.example.yaml) to `compose.yaml`, adjust it,
and start it:

```bash
docker compose up -d
docker compose logs -f
```

On a Synology NAS, Container Manager → **Project** → **Create** accepts the same file.

Then open `http://<nas>:8484`. On first visit you are asked to create the
administrator account. Until that account exists every page redirects to setup,
so do it straight away. After that, go to **Settings** and connect your services.

#### Volume permissions

Set `PUID` and `PGID` to the user and group that own the `/data` folder on the
host. On Synology, `id <your-nas-user>` over SSH shows them; `1026` and `100` are
typical for the first user created. In Container Manager's own wizard, add them
under **Environment**; no `user` setting is needed.

```yaml
environment:
  PUID: 1026
  PGID: 100
```

The container starts as root only long enough to give `/data` to that user and
group, then drops to them before opening the database or the port; nothing runs
as root after that. Files already owned correctly are left alone, so restarts are
quick. Without `PUID`/`PGID` it uses `65532:65532`, the user earlier images ran as.

Starting the container with an explicit user (`user:` in Compose, `--user` with
`docker run`) still works: it then runs as that user, ignores `PUID`/`PGID`, and
changes no ownership, so the folder must already be writable by it.

If the volume is not writable, the container exits at startup and logs the uid and
gid it is running as.

### Environment variables

Everything else is configured in the web UI. None of these are required.

| Variable | Default | Purpose |
|---|---|---|
| `PUID` / `PGID` | `65532` | User and group to run as; `/data` is given to them at startup |
| `TENDARR_LISTEN_ADDR` | `:8484` | Listen address |
| `TENDARR_DATA_DIR` | `/data` | Where the SQLite database lives |
| `TENDARR_COOKIE_SECURE` | `auto` | `auto` marks the session cookie `Secure` when the request arrived over HTTPS (directly or via `X-Forwarded-Proto`). Set `true` behind an HTTPS proxy that does not send that header |
| `TENDARR_SESSION_TTL` | `168h` | How long a sign-in lasts without activity |
| `TENDARR_TRUSTED_ORIGINS` | – | Comma-separated extra origins allowed to submit forms, e.g. `https://plex-tools.example.com`, for reverse proxies that rewrite `Host` |
| `TENDARR_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `TENDARR_LOG_FORMAT` | `text` | `text` or `json` |
| `TZ` | UTC | Timezone for displayed dates |

Logs go to stdout, so `docker logs` / Container Manager's log view show them.

### Connecting services

| Service | Where the key is |
|---|---|
| Plex | Open any item in Plex Web → **Get Info** → **View XML**; the token is the `X-Plex-Token` value in the URL |
| Tautulli | Settings → Web Interface → API → API Key |
| Radarr / Sonarr | Settings → General → Security → API Key |

Every connection is **tested live before it is saved**. Saving fails on a wrong
key, an unreachable host, or the wrong application at that URL (for example
Sonarr's address in the Radarr field). Keys are never sent back to the browser,
and a blank key field keeps the saved one.

Tautulli is optional but strongly recommended. Plex only reports watch history for
the account that owns the token. Without Tautulli, plays by other household users
are invisible, and their favourites can look unwatched.

## How reap decides

An item is **eligible** only when every guard passes. The guards run twice: once
to build the list, and again immediately before each delete. The delete endpoint
also refuses anything the server cannot see in its own scan, so a stale or
tampered form cannot delete something it should not.

| Guard | Default | Meaning |
|---|---|---|
| Unwatched for | `2y` | Last play (or, if never played, the date added) must be at least this long ago |
| In library for at least | `90d` | Grace period from the date added, so new arrivals are never culled |
| Protect if rated ≥ | `7.5` for movies, off for TV | Rated at or above the floor means protected. **No rating, or fewer votes than the threshold, also means protected**: deleting needs positive evidence a title is bad |
| Min size | off | Skip anything too small to bother with |
| Management | – | Only items Radarr/Sonarr can actually delete, see below |
| Ignored | – | Anything carrying the ignore tag |

The **last watched** date is the later of Plex's and Tautulli's. Neither source
is complete on its own. Tautulli only knows about plays since it was installed,
and Plex only knows the token owner's plays.

### What can and cannot be deleted

| State | Meaning | Deletable |
|---|---|---|
| Managed | Matched in Radarr/Sonarr, and confirmed to be the same files | yes |
| Mismatched | Matched, but Radarr/Sonarr manages a **different copy**, typically a separate 4K library matching the 1080p record by IMDb id | no |
| Orphaned | Matched, but Radarr/Sonarr reports no file. Deleting would free nothing and add an import exclusion | no |
| Unmanaged | No Radarr/Sonarr record | no |
| Fuzzy match | Matched on title and year only | only with **Allow title+year matches** |

"Same files" is proven by comparing the Plex library folder with the Radarr/Sonarr
root folder, plus the exact file name for movies. If the two apps see your storage
under different paths, add **Path mappings** in Settings
(`/movies:/mnt/media/movies`) to make the check exact. Without them it compares the
last folder name, which fails safe: an unproven match is never deletable.

### Ignoring items

**Ignore selected** adds a tag (default `tendarr-ignore`, configurable)
to the item in Radarr or Sonarr. Tagged items move to the **Ignored** tab and are
never offered for deletion. You can also apply the tag by hand in Radarr or
Sonarr; it has the same effect. **Un-ignore** removes it.

### Import exclusions

Deleting adds the item to the import exclusion list by default, so an import list
cannot re-add it tomorrow. **Still-airing series are the exception**: an
exclusion on a continuing show stops its next season from ever arriving, and the
cause is invisible months later. The confirmation page offers that as a separate,
unticked option.

## How upgrade decides

A file is listed when it is at least the minimum size and in one of the selected
codecs (by default x264/h264 and Xvid/DivX). It goes on the **Candidates** tab
only when Radarr/Sonarr could actually act on a search:

- the movie or episode (and its series) is **monitored**
- the quality profile **allows upgrades**
- the file is **below the profile's cutoff format score**, or below its quality
  cutoff
- nothing for it is **already downloading**

Anything else goes on **Skipped** with the reason, because the cause is usually a
Radarr/Sonarr setting rather than the file.

**Check releases** runs an interactive search in the background for up to 50
selected items, one at a time to go easy on indexers. It records the best release
Radarr/Sonarr would accept. Results are cached and shown with their age, and the
**Upgrade available** tab lists only items with an acceptable release, and **Not checked yet** lists those never checked. The same choices are in the **Show** filter.

**Upgrade selected** sends one search command for the selection. Radarr/Sonarr
decides what counts as an upgrade and grabs it.

**Unmonitor selected** is how to leave a file alone. It unmonitors the movie in
Radarr, or the episodes in Sonarr. Unmonitored items are never upgraded, by this
tool or by Radarr/Sonarr's own scheduled searches, and they move to **Skipped**.
**Monitor again** on the Skipped tab reverses it. For an episode, this only works
when its series is still monitored; the app never changes a whole series'
monitoring, because that changes what Sonarr downloads.

Unmonitoring has no effect on reap. Reap's **Ignore** is a separate tag, so you can
stop upgrading a file without protecting it from deletion, and the reverse.

### Scheduled release checks

Settings → **Scheduled release checks** runs the same checks in the background,
so upgrades turn up without you starting them. It is **off by default**.

- **One budget across Radarr and Sonarr**, default 20 an hour, maximum 60. They
  usually search the same indexers, so the combined rate is what trackers see.
- **Evenly spaced**: 20 an hour means one every 3 minutes, never a burst.
- **Never-checked items first**, then the stalest results, largest first, and
  alternating between movies and episodes.
- A result is trusted for the **re-check window** (default 7 days). An item whose
  file has changed is checked again regardless.
- It **pauses for a service** while you run a manual check against it, so the two
  never search the same indexers at once.
- It **only checks; it never grabs**. Results show up under **Upgrade available**,
  and each check is logged to the container log. To act on them automatically,
  turn on scheduled upgrades.

### Scheduled upgrades

Settings → **Scheduled upgrades** grabs what the release checks found, one item at
a time. It is **off by default**.

- **Biggest space saving first**: the item whose acceptable release is smallest
  relative to its current file goes next, across movies and episodes.
- **One budget across Radarr and Sonarr**, default one every 2 hours (1 to 168).
  The gap is stored, so restarting the container does not trigger an early grab.
- **Only fresh results count**: the check must be for the current file and inside
  the scheduled checks' re-check window. It uses the same codec and size filters.
- Each upgrade is the same search as **Upgrade selected**, logged in Activity as
  user `scheduler`. The item's release check is then cleared, so it is only tried
  again once a new check finds something.

### The quality cutoff catch

Once a file meets its profile's *quality* cutoff, Radarr/Sonarr only accepts a
custom-format upgrade from a release of the **same quality**. For example, a
WEBDL-1080p file scoring 2 rejects every Bluray-1080p release, however highly it
scores. Such rows are marked *same quality only*. The fix lives in the quality profile: group the qualities you
consider equivalent (Bluray-1080p with WEB 1080p) so the score decides.

## Security

- Single administrator account, bcrypt-hashed password (minimum 12 characters).
  Failed sign-ins are rate limited per client address.
- Sessions are random 256-bit tokens. Only their SHA-256 hash is stored, and the
  cookie is `HttpOnly` and `SameSite=Lax`. Changing the password signs out every
  other browser.
- Cross-site form posts are refused (Go's `CrossOriginProtection`), and every form
  also carries a per-session CSRF token.
- Strict Content Security Policy: no inline scripts, no third-party assets.
- All input is validated server side. Selections are checked against the
  server's own scan, never trusted from the form.
- API keys are stored in the SQLite database on the data volume. Protect that
  folder like you would the keys themselves.
- Tokens are sent as headers, except Tautulli's, which only accepts a query
  parameter. That key is scrubbed from every error message and log line.

The app is designed for a home network. If you expose it beyond that, put it
behind an HTTPS reverse proxy.

Every delete, upgrade search and tag change is recorded on the **Activity** page
and in the container log.

## Development

Requires Go (see `go.mod` for the version).

```bash
go test -race ./...             # unit and end-to-end tests, no live services needed
golangci-lint run               # lint, as CI runs it
TENDARR_DATA_DIR=./data TENDARR_LISTEN_ADDR=127.0.0.1:8484 go run ./cmd/tendarr
```

The tests run every flow (setup, sign-in, settings validation, reap, ignore,
delete, upgrade search and release checks) against in-process fakes of Plex,
Tautulli, Radarr and Sonarr (`internal/testsvc`). They assert on the exact
requests that would reach the real services.

| Package | Responsibility |
|---|---|
| `internal/clients/*` | HTTP clients for each service |
| `internal/library` | Joins Plex, Tautulli and Radarr/Sonarr; matching and storage verification |
| `internal/reap` | Guards, planning and deletion |
| `internal/upgrade` | Upgrade eligibility and release evaluation |
| `internal/store` | SQLite: accounts, sessions, settings, release checks, audit log |
| `internal/app` | Service layer: caching, actions, audit, background jobs |
| `internal/web` | Handlers, templates, auth |

### CI/CD

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs tests (with the race
detector), `golangci-lint` and `govulncheck` on every push and pull request. It
then builds the image, starts it, and probes it before pushing to GHCR:
`main` from the `main` branch, and the tag name plus `latest` from any git tag.
Pull requests build but do not push.

[Dependabot](.github/dependabot.yml) keeps Go modules, GitHub Actions and the
Docker base images up to date weekly.

After the first push, check the package's visibility under the repository's
**Packages** settings. Make it public if you want to pull it without credentials.

## Similar projects

[Slimarr](https://github.com/theantipopau/slimarr) shrinks a Plex library by
searching Usenet for smaller, better-compressed releases and replacing files
itself via SABnzbd or NZBGet. Tendarr is the better fit if you:

- want **better files, not just smaller ones**: upgrades are judged by your
  Radarr/Sonarr quality profiles and custom format scores;
- want Radarr and Sonarr to stay in charge of searching, grabbing and
  importing, with any indexer or download client they support;
- also want to **remove what nobody watches**, using Plex and Tautulli watch
  history;
- prefer reviewing each change before it happens over overnight automation.

## Licence

[MIT](LICENSE)
