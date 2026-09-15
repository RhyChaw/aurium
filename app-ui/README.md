# Aurium dashboard v2

Drop-in restyle of `internal/api/web/`. Palette, a sidebar shell, and a
terminal drawer. Open `preview.html` in a browser to see both themes with no
daemon running.

## What is in here

| file | what it is |
|---|---|
| `aurium-v2.css` | the whole restyle, as an overlay on your existing `style.css` |
| `preview.html` | static preview of the workspace, dark and light, no daemon needed |
| `logo.svg` | the ingot mark, 20x16 traced pixel art |
| `logo-square.svg` | square version, for the favicon and `cargo tauri icon` |

## Wiring it in, smallest change first

1. Copy `aurium-v2.css`, `logo.svg` and `logo-square.svg` into `internal/api/web/`.
2. In `index.html`, load it after the existing sheet so it overrides:

   ```html
   <link rel="stylesheet" href="/style.css">
   <link rel="stylesheet" href="/aurium-v2.css">
   <link rel="icon" type="image/svg+xml" href="/logo-square.svg">
   ```

That alone gives you the palette, the molten selected tile, the animated
header hairline and the gold spend figures. Nothing in `views/` has to change,
because the overlay only targets classes your markup already emits.

Add `class="is-gold"` to the spend `.stat-value` in `views/heartbeat.js` and to
the cost `.total-value` in `views/usage.js` if you want the ramp on those
numbers.

## The sidebar

This one needs markup. Today `index.html` is a column of
`header / nav / notice / main / hint-bar`. The sidebar wants the nav out of
that column and `header` and `main` inside a right hand pane:

```html
<div class="shell">
  <aside class="sidebar">
    <div class="wordmark"><img src="/logo.svg" alt=""><span>Aurium</span></div>
    <nav id="tabs" class="side-nav"></nav>
    <div class="side-group">
      <h2>Projects</h2>
      <div id="side-projects"></div>
    </div>
    <div class="side-foot">
      <span id="conn" class="pill-live">live</span>
      <button id="theme" class="mini">system</button>
    </div>
  </aside>
  <div class="pane">
    <header>...</header>
    <p id="notice" class="notice" role="status" aria-live="polite" hidden></p>
    <main id="panel" data-panel=""></main>
  </div>
</div>
```

`.pane` is `display:flex; flex-direction:column; min-height:0`. The tab
builder in `app.js` changes class from `.tab` to `.side-item` and `.is-active`
stays as it is; the count badge becomes `.side-meta` or keeps `.count`.

Below 900px `.shell` collapses to one column, so the sidebar stacks above the
pane the same way the three panes already do.

## The terminal drawer

Also new markup. It goes at the end of `.centre-col`, after `.repos-strip`:

```html
<section class="term">
  <div class="term-head">
    <span class="term-title">&gt;_ terminal</span>
    <button class="term-tab is-active">planner</button>
    <button class="term-tab" data-state="attention">reviewer-3</button>
    <div class="term-actions">
      <button class="mini">attach</button>
      <button class="mini">clear</button>
    </div>
  </div>
  <div class="term-body">
    <div class="term-line">
      <span class="term-ts">12:04:09</span>
      <span class="term-who" style="color:var(--lane-agent)">planner</span>
      <span class="term-text">ok  internal/api  2.418s</span>
    </div>
    <div class="term-prompt">
      <span class="host">aurium@planner</span>
      <span class="branch">au/rail-perf</span>
      <span class="sig">$</span>
      <i class="term-caret"></i>
    </div>
  </div>
</section>
```

The daemon side does not exist yet. Container stdout is not on the event
stream today, so this needs either a `/v1/containers/{id}/logs` endpoint or a
`container.log` frame on the existing SSE stream. Until then the drawer can
carry the `container.*` events already on the stream, which is honest and
still useful: it shows the container coming up, the worktree cloning, and the
guard hook refusing a push.

Two notes on the prompt line. It is decoration unless you wire stdin, and
`aurium attach` already exists for taking the wheel; consider making the whole
prompt row the attach button rather than faking an input.

## Contrast floor

`--muted` is `#9B95A5` on dark, which is 6:1 on the new ground, and
`--muted-2` at `#8C8698` is 5.2:1. Those are the two dimmest values used for
text anywhere. `--border` and `--border-strong` are borders only. Deepening
the background without keeping these is what turns the theme unreadable, and
it is the one change here that is easy to undo by accident.
