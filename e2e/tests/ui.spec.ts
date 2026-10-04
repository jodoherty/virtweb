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

  // Action forms + display links are offered from the detail page.
  await expect(page.locator('form[action$="/start"]')).toBeVisible();
  await expect(page.locator('form[action$="/undefine"]')).toBeVisible();
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

test('define a VM from XML, start it, destroy it', async ({ page }) => {
  const name = `def-${Date.now()}`;
  const xml = `<domain type='kvm'>
  <name>${name}</name>
  <memory unit='MiB'>2048</memory>
  <vcpu>2</vcpu>
</domain>`;
  await page.goto(`${url()}/servers/${serverId}`);
  // The define panel is a collapsed <details>; open it.
  await page.locator('.define-vm summary').click();
  await page.locator('#define-xml').fill(xml);
  await page.locator('#define-btn').click();
  await expect(page.locator('.banner.ok')).toContainText(`defined ${name}`);
  await expect(
    page.locator('.vm-table tr', { hasText: name }),
  ).toContainText('shut off');

  // Start it from its row. The table is server-rendered with no
  // auto-refresh: the PRG redirect reloads once ("starting"), so poll
  // by reloading until the stub has finished booting.
  const row = page.locator('.vm-table tr', { hasText: name });
  await row.locator('form[action$="/start"] button').click();
  await expect(page.locator('.banner.ok')).toContainText(`start ${name}`);
  await expect
    .poll(async () => {
      await page.reload();
      return row.innerText();
    }, { message: `${name} running`, timeout: 10000 })
    .toContain('running');

  // Clean up (keep the stub env tidy for the other specs): power off,
  // then remove the definition (destroy alone leaves it defined).
  await api.action(serverId, name, 'destroy');
  await api.deleteVM(serverId, name);
  expect((await api.listVMs(serverId)).some((v) => v.name === name)).toBe(false);
});

test('VM list shows the default table', async ({ page }) => {
  await page.goto(`${url()}/servers/${serverId}`);
  await expect(page.locator('h2', { hasText: 'Virtual machines' })).toBeVisible();
  await expect(page.locator('.vm-table', { hasText: 'debian-dev' })).toBeVisible();
  await expect(page.locator('.vm-table', { hasText: 'win10' })).toBeVisible();
});

test('Start button takes the row to running', async ({ page }) => {
  await api.action(serverId, 'debian-dev', 'destroy').catch(() => {});
  await page.goto(`${url()}/servers/${serverId}`);
  const row = page.locator('tr', { hasText: 'debian-dev' });
  await row.getByRole('button', { name: 'Start' }).click();
  // The table is server-rendered with no auto-refresh: the PRG redirect
  // reloads the page once (showing "starting"). Poll by reloading until the
  // stub has finished booting and the row reports running.
  await expect
    .poll(async () => {
      await page.reload();
      return row.innerText();
    }, { message: 'debian-dev running', timeout: 10000 })
    .toContain('running');
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
