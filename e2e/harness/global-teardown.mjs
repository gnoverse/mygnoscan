import { readFileSync, rmSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const tmpDir = join(dirname(fileURLToPath(import.meta.url)), '..', '.tmp');

export default async function globalTeardown() {
  let state;
  try {
    state = JSON.parse(readFileSync(join(tmpDir, 'state.json'), 'utf8'));
  } catch {
    return; // Setup never got far enough to start anything.
  }

  try {
    process.kill(state.pid, 'SIGTERM');
  } catch {
    // Already gone, which is the outcome we wanted.
  }

  rmSync(join(tmpDir, 'state.json'), { force: true });
}
