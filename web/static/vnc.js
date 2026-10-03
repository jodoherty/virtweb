// VNC viewer (M6 groundwork): noVNC 1.7.0 (vendored, third-party/novnc,
// MPL-2.0) is imported now so the vendored embedding is exercised in the
// browser; the transport lands in M6 — an `ssh -W` stdio TCP tunnel to
// the VM's VNC server, bridged over the same chunked-HTTP streams
// pattern as the serial console (RFB messages as byte streams in both
// directions), since noVNC speaks WebSocket.
import RFB from '/novnc/core/rfb.js';

document.getElementById('vnc-status').textContent =
  'noVNC is loaded, but the VNC transport arrives in milestone M6.';
void RFB; // referenced in M6
