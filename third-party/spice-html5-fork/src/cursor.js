"use strict";
/*
   Copyright (C) 2012 by Jeremy P. White <jwhite@codeweavers.com>

   This file is part of spice-html5.

   spice-html5 is free software: you can redistribute it and/or modify
   it under the terms of the GNU Lesser General Public License as published by
   the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   spice-html5 is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Lesser General Public License for more details.

   You should have received a copy of the GNU Lesser General Public License
   along with spice-html5.  If not, see <http://www.gnu.org/licenses/>.
*/

import { create_rgba_png } from './png.js';
import { Constants } from './enums.js';
import { DEBUG } from './utils.js';
import {
  SpiceMsgCursorInit,
  SpiceMsgCursorMove,
  SpiceMsgCursorSet,
} from './spicemsg.js';
import { SpiceSimulateCursor } from './simulatecursor.js';
import { SpiceConn } from './spiceconn.js';

/*----------------------------------------------------------------------------
**  SpiceCursorConn
**      Drive the Spice Cursor Channel
**--------------------------------------------------------------------------*/
function SpiceCursorConn()
{
    SpiceConn.apply(this, arguments);
}

/* The largest cursor image Chromium and Firefox will show. */
var MAX_CSS_CURSOR = 128;

SpiceCursorConn.prototype = Object.create(SpiceConn.prototype);


/* virtweb delta: live cursor telemetry. A "the cursor doesn't show"
   report is otherwise impossible to answer from the other end of the
   wire: these records (window.__spice.cursorDebug) and the lines in
   the viewer's Log panel say exactly which shapes the guest sent
   (protocol type number), whether each was applied, and how (CSS
   cursor, simulated-cursor fallback, or rejected). */
var CURSOR_TYPE_NAMES = { 0: "alpha", 1: "mono", 2: "color4", 3: "color8",
                          4: "color16", 5: "color24", 6: "color32" };

SpiceCursorConn.prototype.cursor_note = function(event, cursor)
{
    var rec = { event: event, at: Date.now() };
    if (cursor && cursor.header)
    {
        rec.w = cursor.header.width;
        rec.h = cursor.header.height;
        rec.type = cursor.header.type;
        rec.typeName = CURSOR_TYPE_NAMES[cursor.header.type] ||
                       ("unknown(" + cursor.header.type + ")");
        rec.hot = [cursor.header.hot_spot_x, cursor.header.hot_spot_y];
        if (cursor.data)
            rec.dataLen = cursor.data.byteLength !== undefined ?
                cursor.data.byteLength : cursor.data.length;
        rec.flags = cursor.flags;
        if (cursor.flags & Constants.SPICE_CURSOR_FLAGS_FROM_CACHE)
            rec.fromCache = true;
    }
    if (this.parent)
    {
        var dbg = this.parent.cursorDebug =
            this.parent.cursorDebug || { count: 0 };
        dbg.count++;
        dbg.last = rec;
    }
    return rec;
};

/* virtweb delta: client-side cursor shape cache
   (SPICE_CURSOR_FLAGS_CACHE_ME / FROM_CACHE). The spice-server sends
   a full shape with CACHE_ME when a cursor first appears, and from
   then on re-selecting that cursor sends a FROM_CACHE SET carrying
   only the header — no pixel data — with the client expected to keep
   the shape in a cache keyed by header.unique (protocol: the unique
   id is "used for storing and retrieving cursors from the cursor
   cache"). Upstream keeps no cache: every FROM_CACHE arrives with
   zero pixels, the conversion fails, and the previously rendered
   shape stays on screen. That is exactly why the guest's default
   arrow — which the server always re-sends as a reference, never as
   pixels — never came back once a special cursor had been shown. */
SpiceCursorConn.prototype.cache_shape = function(cursor)
{
    if (cursor && cursor.header && cursor.data &&
        cursor.flags & Constants.SPICE_CURSOR_FLAGS_CACHE_ME)
    {
        this.cursor_shapes = this.cursor_shapes || {};
        this.cursor_shapes[String(cursor.header.unique)] = cursor;
    }
};

SpiceCursorConn.prototype.lookup_shape = function(header)
{
    var entry = this.cursor_shapes &&
        this.cursor_shapes[String(header && header.unique)];
    return (entry && entry.data) ? entry : null;
};

SpiceCursorConn.prototype.drop_shapes = function(unique)
{
    if (! this.cursor_shapes)
        return;
    if (unique !== undefined && unique !== null)
        delete this.cursor_shapes[String(unique)];
    else
        this.cursor_shapes = {};
};

