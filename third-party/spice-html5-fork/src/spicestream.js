"use strict";
/*
   spice-html5 fork — transport replacement.

   Upstream spice-html5 is hardwired to WebSocket (SpiceConn creates
   `new WebSocket(o.uri, 'binary')` and every child channel clones
   `this.ws.url`), with no seam to replace it. This fork substitutes
   SpiceStream: a WebSocket-shaped channel over virtweb's plain-HTTP
   DuplexChannel (chunked flushed GET stream + ordered POST uplink).
   No websockify, no WebSocket, no duplex fetch.

   The surface SpiceConn/wire.js actually touch:
     addEventListener('open'|'message'|'close'|'error', fn)
         — handlers are called with `this` = the stream, and message
           events carry { data: ArrayBuffer } (binaryType semantics)
     send(ArrayBuffer)
     close(), readyState (0 connecting .. 3 closed), url, binaryType
   Everything else in the tree (display, inputs, cursor, wire reader)
   is transport-agnostic.

   Copyright / license of the surrounding tree: see COPYING /
   COPYING.LESSER (LGPL-3.0-or-later, spice-html5).
*/

import { DuplexChannel } from '/static/duplex.js';

function SpiceStream(o)
{
    if (o === undefined || o.uri === undefined || !o.uri)
        throw new Error("You must specify a uri");
    if (o.inputUri === undefined || !o.inputUri)
        throw new Error("You must specify an inputUri");

    this.url = o.uri;
    this.inputUrl = o.inputUri;
    this.binaryType = "arraybuffer";
    this.readyState = 0; // CONNECTING
    this._listeners = { open: [], message: [], close: [], error: [] };
    this._dead = false;

    var self = this;
    var ch = new DuplexChannel(o.uri, o.inputUri);
    this._ch = ch;

    ch.open().then(function() {
        if (self._dead)
            return;
        self.readyState = 1; // OPEN
        self._emit("open", {});
    }).catch(function(e) {
        if (self._dead)
            return;
        self.readyState = 3; // CLOSED
        self._emit("error", e);
        self._emit("close", { code: 1006, reason: String(e.message || e) });
    });

    // Downlink: ReadableStream chunks -> 'message' events. The
    // SpiceWireReader reassembles them into protocol messages exactly
    // as it would WebSocket messages.
    (async function() {
        try {
            for await (const value of ch.readable) {
                self._emit("message", { data: value });
            }
            if (self.readyState !== 3) {
                self.readyState = 3;
                self._emit("close", { code: 1000, reason: "" });
            }
        } catch (e) {
            if (self.readyState === 3)
                return;
            self.readyState = 3;
            self._emit("error", e);
            self._emit("close", { code: 1006, reason: String(e.message || e) });
        }
    })();
}

SpiceStream.prototype = {
    addEventListener: function(type, fn) {
        (this._listeners[type] = this._listeners[type] || []).push(fn);
    },

    _emit: function(type, e) {
        var fns = this._listeners[type] || [];
        for (var i = 0; i < fns.length; i++)
            fns[i].call(this, e);
    },

    // Sends one protocol frame. Writes before the stream is open are
    // queued by DuplexChannel and flushed in order on open, so the
    // link handshake (sent right after 'open') needs no special case.
    send: function(mb) {
        this._ch.write(mb);
    },

    close: function() {
        if (this._dead)
            return;
        this._dead = true;
        this.readyState = 2; // CLOSING
        this._ch.close();
        // The 'close' event fires when the downlink pump notices the
        // stream ended; SpiceConn ignores it in the deliberate-teardown
        // case (state already "closing").
    },
};

export { SpiceStream };
