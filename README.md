Digital Rain
============

Digital Rain is an HTML5 + Canvas demo app written entirely in [Go](http://golang.org/).
It's intended to mimic the look of the [falling text](https://www.youtube.com/watch?v=rpWrtXyEAN0) in the movie [The Matrix](http://www.imdb.com/title/tt0133093/).

[Live Demo](http://tidwall.com/digitalrain/)

Build
-----

Install [Go](http://golang.org/) and [GopherJS](http://github.com/gopherjs/gopherjs)

```bash
# Prepare GOPATH
export GOPATH=$(go env GOPATH)
export PATH=$PATH:$(go env GOPATH)/bin

# Install gopherjs
go install github.com/gopherjs/gopherjs@v1.18.0-beta2

# Install specific go version for gopherjs
go install golang.org/dl/go1.18.10@latest
go1.18.10 download
export GOPHERJS_GOROOT="$(go1.18.10 env GOROOT)"

# Build and Serve
gopherjs build digitalrain.go --minify
gopherjs serve
```

Verifying smooth canvas rebuilds
--------------------------------

After `gopherjs serve`, open the page in a browser and open DevTools
(Console + Elements). The page exposes `window.digitalRain` with the two
rain layers (`rain1`, `rain2`) for inspection.

1. **Rebuild is a smooth migration.** Resize the window (or zoom with
   Ctrl +/- to change `devicePixelRatio`). Watch any single column of
   drops: it keeps falling continuously — no teleporting, no brightness
   pop. In the console, `digitalRain.rain1.Generation()` increments by
   exactly one per effective rebuild.
2. **Old canvas is gone.** After several resizes,
   `document.querySelectorAll('canvas').length` is still `2` — the node
   count never grows with rebuilds.
3. **Old listeners are inert.** In Chrome/Edge DevTools, select a canvas
   in Elements and run `getEventListeners($0)`: exactly one `click` and
   one `mousemove` listener per live canvas. Detached canvases are not
   reachable from the DOM, and their captured generation no longer
   matches, so even a synthetically dispatched event on a stale canvas
   reference is ignored (the handler returns early).
4. **Clicks still work after rebuilds.** Resize, then click blank space:
   the glyph color scheme and page background toggle (low/high
   brightness). Click the bottom-right link area: it navigates to the
   GitHub link. Rapid clicking interleaved with resizing ends in the
   color scheme of the last effective click.
5. **Failed rebuild keeps the old canvas.** In the console, hide the
   body (`document.body.style.display='none'`) and dispatch
   `window.dispatchEvent(new Event('resize'))`: the generation count
   does not increase and the previous canvases keep rendering once the
   body is shown again.

License
-------

Digital Rain is available under the [MIT License](LICENSE).