SpiceCursorConn.prototype.process_channel_message = function(msg)
{
    if (msg.type == Constants.SPICE_MSG_CURSOR_INIT)
    {
        var cursor_init = new SpiceMsgCursorInit(msg.data);
        DEBUG > 1 && console.log("SpiceMsgCursorInit");
        if (this.parent && this.parent.inputs &&
            this.parent.inputs.mouse_mode == Constants.SPICE_MOUSE_MODE_SERVER)
        {
            // FIXME - this imagines that the server actually
            //          provides the current cursor position,
            //          instead of 0,0.  As of May 11, 2012,
            //          that assumption was false :-(.
            this.parent.inputs.mousex = cursor_init.position.x;
            this.parent.inputs.mousey = cursor_init.position.y;
        }
        /* virtweb delta: INIT carries the CURRENT cursor shape
           (SpiceCursor, per the protocol — "the server sends
           SPICE_MSG_CURSOR_INIT to set current pointer state (i.e.,
           shape, position, visibility)"), and QEMU sends the initial
           shape ONLY there: a separate CURSOR_SET arrives later just
           when the guest CHANGES the shape. Upstream discards the
           shape ("we don't handle most of the parameters"), so the
           default arrow — the shape active when the channel opens —
           was never rendered, while every later shape change
           (hourglass, resize, ...) was: the client showed real
           cursors only for the non-default ones. Apply it like a
           SET; honour the visible flag. */
        var rec = this.cursor_note("INIT", cursor_init.cursor);
        rec.visible = !!cursor_init.visible;
        if (rec.w)
            this.log_info("cursor: INIT shape " + rec.w + "x" + rec.h + " " +
                          rec.typeName +
                          (rec.visible ? " (visible)" : " (hidden)"));
        if (cursor_init.cursor)
        {
            if (cursor_init.visible)
            {
                if (! this.set_cursor(cursor_init.cursor))
                    this.warn_once_per_cursor_type(cursor_init.cursor.header.type);
            }
            else
                this.hide_cursor();
        }
        return true;
    }

    if (msg.type == Constants.SPICE_MSG_CURSOR_SET)
    {
        var cursor_set = new SpiceMsgCursorSet(msg.data);
        DEBUG > 1 && console.log("SpiceMsgCursorSet");
        if (cursor_set.cursor.flags & Constants.SPICE_CURSOR_FLAGS_NONE)
        {
            this.hide_cursor();
            return true;
        }

        /* CACHE_ME / FROM_CACHE are handled below; only genuinely
           unknown bits are a problem. (The upstream FIXME here
           predates that handling.) */
        if (cursor_set.cursor.flags &
            ~(Constants.SPICE_CURSOR_FLAGS_NONE |
              Constants.SPICE_CURSOR_FLAGS_CACHE_ME |
              Constants.SPICE_CURSOR_FLAGS_FROM_CACHE))
            this.log_warn("FIXME: No support for cursor flags " + cursor_set.cursor.flags);

        /* An unconvertible shape is not an unhandled message: returning
           false here made the base class log a second, misleading
           "Unknown message type 103" for every one of these. */
        var rec = this.cursor_note("SET", cursor_set.cursor);
        /* virtweb delta: restore a cached shape (header only, no
           pixel data) — see cache_shape above. */
        if (cursor_set.cursor.flags & Constants.SPICE_CURSOR_FLAGS_FROM_CACHE)
        {
            var cached = this.lookup_shape(cursor_set.cursor.header);
            if (cached)
            {
                this.set_cursor(cached);
                return true;
            }
            this.log_warn("cursor: FROM_CACHE reference to unknown shape " +
                          cursor_set.cursor.header.unique +
                          "; keeping the last shape.");
            return true;
        }
        if (cursor_set.cursor.flags == 0 &&
            (! this.lastSetLogged ||
             this.lastSetLogged.w != rec.w || this.lastSetLogged.h != rec.h ||
             this.lastSetLogged.type != rec.type))
        {
            /* Windows sends a SET on nearly every hover; log a shape
               only when it actually differs from the last one logged. */
            this.lastSetLogged = rec;
            this.log_info("cursor: SET shape " + rec.w + "x" + rec.h + " " +
                          rec.typeName);
        }

        if (! this.set_cursor(cursor_set.cursor))
            this.warn_once_per_cursor_type(cursor_set.cursor.header.type);

        return true;
    }

    if (msg.type == Constants.SPICE_MSG_CURSOR_MOVE)
    {
        /* Only meaningful under server mouse mode, where the server owns
           the pointer position and we track it for inputs. Under client
           mode the browser draws the pointer and already knows where it
           is, so there is nothing to do -- but it is handled either way,
           rather than reported as unimplemented on every move. */
        var cursor_move = new SpiceMsgCursorMove(msg.data);
        if (this.parent && this.parent.inputs &&
            this.parent.inputs.mouse_mode == Constants.SPICE_MOUSE_MODE_SERVER)
        {
            this.parent.inputs.mousex = cursor_move.position.x;
            this.parent.inputs.mousey = cursor_move.position.y;
        }
        return true;
    }

    if (msg.type == Constants.SPICE_MSG_CURSOR_HIDE)
    {
        DEBUG > 1 && console.log("SpiceMsgCursorHide");
        this.cursor_note("HIDE", null);
        this.log_info("cursor: HIDE");
        this.hide_cursor();
        return true;
    }

    if (msg.type == Constants.SPICE_MSG_CURSOR_TRAIL)
    {
        this.known_unimplemented(msg.type, "Cursor Trail");
        return true;
    }

    if (msg.type == Constants.SPICE_MSG_CURSOR_RESET)
    {
        DEBUG > 1 && console.log("SpiceMsgCursorReset");
        this.cursor_note("RESET", null);
        this.drop_shapes();
        this.log_info("cursor: RESET (shape cache cleared)");
        document.getElementById(this.parent.screen_id).style.cursor = "auto";
        return true;
    }

    if (msg.type == Constants.SPICE_MSG_CURSOR_INVAL_ONE)
    {
        /* virtweb delta: drop one cached shape (u64 unique payload). */
        this.drop_shapes(new DataView(msg.data).getBigUint64(0, true));
        return true;
    }

    if (msg.type == Constants.SPICE_MSG_CURSOR_INVAL_ALL)
    {
        this.drop_shapes();
        return true;
    }

    return false;
}

