// The live event stream.
//
// EventSource cannot set an Authorization header, so the token goes in the
// query string — confined to this one route, which is why the daemon accepts
// it there and nowhere else. The stream is loopback-only and the token is
// already in this page, so this is a concession to the browser API rather than
// a weaker credential path.

import { currentToken } from "./api.js";
import { state, update, MAX_EVENTS } from "./state.js";

let source = null;
let retry = 1000;

/**
 * connect opens the stream and hands each frame to onEvent.
 *
 * `since` is 0 on the first connect so the page opens showing history rather
 * than an empty pane until something happens, and the last seen id afterwards
 * so a reconnect neither misses nor duplicates.
 */
export function connect(onEvent) {
  close();
  update({ conn: "connecting" });

  const params = new URLSearchParams({
    token: currentToken() ?? "",
    since: lastEventID ?? 0,
  });
  source = new EventSource(`/v1/events?${params}`);

  source.onopen = () => {
    retry = 1000;
    update({ conn: "live", notice: null });
  };

  source.onmessage = (msg) => {
    let e;
    try {
      e = JSON.parse(msg.data);
    } catch {
      return; // a malformed frame must not kill the stream
    }
    lastEventID = e.id ?? lastEventID;
    state.events.push(e);
    if (state.events.length > MAX_EVENTS * 2) {
      state.events = state.events.slice(-MAX_EVENTS);
    }
    onEvent(e);
  };

  source.onerror = () => {
    update({ conn: "down" });
    close();
    // Back off to a ceiling rather than hammering a daemon that is down, but
    // keep the first few retries quick: the common case is a daemon that was
    // restarted a second ago.
    setTimeout(() => connect(onEvent), retry);
    retry = Math.min(retry * 2, 15000);
  };
}

let lastEventID = null;

export function close() {
  if (source) {
    source.close();
    source = null;
  }
}
