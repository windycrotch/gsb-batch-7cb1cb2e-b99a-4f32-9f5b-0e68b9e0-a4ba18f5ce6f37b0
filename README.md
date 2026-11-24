# `pinhole`

<a href="https://godoc.org/github.com/tidwall/pinhole"><img src="https://img.shields.io/badge/api-reference-blue.svg?style=flat-square" alt="GoDoc"></a>

3D Wireframe Drawing Library for Go

[Javascript Version](https://github.com/tidwall/pinhole-js)  
[Demo](https://tidwall.com/pinhole/)


<img src="http://i.imgur.com/EhtVA6C.jpg" width="300" height="300" alt="earth"><img src="http://i.imgur.com/fKe1N3E.jpg" width="300" height="300" alt="shapes">
<img src="http://i.imgur.com/qQRqGPe.jpg" width="300" height="300" alt="spiral"><img src="http://i.imgur.com/FbO8tY4.jpg" width="300" height="300" alt="gopher">

## Why does this exist?

I needed a CPU based 3D rendering library with a very simple API for visualizing data structures. No bells or whistles, just clean lines and solid colors.

## Getting Started

### Installing

To start using `pinhole`, install Go and run `go get`:

```sh
$ go get -u github.com/tidwall/pinhole
```

This will retrieve the library.

### Using

The coordinate space has a locked origin of `0,0,0` with the min/max boundaries of `-1,-1,-1` to `+1,+1,+1`.
The `Z` coordinate extends from `-1` (nearest) to `+1` (farthest).

There are four types of shapes; `line`, `cube`, `circle`, and `dot`. 
These can be transformed with the `Scale`, `Rotate`, and `Translate` functions.
Multiple shapes can be transformed by nesting in a `Begin/End` block.


A simple cube:

```go
p := pinhole.New()
p.DrawCube(-0.3, -0.3, -0.3, 0.3, 0.3, 0.3)
p.SavePNG("cube.png", 500, 500, nil)
```

<img src="http://i.imgur.com/ofJ2T7Y.jpg" width="300" height="300">


Rotate the cube:

```go
p := pinhole.New()
p.DrawCube(-0.3, -0.3, -0.3, 0.3, 0.3, 0.3)
p.Rotate(math.Pi/3, math.Pi/6, 0)
p.SavePNG("cube.png", 500, 500, nil)
```

<img src="http://i.imgur.com/UewuE4L.jpg" width="300" height="300">

Add, rotate, and transform a circle:

```go
p := pinhole.New()
p.DrawCube(-0.3, -0.3, -0.3, 0.3, 0.3, 0.3)
p.Rotate(math.Pi/3, math.Pi/6, 0)

p.Begin()
p.DrawCircle(0, 0, 0, 0.2)
p.Rotate(0, math.Pi/2, 0)
p.Translate(-0.6, -0.4, 0)
p.Colorize(color.RGBA{255, 0, 0, 255})
p.End()

p.SavePNG("cube.png", 500, 500, nil)
```

<img src="http://i.imgur.com/UafJsKW.jpg" width="300" height="300">

## Incremental framing

`Image` and `SavePNG` never mutate the scene: rendering sorts an internal copy
of the lines, so the order of `p.lines` — and therefore every `Begin/End`
block range — is preserved. The same `Pinhole` can therefore be rotated and
rendered frame after frame, and each frame is pixel-identical to a scene
rebuilt from scratch with the same sequence of operations.

```go
p := pinhole.New()
p.DrawCube(-0.3, -0.3, -0.3, 0.3, 0.3, 0.3)
for i := 0; i < n; i++ {
    p.Rotate(0, math.Pi/30, 0)
    // identical to rendering a freshly rebuilt scene at this frame
    img := p.Image(500, 500, nil)
    _ = img
}
```

Misuse is non-fatal and never leaks state into later frames:

- an `End` without a matching `Begin` is a no-op; nested blocks keep
  transforms scoped to the currently open block,
- calling `Begin` again before `End` (including after an in-block render) does
  not corrupt block scoping,
- `Center` on an empty scene or empty open block is a no-op,
- a failed `SavePNG` (e.g. an unwritable path) returns the error and leaves
  the `Pinhole` fully usable for the next `Image`.

A runnable end-to-end sample lives in
[`examples/incremental`](examples/incremental/main.go).

## Contact

Josh Baker [@tidwall](http://twitter.com/tidwall)

## License

`pinhole` source code is available under the ISC [License](/LICENSE).

