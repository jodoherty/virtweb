import { test, expect } from '@playwright/test';
import { requireEnv } from '../src/envinfo.js';
import { Api } from '../src/api.js';
import { waitFor } from '../src/wait.js';
import { openConsole, ConsoleSession, waitForNoSession } from '../src/console.js';

let m: ReturnType<typeof requireEnv>;
let api: Api;
let serverId: string;
const url = () => m.virtweb.url;

test.beforeAll(async () => {
  m = requireEnv();
  api = new Api(m.virtweb.url);
  serverId = m.serverId;
});

// Clear any console session leaked by a previous test/page in the shared
// persistent env (the server drops a session asynchronously on disconnect).
test.beforeEach(async () => {
  await waitForNoSession(url(), serverId, 'debian-dev');
  await waitForNoSession(url(), serverId, 'win10');
});

// read the xterm buffer as an array of non-empty lines
async function termLines(page: import('@playwright/test').Page) {
  return page.evaluate(() => {
    const b = (window as any).__term?.buffer?.active;
    if (!b) return [];
    const out: string[] = [];
    for (let i = 0; i < b.length; i++) {
      const l = b.getLine(i);
      // trim both ends: xterm wraps long lines, so wrapped continuation
      // lines carry leading whitespace.
      const s = l ? l.translateToString(true).trim() : '';
      if (s) out.push(s);
    }
    return out;
  });
}

test('dashboard: servers listed, add-server form present', async ({ page }) => {
  await page.goto(`${url()}/`);
  await expect(page.locator('h2', { hasText: 'Servers' })).toBeVisible();
  await expect(page.locator('.server-table', { hasText: 'e2e' })).toBeVisible();
  await expect(page.locator('.add-server')).toBeVisible();
});

test('add server via form lands on the server page', async ({ page }) => {
  await page.goto(`${url()}/`);
  const name = `ui-${Date.now()}`;
  await page.locator('.add-server input[name=name]').fill(name);
  await page.locator('.add-server input[name=user]').fill('u');
  await page.locator('.add-server input[name=host]').fill('example.invalid');
  await page.locator('.add-server button[type=submit]').click();
  await expect(page).toHaveURL(new RegExp(`/servers/[^/]+$`));
  await expect(page.locator('h2', { hasText: 'Virtual machines' })).toBeVisible();
  await expect(page.locator('strong', { hasText: name })).toBeVisible();
  const servers = await api.listServers();
  await api.deleteServer(servers.find((s) => s.name === name)?.id ?? '');
});

const e2eKey = "-----BEGIN OPENSSH PRIVATE KEY-----\npublished-fake-e2e-key\n-----END OPENSSH PRIVATE KEY-----\n";

test('server credentials: key is stored server-side, masked in the API, replaceable from the edit form', async ({ page }) => {
  // Create a server through the API with a key; the responses must
  // carry keySet but never the material.
  const name = `cred-${Date.now()}`;
  const created = await api.addServer({ name, user: 'u', host: 'example.invalid', key: e2eKey });
  try {
    expect((created as Record<string, unknown>).keySet).toBe(true);
    expect((created as Record<string, unknown>).key).toBeUndefined();
    const listed = await api.listServers();
    const view = listed.find((s) => s.id === created.id) as Record<string, unknown>;
    expect(view.keySet).toBe(true);
    expect(view.key).toBeUndefined();
    const raw = await fetch(`${url()}/api/servers/${created.id}`).then((r) => r.text());
    expect(raw).not.toContain('published-fake-e2e-key');

    // The edit form offers a key textarea and a "remove stored key"
    // checkbox; the key material itself is not displayed.
    await page.goto(`${url()}/servers/${created.id}`);
    await page.locator('.edit-server summary').click();
    const keybox = page.locator('.edit-server textarea[name=key]');
    await expect(keybox).toBeVisible();
    expect(await keybox.inputValue()).not.toContain('published-fake-e2e-key');
    await expect(page.locator('.edit-server input[name=removeKey]')).toBeVisible();

    // Paste a replacement key and save; the stored key changes.
    const replacement = '-----BEGIN OPENSSH PRIVATE KEY-----\nreplaced\n-----END OPENSSH PRIVATE KEY-----\n';
    await keybox.fill(replacement);
    await page.locator('.edit-server button[type=submit]').click();
    await page.waitForLoadState('load');
    const st2 = await api.listServers().then((ls) => ls.find((s) => s.id === created.id));
    expect((st2 as Record<string, unknown>).keySet).toBe(true);

    // Tick "remove stored key" and save; keySet drops.
    await page.locator('.edit-server summary').click();
    await page.locator('.edit-server input[name=removeKey]').check();
    await page.locator('.edit-server button[type=submit]').click();
    await page.waitForLoadState('load');
    const st3 = await api.listServers().then((ls) => ls.find((s) => s.id === created.id));
    expect((st3 as Record<string, unknown>).keySet).toBe(false);
    // the checkbox disappears now that no key is stored
    await expect(page.locator('.edit-server input[name=removeKey]')).toHaveCount(0);
  } finally {
    await api.deleteServer(created.id);
  }
});

