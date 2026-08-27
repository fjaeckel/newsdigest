# newsdigest

A daily brief of your RSS feeds, summarised by Claude, readable on your phone.

Every morning it fetches your feeds, has Claude merge the day's articles into a
short list of topics, and serves them as a web page you can skim and tap to mark
read. Sports (or anything else you name) never makes it in.

- **One Go binary, one container.** No database — digests are JSON files on disk.
- **Works with your Claude subscription.** No per-token API bill required.
- **Phone-first.** Add it to your home screen and it behaves like an app.

## Quick start

```bash
git clone https://github.com/fjaeckel/newsdigest && cd newsdigest

cp config/feeds.example.yaml config/feeds.yaml   # your feeds, your timezone
cp .env.example .env                             # your Claude credential

docker compose up -d --build
```

Then open <http://localhost:8080>. If nothing has been generated yet, hit
**Build one now** rather than waiting until tomorrow morning.

### Two things to put in `.env`

**A Claude credential.** Either one works; the app picks whichever it finds.

| | How | Cost |
|---|---|---|
| Subscription *(recommended)* | Run `claude setup-token` on a machine where you're logged into Claude Code, paste the result as `CLAUDE_CODE_OAUTH_TOKEN` | Included in Pro/Max |
| API key | Put an `ANTHROPIC_API_KEY` from the [console](https://console.anthropic.com/settings/keys) | Per token, roughly a few cents a day |

The subscription path shells out to the Claude Code CLI, which is baked into the
image. If you only ever want the API path, build with
`--build-arg CLAUDE_CLI=false` for a much smaller image.

**A `DIGEST_TOKEN`.** Generate one with `openssl rand -hex 24`. The first time
you open the site on your phone, visit `https://your-host/?t=THAT_VALUE` — it
sets a cookie and you stay logged in. Leave it empty only if the app is on a
network nobody else can reach.

## Configuring it

Everything lives in `config/feeds.yaml`; see
[`config/feeds.example.yaml`](config/feeds.example.yaml) for the annotated
version. The parts you'll actually change:

```yaml
timezone: Europe/Berlin
run_at: "08:00"

exclude:
  topics:
    - Sports of any kind — matches, results, transfers, athletes, leagues.
  keywords: [bundesliga, nfl, olympics]

feeds:
  - name: Tagesschau
    url: https://www.tagesschau.de/index~rss2.xml
```

Exclusion happens twice. `keywords` is a cheap local substring filter that drops
obvious junk before it costs you any tokens; `topics` is free text handed to
Claude, which catches everything the keyword list misses (a football story that
never says "football"). Add anything you're done with — crypto hype, celebrity
news, whatever.

`config/feeds.yaml` is mounted read-only. After editing it, run
`docker compose restart`.

## On your phone

Open the site in Safari or Chrome and use **Add to Home Screen**. It runs
full-screen with its own icon, follows your light/dark setting, and respects the
notch.

- **Tap a topic** to mark it read — it dims, and the unread count drops.
- **Hide read** collapses what you've already seen. The setting sticks per device.
- **Mark all read** when you're done for the morning.
- **‹ ›** move between days; **Archive** lists everything kept.

Read state lives on the server, so marking something read on your phone also
marks it read on your laptop.

## Running it

```bash
docker compose logs -f          # watch it work
docker compose restart          # after editing feeds.yaml
docker compose run --rm newsdigest -once   # generate right now, then exit
```

Without Docker: `go build . && ./newsdigest`. Point `NEWSDIGEST_CONFIG` and
`NEWSDIGEST_DATA` wherever you like.

### Environment variables

| Variable | Default | What it does |
|---|---|---|
| `CLAUDE_CODE_OAUTH_TOKEN` | — | Claude subscription token from `claude setup-token` |
| `ANTHROPIC_API_KEY` | — | Anthropic API key, used if no subscription token is set |
| `NEWSDIGEST_BACKEND` | auto | Force `cli` or `api` instead of auto-detecting |
| `DIGEST_TOKEN` | — | Shared secret for the web UI. Set this if it's internet-facing |
| `NEWSDIGEST_KEEP_DAYS` | `30` | Days of digests to keep; `0` keeps everything |
| `NEWSDIGEST_ADDR` | `:8080` | Listen address |
| `NEWSDIGEST_CONFIG` | `config/feeds.yaml` | Config path |
| `NEWSDIGEST_DATA` | `data` | Data directory |

## How it works

```
feeds.yaml ─▶ fetch all feeds concurrently (26h window)
           ─▶ drop items matching exclude.keywords
           ─▶ one Claude call: merge into ≤N topics, drop excluded subjects
           ─▶ data/digests/YYYY-MM-DD.json
           ─▶ web UI, read state in data/read.json
```

One Claude call per day, so cost and rate limits are a non-issue.

Claude returns topics referencing articles *by index*, and the server maps those
indexes back onto the real feed items. Links and outlet names therefore always
come from your feeds, never from the model — it can't invent a source. Indexes
that don't exist are dropped rather than guessed at.

A feed that's down shows up under **Run details** at the bottom of the page
instead of silently shrinking your brief. Topic IDs are derived from the date
and headline, so regenerating a day keeps your read marks intact.

If the container starts after `run_at` and today's digest is missing, it
generates one immediately rather than leaving you with a blank page until
tomorrow.

## Data on disk

```
data/
  digests/2026-08-27.json    one file per day
  read.json                  which topics you've read
```

Back it up by copying the directory. That's the whole state.

## Tests

```bash
go test ./...
```

The suite runs the full pipeline — fake RSS server, stubbed Claude backend,
rendered HTML — so it never touches the network or your quota.
