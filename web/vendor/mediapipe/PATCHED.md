# Local patch to vision_bundle.mjs

MediaPipe Tasks Vision ships a usage logger that batches events and POSTs them
to `https://odml.pa.googleapis.com/v1/log` every 60 seconds. Aether promises
that nothing leaves the organization, so the logger is made inert. Three exact
replacements, each matching once in the original file:

1. `this.l=t??"",this.h=setInterval(()=>{this.flush()},6e4)`
   becomes `this.l=t??"",this.h=void 0` (no timer is ever scheduled);
2. `flush(t,e){if(this.error)` becomes
   `flush(t,e){this.i=[];t?.();return;if(this.error)` (a flush discards the
   batch and reports success without sending);
3. `"https://odml.pa.googleapis.com/v1/log"` becomes `""`.

The server's Content-Security-Policy (`connect-src 'self'`) blocked the request
before this patch; the patch stops it being attempted at all.
`web/src/vendor.test.mjs` fails if any of this is undone, so reapply it after
upgrading the bundle.