/* One warning per shape type per connection. Windows sends a cursor on
   nearly every hover, so an unconvertible type used to produce an
   unbounded stream of identical warnings. */
SpiceCursorConn.prototype.warn_once_per_cursor_type = function(type)
{
    if (! this.warned_cursor_types)
        this.warned_cursor_types = {};
    if (this.warned_cursor_types[type])
        return;
    this.warned_cursor_types[type] = true;
    this.log_warn("No support for cursor type " + type + "; leaving the cursor as it was.");
}

/* SPICE cursor shapes to the RGBA the PNG writer expects.
   Returns null for a type we cannot convert.
   Layouts follow spice-gtk's set_cursor() (src/channel-cursor.c). */
/* virtweb delta: record HOW the last shape was applied on the shared
   cursorDebug record, so the page can distinguish "applied as CSS
   cursor", "fell back to the simulated cursor image", and "the
   browser rejected the image". */
SpiceCursorConn.prototype.cursor_applied = function(how)
{
    if (this.parent && this.parent.cursorDebug && this.parent.cursorDebug.last)
        this.parent.cursorDebug.last.applied = how;
};

function cursor_to_rgba(header, data)
{
    var width = header.width;
    var height = header.height;
    var pixels = width * height;
    var u8 = new Uint8Array(data);
    var rgba = new Uint8Array(pixels * 4);
    var i, at;

    /* SPICE ships 32bpp shapes as native-endian ARGB, which on every
       platform we run on is B,G,R,A in memory; PNG wants R,G,B,A. The
       swap is invisible on the black/white/transparent cursors that
       dominate, which is why it went unnoticed, but colour cursors came
       out with red and blue exchanged. */
    if (header.type == Constants.SPICE_CURSOR_TYPE_ALPHA)
    {
        if (u8.length < pixels * 4)
            return null;
        for (i = 0, at = 0; i < pixels; i++, at += 4)
        {
            rgba[at]     = u8[at + 2];
            rgba[at + 1] = u8[at + 1];
            rgba[at + 2] = u8[at];
            rgba[at + 3] = u8[at + 3];
        }
        return rgba;
    }

    /* Two 1bpp masks, AND then XOR, each row padded to a byte. The four
       combinations are the classic ones:
         AND=1 XOR=0  transparent
         AND=1 XOR=1  invert the screen
         AND=0 XOR=0  black
         AND=0 XOR=1  white
       A CSS cursor cannot invert what is under it, so those pixels are
       drawn black -- the same choice every other HTML SPICE/VNC client
       makes. It is what Windows' I-beam and resize cursors use, and
       black-on-transparent reads correctly against the light backgrounds
       those appear over. */
    if (header.type == Constants.SPICE_CURSOR_TYPE_MONO)
    {
        var bpl = (width + 7) >> 3;
        if (u8.length < bpl * height * 2)
            return null;
        for (var y = 0; y < height; y++)
        {
            for (var x = 0; x < width; x++)
            {
                var byte_at = y * bpl + (x >> 3);
                var bit = 0x80 >> (x & 7);
                var and_bit = (u8[byte_at] & bit) != 0;
                var xor_bit = (u8[bpl * height + byte_at] & bit) != 0;

                at = (y * width + x) * 4;
                if (and_bit && ! xor_bit)
                    continue;               /* transparent; rgba is zeroed */

                var lum = xor_bit && ! and_bit ? 255 : 0;
                rgba[at] = rgba[at + 1] = rgba[at + 2] = lum;
                rgba[at + 3] = 255;
            }
        }
        return rgba;
    }

    /* 32bpp BGRA followed by a 1bpp AND mask: mask set means the pixel
       is transparent. spice-gtk renders the "white pixel that is also
       masked" case as a dim checkerboard rather than a hole, because
       that combination is how shapes encode invert; do the same. */
    if (header.type == Constants.SPICE_CURSOR_TYPE_COLOR32)
    {
        var mask_at = pixels * 4;
        if (u8.length < mask_at + ((width + 7) >> 3) * height)
            return null;
        for (i = 0, at = 0; i < pixels; i++, at += 4)
        {
            var masked = (u8[mask_at + (i >> 3)] & (0x80 >> (i & 7))) != 0;
            var white = u8[at] == 0xff && u8[at + 1] == 0xff && u8[at + 2] == 0xff;
            if (masked && white)
            {
                var dark = (((i % width) ^ ((i / width) | 0)) & 1) != 0;
                rgba[at] = rgba[at + 1] = rgba[at + 2] = dark ? 0x30 : 0x50;
                rgba[at + 3] = dark ? 0xc0 : 0x30;
                continue;
            }
            rgba[at]     = u8[at + 2];
            rgba[at + 1] = u8[at + 1];
            rgba[at + 2] = u8[at];
            rgba[at + 3] = masked ? 0 : 255;
        }
        return rgba;
    }

    return null;
}

