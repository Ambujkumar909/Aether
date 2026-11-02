// Idle detection, the way a desktop client does it: no input on this device
// for a while means the person is away from it. The server folds every
// device together -- one active device and the person is not away -- so this
// only has to be honest about this one.
//
// Pure apart from the injected clock, so the thresholds are tested without
// waiting. Wire it to input events (activity), to the page's visibility
// (visibility) and to a coarse timer (tick).
export const IDLE_AFTER = 5 * 60_000; // five minutes, as Teams

export function createIdleWatch({ after = IDLE_AFTER, now = () => Date.now(), onChange, hiddenIsIdle = false } = {}) {
  let last = now();
  let idle = false;
  let hidden = false;
  const set = (v) => {
    if (v === idle) return;
    idle = v;
    onChange?.(idle);
  };
  return {
    // Any input: the person is here.
    activity() {
      last = now();
      set(false);
    },
    // The page went to the background or came back. On a phone the app in
    // the background is the person gone (hiddenIsIdle); on a desktop it is
    // only the timer that decides.
    visibility(isHidden) {
      hidden = isHidden;
      if (!isHidden) {
        last = now();
        set(false);
      } else if (hiddenIsIdle) {
        set(true);
      }
    },
    // Called every so often; flips to idle once the quiet has lasted long
    // enough. Never flips back by itself: only input does that.
    tick() {
      if (!idle && (now() - last >= after || (hidden && hiddenIsIdle))) set(true);
    },
    get idle() {
      return idle;
    },
  };
}
