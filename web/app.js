// virtweb frontend (vanilla JS, no build step).
//
// The UI is a small hash-based SPA:
//   #/                 server list + add/remove servers
//   #/server/<id>      VM list for a server + VM actions
//   #/console/<id>/<vm> serial console
//   #/vnc/<id>/<vm>    VNC viewer

'use strict';

const app = document.getElementById('app');

const routes = {
  servers: renderServers,
  server: renderServer,
  console: renderConsole,
  vnc: renderVNC,
};

function currentRoute() {
  const hash = location.hash.slice(1) || '/';
  const parts = hash.split('/').filter(Boolean);
  return { name: parts[0] || 'servers', args: parts.slice(1) };
}

function navigate() {
  const { name, args } = currentRoute();
  const render = routes[name] || renderServers;
  render(app, ...args).catch((err) => {
    app.textContent = 'Error: ' + err.message;
  });
}

document.addEventListener('hashchange', navigate);

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  if (!res.ok) {
    let msg = res.status;
    try {
      const body = await res.json();
      if (body.error) msg = body.error;
    } catch {}
    throw new Error(msg);
  }
  return res.status === 204 ? null : res.json();
}

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (v !== null && v !== undefined) el.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined) continue;
    el.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  return el;
}

/* ---------- routes ---------- */

async function renderServers(el) {
  const servers = await api('/api/servers');
  el.textContent = '';

  const list = h('ul', { class: 'server-list' });
  for (const s of servers) {
    const status = h('span', { class: 'status' }, '…');
    list.append(h('li', {},
      h('a', { href: `#/server/${s.id}`, class: 'server-link' },
        h('strong', {}, s.name),
        ` ${s.user}@${s.host}:${s.port || 22}`),
      h('button', { class: 'danger', onclick: () => removeServer(s, status) }, 'Remove'),
      status,
    ));
  }
  if (servers.length === 0) {
    list.append(h('li', { class: 'muted' }, 'No servers yet. Add one below.'));
  }

  const form = h('form', { class: 'add-server', onsubmit: (e) => addServer(e, list) },
    h('h2', {}, 'Add server'),
    h('div', { class: 'row' },
      h('input', { name: 'name', placeholder: 'Name', required: 'required' }),
      h('input', { name: 'user', placeholder: 'user', required: 'required' }),
      h('input', { name: 'host', placeholder: 'hostname', required: 'required' }),
      h('input', { name: 'port', placeholder: 'port (22)', type: 'number', min: '1', max: '65535' }),
      h('input', { name: 'uri', placeholder: 'libvirt URI (qemu:///system)', value: 'qemu:///system' }),
    ),
    h('button', { type: 'submit' }, 'Add'),
  );

  el.append(h('h2', {}, 'Servers'), list, form);
}

async function addServer(e, list) {
  e.preventDefault();
  const fd = new FormData(e.target);
  const body = Object.fromEntries(fd.entries());
  if (!body.port) delete body.port;
  const s = await api('/api/servers', { method: 'POST', body: JSON.stringify(body) });
  e.target.reset();
  if (e.target.querySelector('[name=uri]'))
    e.target.querySelector('[name=uri]').value = 'qemu:///system';
  location.hash = `#/server/${s.id}`;
}

async function removeServer(s, status) {
  if (!confirm(`Remove server ${s.name}?`)) return;
  try {
    await api(`/api/servers/${s.id}`, { method: 'DELETE' });
    location.hash = '#/';
    navigate();
  } catch (err) {
    status.textContent = err.message;
  }
}

async function renderServer(el, id) {
  const server = await api(`/api/servers/${id}`);
  const vms = await api(`/api/servers/${id}/vms`);
  el.textContent = '';

  const table = h('table', { class: 'vm-table' });
  table.append(h('tr', {},
    h('th', {}, 'Name'), h('th', {}, 'State'), h('th', {}, 'vCPU'),
    h('th', {}, 'Memory'), h('th', {}, 'IP'), h('th', {}, '')));
  for (const vm of vms) {
    const row = h('tr', { class: vm.running ? 'running' : 'stopped' },
      h('td', {}, vm.name),
      h('td', {}, vm.state),
      h('td', {}, String(vm.vcpus ?? '–')),
      h('td', {}, vm.memory ? `${vm.memory} MiB` : '–'),
      h('td', {}, vm.ip || '–'),
      h('td', { class: 'actions' },
        h('button', { onclick: () => vmAction(server, vm, 'start') }, 'Start'),
        h('button', { onclick: () => vmAction(server, vm, 'shutdown') }, 'Shutdown'),
        h('button', { onclick: () => vmAction(server, vm, 'destroy'), class: 'danger' }, 'Destroy'),
        h('button', { onclick: () => vmAction(server, vm, 'reboot') }, 'Reboot'),
        h('a', { class: 'btn', href: `#/console/${server.id}/${vm.name}` }, 'Console'),
        h('a', { class: 'btn', href: `#/vnc/${server.id}/${vm.name}` }, 'VNC'),
      ),
    );
    table.append(row);
  }
  if (vms.length === 0) {
    table.append(h('tr', {}, h('td', { colspan: 6, class: 'muted' }, 'No virtual machines.')));
  }

  el.append(
    h('p', {}, h('a', { href: '#/' }, '←'), ' ', h('strong', {}, server.name)),
    h('h2', {}, 'Virtual machines'), table,
  );
}