/* True if the shape was converted and applied. */
SpiceCursorConn.prototype.set_cursor = function(cursor)
{
    var rgba = cursor_to_rgba(cursor.header, cursor.data);
    if (! rgba)
    {
        this.cursor_applied("not converted (unsupported shape type " +
                            cursor.header.type + ")");
        return false;
    }
    this.cache_shape(cursor);

    var pngstr = create_rgba_png(cursor.header.width, cursor.header.height, rgba);
    var curstr = 'url(data:image/png,' + pngstr + ') ' +
        cursor.header.hot_spot_x + ' ' + cursor.header.hot_spot_y + ", default";
    var screen = document.getElementById(this.parent.screen_id);
    screen.style.cursor = 'auto';
    screen.style.cursor = curstr;
    /* Chromium and Firefox drop a cursor image over 128 px on a side
       without saying so: the computed style still reads as the url,
       and the pointer is the plain arrow, with the arrow's hot spot,
       so it points somewhere other than the guest's cursor does.  A
       shape that size is drawn under the pointer instead. */
    var too_large = cursor.header.width > MAX_CSS_CURSOR || cursor.header.height > MAX_CSS_CURSOR;
    if (too_large || window.getComputedStyle(screen, null).cursor == 'auto')
    {
        SpiceSimulateCursor.simulate_cursor(this, cursor, screen, pngstr, too_large);
        this.cursor_applied(too_large ?
            "simulated (shape exceeds " + MAX_CSS_CURSOR + "px)" :
            "simulated (browser rejected the CSS cursor image)");
    }
    else
    {
        /* This cursor took effect natively, so drop the simulated one
           left by an earlier browser-rejected cursor; the mousemove
           handler would otherwise keep painting it alongside the real
           cursor forever. */
        this.remove_simulated_cursor();
        this.cursor_applied("css");
    }
    return true;
}

/* A hidden pointer is hidden whichever way the last shape was shown:
   the CSS cursor goes to none, and a simulated cursor image is removed,
   or the mousemove handler would keep painting it. */
SpiceCursorConn.prototype.hide_cursor = function()
{
    document.getElementById(this.parent.screen_id).style.cursor = "none";
    this.remove_simulated_cursor();
}

SpiceCursorConn.prototype.remove_simulated_cursor = function()
{
    if (! this.spice_simulated_cursor)
        return;
    if (this.spice_simulated_cursor.parentNode)
        this.spice_simulated_cursor.parentNode.removeChild(this.spice_simulated_cursor);
    delete this.spice_simulated_cursor;
}

SpiceCursorConn.prototype.cleanup = function()
{
    /* The screen div outlives the connection: a simulated cursor left in
       it would sit frozen over the next session, and a guest that ended
       with its pointer hidden (or a custom shape) would leave the next
       one without a visible pointer until it sends a shape of its own. */
    this.remove_simulated_cursor();
    var screen = document.getElementById(this.parent.screen_id);
    if (screen)
        screen.style.cursor = "auto";
    SpiceConn.prototype.cleanup.call(this);
}

export {
  SpiceCursorConn,
};
