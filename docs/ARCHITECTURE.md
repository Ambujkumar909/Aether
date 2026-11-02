
### 6bk. Video effects

Teams' words, from its package: "Video effects", "Background effects",
"None", "Standard blur", "Portrait blur", "Add new", "Filters", "Soft
focus", "Adjust brightness", "Mirror my video", "Background filters". In the
meeting they sit under Camera's arrow (and More); in the pre-join, behind
"Background filters".

**On this device, and private by construction.**

- The camera's frames are segmented by MediaPipe's selfie segmenter
  (vendored, Apache-2.0: `web/vendor/mediapipe`, version 1.1.1, the SIMD
  WebAssembly build and the 250 KB float16 model).
- They are composited on a canvas, and the canvas's stream is what gets
  published. The room never receives the unprocessed picture.
- An image you add is scaled to 1920 wide and kept in this browser
  (IndexedDB, at most 12, 8 MB each as picked). It is never uploaded. Teams
  uploads custom backgrounds to its cloud. The e2e harness checks that no
  request carries it.
- A background you asked to hide is never shown. Until the segmenter has
  loaded (the first time, a 4 MB gzipped download), or if it cannot load,
  the whole frame is blurred rather than sent as it is.

**No effect costs nothing.** With None and no filters there is no pipeline:
the camera's own track is published. The camera passes through
`camTrackFor` wherever it is acquired, as the microphone passes through
`micTrackFor`. Stopping the published track stops the pipeline and the
camera behind it. Every place that turns the camera off therefore turns its
light off without knowing about effects. The harness checks this, and fails
when the camera release is removed.

**Filters** need no segmenter. "Adjust brightness" lifts a dim picture in
proportion to its mean luma, up to 1.8x, and leaves a bright one alone.
"Soft focus" lays a light blur over the sharp picture. "Mirror my video"
changes only your own view; others always see you the right way round.

**Cost on this machine** (Chrome headless, fake camera at 640x480,
per-frame time measured inside the pipeline):

| | Per frame | One segmentation, with its read-back | Segmented |
| --- | ---: | ---: | --- |
| GPU on (`AETHER_E2E_GPU=1`) | 16.4 ms | 32.7 ms | every 2nd frame |
| GPU off | 26.9 ms | 45.5 ms | every 2nd frame |

- The background is blurred at a quarter of the size and scaled up: a blur
  has no fine detail to lose, and it is a sixteenth of the pixels.
- Where a segmentation costs more than 12 ms, the mask is made every other
  frame. The smoothed mask carries the edge between, and the picture itself
  stays at full rate.
- Most of a segmentation is reading the mask back from the GPU. A WebGL
  compositor that uses the mask where it already is, or the pipeline in a
  worker, is the measured next step. It is marked in `effects.js`.

**On the wire: a finding, and not a flattering one.** A's camera over the
socket media path, 12 s per window (tshark, loopback IPv4, plaintext
server, `tools/e2e/measure-effects.mjs`):

| Window | On the wire | Payload |
| --- | ---: | ---: |
| None | 509 kbit/s | 478 kbit/s |
| Standard blur | 505 kbit/s | 480 kbit/s |
| Portrait blur | 532 kbit/s | 506 kbit/s |
| A plain background | 567 kbit/s | 537 kbit/s |
| None again | 457 kbit/s | 429 kbit/s |

Blurring or replacing the background saves nothing on this path. The VP8
encoder is configured with a 600 kbit/s target and spends it whatever the
picture holds, so a frame that is mostly flat colour costs the same as a
detailed one. Rule 3 says a still picture should cost almost nothing, and
on the camera path it does not. The fix is the encoder's rate control: a
quality target (`bitrateMode: 'quantizer'`, or a constant-quality mode),
not a bitrate target. That is a transport change with its own measurement,
queued next. The fake camera shows no person, so this measures the encoder
against a blurred or flat frame, not a person against a room.

Tests:

- `web/src/effects.test.mjs`: settings never trusted, which settings need a
  pipeline or the segmenter, blur radius across resolutions, the soft mask
  edge, temporal smoothing, the brightness gain.
- `tools/e2e/effects.e2e.mjs`: two browsers, 32 checks, read on the pixels B
  receives. Blur takes A's detail away, a background shows its colour, your
  own image shows and is never uploaded, None brings the camera back, the
  filters, mirroring, camera off releasing the camera, and the choice kept.
- Layout harness: the panel in the meeting, and the pre-join sheet, at
  every width.

**A defect found on the way.** The pipeline's `stop` called the track's
`stop`, which the app had wrapped to call the pipeline's `stop`: a stack
overflow on "None" and on camera off, caught by the first e2e run. The
pipeline now keeps the browser's own `stop`.
