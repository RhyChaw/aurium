# aurium.dev source

Static landing page. No build step, no dependencies, no bundler.

| file | what it is |
|---|---|
| `index.html` | the page |
| `support.js` | the render runtime it loads |
| `logo.svg` | the ingot mark, 20x16 traced pixel art |
| `logo-square.svg` | square version, for favicons and `cargo tauri icon` |
| `.nojekyll` | stops Pages running Jekyll over it |

## Publishing

Settings > Pages > Source: **Deploy from a branch** > Branch: `main`, folder: `/docs`.

It is live at `https://rhychaw.github.io/aurium/` about a minute later. For a custom
domain, add a `CNAME` file here containing the bare domain and point a CNAME record
at `rhychaw.github.io`.

## Editing

The page is one file. Copy is plain text in the markup; the repeated blocks
(pillars, use cases, steps, download cards) are arrays at the bottom of
`index.html` in the `renderVals()` method. Change the array, the page changes.
