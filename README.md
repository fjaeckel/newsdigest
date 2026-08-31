# newsdigest

A daily brief of your RSS feeds, summarised by Claude, readable on your phone.

Every morning it fetches your feeds, has Claude merge the day's articles into a
short list of topics, and serves them as a web page you can skim and tap to mark
read. Sports (or anything else you name) never makes it in.

The front page is a handful of paragraphs in the style of The Economist's *The
World in Brief* — but written from **your** feeds, so a groupset launch or a
rule change in aviation can lead it on the day it happens. Underneath sit your
standing feeds, one per **category**, which is where you read everything.

Categories are briefed in one of two modes, because there are two reasons to
follow a subject:

| | `mode: brief` | `mode: complete` |
|---|---|---|
| What it is | An edited selection | A reading list |
| Merging | Aggressive, across outlets | Only when plainly the same story |
| Dropping | Yes, past `max_topics` | **Never** |
| Laid out by | Importance | Outlet |
| Good for | The news | Aviation, bikes, anything you follow closely |

A complete category is the "I don't want to miss a thing" mode, and it is
enforced in code rather than trusted to the prompt: any article Claude fails to
mention is added back with the feed's own headline and link. See
[How it works](#how-it-works).

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

categories:
  news:
    mode: brief          # edited down, most important first
  aviation:
    mode: complete       # every article, grouped by outlet
  cycling:
    mode: complete

brief:
  enabled: true          # the cross-category front page
  max_items: 8

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

The `categories:` block is optional and only says how each one is briefed;
leave it out and everything is `brief`, as it was before modes existed. Each
entry takes `mode`, an optional `max_topics` of its own, and `group_by`
(`importance` or `feed`) if you don't want the layout that follows from the
mode.

Exclusion happens twice. `keywords` is a cheap local substring filter that drops
obvious junk before it costs you any tokens; `topics` is free text handed to
Claude, which catches everything the keyword list misses (a football story that
never says "football"). Add anything you're done with — crypto hype, celebrity
news, whatever.

Exclusion still applies inside a complete category — "don't miss anything" is
not the same as "show me the things I've told you I never want". The difference
is that a complete category has to *declare* what it dropped, so a deliberate
exclusion can be told apart from an article that simply went missing.

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

The home screen opens on **Today's top stories**: this morning's front page,
four to eight paragraphs drawn from every category, each opening on the country,
company or person it's about, with the outlets behind it as chips underneath.
It's a skim and nothing more — every story in it is also waiting in its own
feed, so skipping the front page can't cost you anything.

Under it are your categories, each with its unread count; a category briefed in
full is marked **all**. Tapping one opens `/c/<category>`: that category's
standing feed, newest day first, holding every topic briefed into it that you
haven't read yet. A complete category is grouped under an outlet heading per
day, in the order the feeds appear in your config, so you can read it the way
you'd read the outlets themselves. Read topics drop out of the view — **Show
read** brings them back — so the feed works as an inbox rather than a daily
snapshot you can miss. A day, or an outlet within it, disappears once everything
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
           ─▶ per category, one Claude call: merge into topics
           ─▶ complete categories: add back anything the model lost
           ─▶ one more call: the front page, written from those topics
           ─▶ data/digests/YYYY-MM-DD.json
           ─▶ web UI, read state in data/read.json
```

A handful of Claude calls per day, so cost and rate limits stay a non-issue.

Claude returns topics referencing articles *by index*, and the server maps those
indexes back onto the real feed items. Links and outlet names therefore always
come from your feeds, never from the model — it can't invent a source. Indexes
that don't exist are dropped rather than guessed at.

### How "complete" is actually guaranteed

Asking a model nicely to cover everything is not a guarantee, so the app checks.
A complete category's brief must account for every item: each one either appears
in a topic's `source_indexes` or is named in `excluded_indexes` as a deliberate
drop under one of your `exclude.topics` rules. Anything in neither list is an
article the model quietly lost, and the app adds it back as its own topic
carrying the feed's headline, blurb and link. **Unsummarised is a far better
failure than absent.**

Two consequences worth knowing:

- A long day is split into several calls of 60 items rather than trimmed, since
  a complete category can't answer volume by dropping the tail. Items are
  chunked newest-first, so two outlets covering the same thing usually land in
  the same call, which is where they can still be merged.
- **Run details** at the bottom of a day's brief reports coverage per category —
  "31 of 31 articles covered in 27 topics" — including how many were added back.
  That line is the claim that nothing was missed, stated rather than assumed.

### The front page

The front page is written from the finished topics, never from the raw feed, so
it can't report anything a category didn't. Each paragraph cites the topics it
drew on, which is how the page shows real outlet chips under prose the model
wrote. If the call fails, you lose the front page and keep the morning: the
failure is listed under **Run details** and the categories are untouched — and
because it's written from stored topics, the next restart can repair it with a
single call rather than a full re-run.

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

There's one exception to that last rule, and it's narrow on purpose. If the day
has topics but no **front page** — it was briefed before the front page existed,
or that one call failed — startup writes just the front page. No feeds are
fetched and no category is briefed again, because the front page is written from
stored topics anyway: one Claude call rather than one per category, and since no
topic is rebuilt, no read mark moves.

A front page call that completes but produces nothing is recorded as such, not
left blank, so a day that has genuinely been tried isn't tried again on every
restart. Those retries wait for `run_at`, like an empty morning does; a day that
has simply never been asked is written immediately.

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
