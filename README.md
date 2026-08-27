# newsdigest

A daily brief of your RSS feeds, summarised by Claude, readable on your phone.

Every morning it fetches your feeds, has Claude merge the day's articles into a
short list of topics, and serves them as a web page you can skim and tap to mark
read. Sports (or anything else you name) never makes it in.

Each feed belongs to a **category** — news, cycling, aviation, whatever you like
— and every category gets its own brief and its own standing feed. A category
holds unread topics until you read them, so missing a morning doesn't mean
missing the news.

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

# Per category, per day — not a total shared across them.
max_topics: 18

exclude:
  topics:
    - Sports of any kind — matches, results, transfers, athletes, leagues.
  keywords: [bundesliga, nfl, olympics]

feeds:
  - name: Tagesschau
    category: news
    url: https://www.tagesschau.de/index~rss2.xml
  - name: road.cc
    category: cycling
    url: https://road.cc/feed
```

Every feed names a `category`, and each category is briefed separately: one
Claude call per category that has items that day, with its own `max_topics`
budget. That's what stops a loud category from crowding out a quiet one — and
it's also why the categories you choose drive the token bill. A feed with no
`category` falls back to `general`. Categories appear on the home screen in the
order they first appear in the file, so the file's order is the reader's order.

Exclusion happens twice. `keywords` is a cheap local substring filter that drops
obvious junk before it costs you any tokens; `topics` is free text handed to
Claude, which catches everything the keyword list misses (a football story that
never says "football"). Add anything you're done with — crypto hype, celebrity
news, whatever.

`config/feeds.yaml` is mounted read-only. After editing it, run
`docker compose restart`.

## On your phone

Open the site in Safari or Chrome and use **Add to Home Screen**. It runs
full-screen with its own icon and respects the notch. The design is
black-on-white by choice and stays that way regardless of your device theme.

- **Tap a topic** to mark it read — it dims, and the unread count drops.
- **Hide read** collapses what you've already seen. The setting sticks per device.
- **Mark all** when you're done — on a category feed this clears every day it
  spans, not just the one on screen.
- **‹ ›** move between days; **Archive** lists everything kept.

Sources sit under each topic as one chip per outlet. When an outlet ran several
pieces on a story they're all kept — the chip carries a count and opens to the
individual articles by headline. **Expand sources** flips whether those open by
default; either way you can always tap a chip to dig in. That setting sticks per
device too.

The home screen is your categories, each with its unread count. Tapping one
opens `/c/<category>`: that category's standing feed, newest day first, holding
every topic briefed into it that you haven't read yet. Read topics drop out of
the view — **Show read** brings them back — so the feed works as an inbox rather
than a daily snapshot you can miss. A day heading disappears once everything
under it has been read.

Each day heading links back to `/d/<date>`, the full cross-category brief for
that day, which is also reachable from the bottom of the home screen.

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
instead of silently shrinking your brief. A category whose brief fails costs
that category its topics for the day, not the whole morning — the failure is
listed alongside the dead feeds. Topic IDs are derived from the date, category
and headline, so regenerating a day keeps your read marks intact.

**On startup** it makes sure the day has something to read:

- Nothing synced yet today → it briefs immediately, whatever the time. Waiting
  for `run_at` would leave a blank page for hours and there's nothing to lose by
  running early.
- Today ran but produced nothing in any category → that's a real result over a
  quiet window, so `run_at` still guards the retry. Before it, the day is left
  alone; after it, a restart retries.
- Today already has topics → never regenerated, however uneven the categories
  are. A quiet category is normal, and treating one as a gap to fill would
  re-run the model on every restart.

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