test('edit server form updates the definition in place', async ({ page }) => {
  await page.goto(`${url()}/servers/${serverId}`);
  // The edit panel is a collapsed <details>; open it.
  await page.locator('.edit-server summary').click();
  // Pre-populated with the current definition (env server "e2e").
  await expect(page.locator('.edit-server input[name=name]')).toHaveValue('e2e');
  await expect(page.locator('.edit-server input[name=host]')).toHaveValue('stub-host');

  // Change only the port (the name must stay "e2e" — the env CLI
  // re-registers the server by that name), verify it sticks, restore.
  const before = (await api.listServers()).find((s) => s.id === serverId)!;
  await page.locator('.edit-server input[name=port]').fill('2222');
  await page.locator('.edit-server button[type=submit]').click();
  await expect(page).toHaveURL(new RegExp(`/servers/${serverId}`));
  await expect(page.locator('span.muted', { hasText: 'stub-host' })).toContainText(
    'stub-host:2222',
  );
  expect((await api.listServers()).find((s) => s.id === serverId)?.port).toBe(2222);

  // Restore the original definition.
  await page.locator('.edit-server summary').click();
  await page
    .locator('.edit-server input[name=port]')
    .fill(before.port ? String(before.port) : '');
  await page.locator('.edit-server button[type=submit]').click();
  await expect(page).toHaveURL(new RegExp(`/servers/${serverId}`));
});

test('VM detail page shows live info and the domain XML', async ({ page }) => {
  // Arrive via the name link on the server page.
  await page.goto(`${url()}/servers/${serverId}`);
  await page.locator('.vm-table a', { hasText: 'debian-dev' }).click();
  await expect(page.locator('h2')).toHaveText('debian-dev');

  // The info table shows at least state/vCPU/memory rows.
  for (const label of ['State', 'vCPU', 'Memory']) {
    await expect(page.locator('.vm-info th', { hasText: label })).toBeVisible();
  }

  // The domain XML is present (HTML-escaped in the <pre>), and the
  // Copy button exists for it.
  await expect(page.locator('#vm-xml')).toContainText('<name>debian-dev</name>');
  await expect(page.locator('#copy-xml')).toBeVisible();

  // Action forms + display links are offered from the detail page, and
  // the interrupting actions (shutdown/destroy/reboot) are guarded by
  // the confirm dialog (data-confirm), while start is not.
  await expect(page.locator('form[action$="/start"]')).toBeVisible();
  for (const a of ['shutdown', 'destroy', 'reboot']) {
    await expect(page.locator(`form[action$="/${a}"]`)).toHaveAttribute(
      'data-confirm', /.+/);
  }
  await expect(page.locator('form[action$="/start"]')).not.toHaveAttribute('data-confirm');
  await expect(page.locator('a[href$="/console"]')).toBeVisible();

  // The Refresh link re-renders the same page.
  await page.locator('a.btn[href$="/vms/debian-dev"]').click();
  await expect(page.locator('h2')).toHaveText('debian-dev');
});

test('VM detail page shows the console as in use', async ({ page }, testInfo) => {
  // The "Console" row polls the console-status endpoint on load:
  // "open" while a session is live (here, an API-level one from
  // another "tab"), "–" once it is gone.
  const s = await openConsole({
    baseUrl: url(),
    serverId,
    vm: 'debian-dev',
    logPath: testInfo.outputPath('console.log'),
  });
  try {
    await page.goto(`${url()}/servers/${serverId}/vms/debian-dev`);
    await expect(page.locator('#vm-console-status')).toContainText('open', {
      timeout: 10_000,
    });
  } finally {
    await s.close();
    await waitForNoSession(url(), serverId, 'debian-dev');
    await page.goto(`${url()}/servers/${serverId}/vms/debian-dev`);
    await expect(page.locator('#vm-console-status')).toContainText('–', {
      timeout: 10_000,
    });
  }
});

