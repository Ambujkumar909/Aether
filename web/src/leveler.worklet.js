// AudioWorklet wrapper for leveler.js. Mono in, mono out; passes silence
// through when there is no input.
import { createLeveler } from './leveler.js';

class LevelerProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.lev = createLeveler();
    this.stopped = false;
    this.port.onmessage = (e) => {
      if (e.data === 'destroy') this.stopped = true;
    };
  }
  process(inputs, outputs) {
    if (this.stopped) return false;
    const inp = inputs[0]?.[0];
    const out = outputs[0]?.[0];
    if (!out) return true;
    if (!inp) {
      out.fill(0);
      return true;
    }
    this.lev.process(inp, out);
    return true;
  }
}
registerProcessor('aether-leveler', LevelerProcessor);