async function vmAction(server, vm, action) {
  const btn = event.target;
  btn.disabled = true;
  try {
    await api(`/api/servers/${server.id}/vms/${encodeURIComponent(vm.name)}/${action}`,
      { method: 'POST' });
    location.reload();
  } catch (err) {
    alert(`${vm.name}: ${action} failed: ${err.message}`);
    btn.disabled = false;
  }
}

/* ---------- serial console (M4) ----------
 *
 * Output is a chunked GET response whose body is a ReadableStream
 * ("readable byte stream" per MDN); we consume it with the standard
 * ReadableStreamDefaultReader pattern (body.getReader() / read()).
 * Keystrokes are POSTed as raw bytes. No WebSocket.
 */

const consoleMaxChars = 20000; // scrollback cap

async function renderConsole(el, id, vmName) {
  el.textContent = '';
  const status = h('p', { class: 'muted' }, 'Connecting…');
  const term = h('pre', { class: 'console-term', tabindex: '0' });
  el.append(h('h2', {}, `Console — ${vmName}`), status, term);
  term.focus();

  const base = `/api/servers/${id}/vms/${encodeURIComponent(vmName)}/console`;

  let res;
  try {
    res = await fetch(base + '/stream');
  } catch (err) {
    status.textContent = 'Failed to open console: ' + err.message;
    return;
  }
  if (res.status === 409) {
    status.textContent = 'A console session for this VM is already open.';
    res.body.cancel().catch(() => {});
    return;
  }
  if (!res.ok || !res.body) {
    status.textContent = 'Failed to open console (HTTP ' + res.status + ').';
    if (res.body) res.body.cancel().catch(() => {});
    return;
  }

  // res.body is a ReadableStream of bytes; getReader() returns the
  // default reader we use to pull chunks (MDN: "Using readable byte
  // streams"). On the error paths we cancel so the body is released
  // instead of being drained in the background.
  const reader = res.body.getReader();
  status.textContent = 'Connected. Click the terminal to type.';

  term.addEventListener('keydown', (e) => {
    const bytes = keyToBytes(e);
    if (!bytes) return;
    e.preventDefault();
    fetch(base + '/input', {
      method: 'POST',
      headers: { 'Content-Type': 'application/octet-stream' },
      body: bytes,
    });
  });

  const decoder = new TextDecoder();
  try {
    for (;;) {
      const { value, done } = await reader.read(); // {value: Uint8Array, done}
      if (done) break;
      appendOutput(term, decoder.decode(value, { stream: true }));
    }
    appendOutput(term, decoder.decode()); // flush trailing bytes
    status.textContent = 'Session ended.';
  } catch (err) {
    status.textContent = 'Console stream error: ' + err.message;
    reader.cancel().catch(() => {}); // release the lock on failure
  }
}

function appendOutput(term, text) {
  if (!text) return;
  let next = term.textContent + text;
  if (next.length > consoleMaxChars) {
    next = next.slice(next.length - consoleMaxChars);
  }
  term.textContent = next;
  term.scrollTop = term.scrollHeight;
}

// Maps a keydown event to console input bytes (null = ignore).
function keyToBytes(e) {
  if (e.ctrlKey || e.metaKey) {
    if (e.altKey) return null; // let the browser handle browser shortcuts
    const k = e.key.toLowerCase();
    if (k.length === 1 && k >= 'a' && k <= 'z') {
      return [k.charCodeAt(0) - 96]; // ctrl+letter → 0x01..0x1a
    }
    return null;
  }
  switch (e.key) {
    case 'Enter': return [13];         // \r
    case 'Backspace': return [0x7f];
    case 'Tab': return [9];
    case 'Escape': return [27];
    case 'ArrowUp': return [27, 65, 66];   // ESC A
    case 'ArrowDown': return [27, 67, 66]; // ESC B
    case 'ArrowRight': return [27, 68, 66]; // ESC C
    case 'ArrowLeft': return [27, 69, 66];  // ESC D
    case 'Home': return [27, 72, 70];      // ESC H
    case 'End': return [27, 70, 70];       // ESC F
    case 'Delete': return [27, 51, 126];   // ESC [ 3 ~
    case 'PageUp': return [27, 55, 126];   // ESC [ 5 ~
    case 'PageDown': return [27, 56, 126]; // ESC [ 6 ~
    default:
      if (e.key.length === 1) {
        return [...new TextEncoder().encode(e.key)];
      }
      return null;
  }
}

/* ---------- VNC (implemented in M6) ---------- */

async function renderVNC(el, id, vmName) {
  el.textContent = '';
  el.append(h('h2', {}, `VNC — ${vmName}`),
    h('p', { class: 'muted' }, 'VNC viewer not yet available (milestone M6).'));
}

navigate();