test('interrupting actions require a confirm dialog; rejecting leaves the VM running', async ({ page }) => {
  // Start debian-dev, then try to shut it down from the server page.
  await api.action(serverId, 'debian-dev', 'destroy').catch(() => {});
  await api.action(serverId, 'debian-dev', 'start');
  // Actions live on the VM detail page, not the listing.
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev`);
  // wait for the stub to finish booting (the page re-renders on reload)
  await expect
    .poll(async () => {
      await page.reload();
      return page.locator('body').innerText();
    }, { message: 'debian-dev running', timeout: 10000 })
    .toContain('running');

  let confirmShown = false;
  let reject = true;
  page.on('dialog', async (d) => {
    confirmShown = true;
    expect(d.message()).toContain('debian-dev');
    if (reject) await d.dismiss(); // reject: the VM must keep running
    else await d.accept();
  });

  await page.locator('form[action$="/shutdown"] button').click();
  await expect
    .poll(() => confirmShown, { message: 'confirm dialog shown' })
    .toBeTruthy();

  // The action was cancelled: the PRG redirect re-renders the page and
  // the VM still reports running.
  await page.waitForLoadState('load');
  await expect
    .poll(async () => page.locator('body').innerText(), { message: 'still running after reject' })
    .toContain('running');
  expect((await api.listVMs(serverId)).find((v) => v.name === 'debian-dev')?.state)
    .toBe('running');

  // Accepting the dialog does the action.
  reject = false;
  await page.locator('form[action$="/shutdown"] button').click();
  await expect
    .poll(async () => (await api.listVMs(serverId)).find((v) => v.name === 'debian-dev')?.state,
      { message: 'shut off after accept' })
    .toBe('shut off');
});

test('VM list shows the default table', async ({ page }) => {
  await page.goto(`${url()}/servers/${serverId}`);
  await expect(page.locator('h2', { hasText: 'Virtual machines' })).toBeVisible();
  await expect(page.locator('.vm-table', { hasText: 'debian-dev' })).toBeVisible();
  await expect(page.locator('.vm-table', { hasText: 'win10' })).toBeVisible();
});

test('Start on the detail page takes the VM to running', async ({ page }) => {
  await api.action(serverId, 'debian-dev', 'destroy').catch(() => {});
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev`);
  await page.getByRole('button', { name: 'Start' }).click();
  // The detail page is server-rendered with no auto-refresh: the PRG
  // redirect reloads the page once (showing "starting"). Poll by
  // reloading until the stub has finished booting.
  await expect
    .poll(async () => {
      await page.reload();
      return page.locator('body').innerText();
    }, { message: 'debian-dev running', timeout: 10000 })
    .toContain('running');
});

test('the VM listing has no actions, only state and display links', async ({ page }) => {
  await page.goto(`${url()}/servers/${serverId}`);
  const row = page.locator('.vm-table tr', { hasText: 'debian-dev' });
  await expect(row.locator('form')).toHaveCount(0);
  await expect(row.locator('button')).toHaveCount(0);
  // state + details link + display links remain (state value
  // depends on other specs; any known libvirt state is fine)
  await expect(row).toHaveText(/(running|shut off|starting)/);
  await expect(row.locator('a[href$="/vms/debian-dev"]')).toBeVisible();
  await expect(row.locator('a[href$="/console"]')).toBeVisible();
});

test('console page: type a command, read the reply', async ({ page }) => {
  await api.action(serverId, 'debian-dev', 'start').catch(() => {});
  await waitFor(async () => {
    const vms = await api.listVMs(serverId);
    return vms.find((v) => v.name === 'debian-dev')?.state === 'running';
  });
  await page.goto(`${url()}/servers/${serverId}/vms/debian-dev/console`);
  await expect(page.locator('#console-status')).toHaveText('Connected.');
  await page.locator('#term').click();
  await page.keyboard.type('hostname');
  await page.keyboard.press('Enter');
  await expect
    .poll(async () => termLines(page).then((l) => l.some((x) => x === 'debian-dev')), {
      message: 'hostname reply in terminal',
    })
    .toBe(true);
});

test('console page for a shut-off VM shows the start hint', async ({ page }) => {
  await api.action(serverId, 'win10', 'destroy').catch(() => {});
  await page.goto(`${url()}/servers/${serverId}/vms/win10/console`);
  await expect(page.locator('.banner.warn', { hasText: /Start it/ })).toBeVisible();
  await expect(page.locator('#term')).toHaveCount(0);
});

test('VNC page loads noVNC and attempts a tunnel connection', async ({ page }) => {
  // noVNC is wired over the plain-HTTP tunnel endpoints. The stub
  // tunnel is not a real RFB server, so the handshake fails — the point
  // here is that the page attempts a real connection.
  // (transport.spec.ts covers the transport itself.)
  await page.goto(`${url()}/servers/${serverId}/vms/win10/vnc`);
  await expect(page.locator('#vnc')).toHaveCount(1);
  await expect(page.locator('#vnc-status')).toContainText(
    /Connecting|Connected|Connection lost|Disconnected/,
  );
});
