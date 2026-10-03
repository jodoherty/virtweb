// Custom Playwright reporter: on test failure in attach mode, print the
// "you are inside the test environment" cheat-sheet (the env persists,
// so the failed state is still live and inspectable).
import { read } from './env/manifest.js';

export default class CheatSheet {
  onTestEnd(test, result) {
    if (result.status === 'passed') return;
    if (process.env.E2E_ENV === 'own') return; // env is ephemeral there
    const m = read();
    if (!m) return;
    const lines = [
      '',
      '── debug: the test environment is still running (attach mode) ──',
      `  virtweb url : ${m.virtweb.url}`,
      `  server id   : ${m.serverId ?? '(none)'}`,
      `  backend     : ${m.backend}`,
      '  logs        : npm run env:logs',
      '  manifest    : .artifacts/env.json   (npm run env:status)',
      '  reset state : npm run env:reset',
    ];
    if (m.remote) {
      lines.push(`  remote ssh  : ${m.remote.ssh}`);
      lines.push(`  podman exec : ${m.remote.exec}`);
      for (const [vm, info] of Object.entries(m.vms || {})) {
        lines.push(`  raw serial  : ${info.serial}`);
      }
    }
    console.error(lines.join('\n'));
  }
}
