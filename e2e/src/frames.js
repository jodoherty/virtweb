// Wire protocol v1 helpers for the e2e harness — mirrors the browser's
// FrameDecoder in web/static/duplex.js and the server in
// internal/api/frame.go: [tag:1][len:4 big-endian][payload].
export const TAG = {
  hello: 0x01,
  data: 0x02,
  keepalive: 0x03,
};
const PROTO_VERSION = 1;
const MAX_FRAME_PAYLOAD = 16 * 1024 * 1024;

// encodeFrame builds one frame (for input POST bodies).
export function encodeFrame(tag, payload) {
  const out = new Uint8Array(5 + payload.length);
  out[0] = tag;
  out[1] = (payload.length >>> 24) & 0xff;
  out[2] = (payload.length >>> 16) & 0xff;
  out[3] = (payload.length >>> 8) & 0xff;
  out[4] = payload.length & 0xff;
  out.set(payload, 5);
  return out;
}

// FrameDecoder reassembles frames from raw stream chunks; feed()
// returns the DATA payloads (HELLO validated once, KEEPALIVEs dropped
// but reported to the optional onKeepalive observer).
// Throws on torn/oversized/unknown frames.
export class FrameDecoder {
  /** @param {(e: object) => void} [onKeepalive] */
  constructor(onKeepalive) {
    this.pending = null;
    this.helloSeen = false;
    this.onKeepalive = onKeepalive;
  }
  feed(chunk) {
    const old = this.pending;
    const buf = new Uint8Array((old ? old.length : 0) + chunk.length);
    if (old) buf.set(old, 0);
    buf.set(chunk, old ? old.length : 0);
    this.pending = buf;
    const payloads = [];
    for (;;) {
      if (this.pending.length < 5) break;
      const p = this.pending;
      const tag = p[0];
      const len = ((p[1] << 24) | (p[2] << 16) | (p[3] << 8) | p[4]) >>> 0;
      if (len > MAX_FRAME_PAYLOAD) throw new Error('frame payload exceeds the maximum');
      if (this.pending.length < 5 + len) break;
      const payload = this.pending.slice(5, 5 + len);
      this.pending = this.pending.subarray(5 + len);
      switch (tag) {
        case TAG.hello:
          if (this.helloSeen) throw new Error('duplicate HELLO frame');
          if (payload.length !== 2 || payload[0] !== PROTO_VERSION) {
            throw new Error(`unsupported protocol version ${payload.length ? payload[0] : '(none)'}`);
          }
          this.helloSeen = true;
          break;
        case TAG.data:
          payloads.push(payload);
          break;
        case TAG.keepalive:
          this.onKeepalive?.({ tag });
          break;
        default:
          throw new Error('unknown frame tag 0x' + tag.toString(16));
      }
    }
    return payloads;
  }
}
