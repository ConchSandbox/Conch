// npm install e2b@2.46.1
// Provide E2B_API_KEY, E2B_API_URL, E2B_SANDBOX_URL and E2B_TEMPLATE_ID.
import { Sandbox } from 'e2b';

for (const name of ['E2B_API_KEY', 'E2B_API_URL', 'E2B_SANDBOX_URL', 'E2B_TEMPLATE_ID']) {
  if (!process.env[name]) throw new Error(`Set ${name} before running this test`);
}

function check(condition, message) {
  if (!condition) throw new Error(message);
}

let box;
try {
  box = await Sandbox.create(process.env.E2B_TEMPLATE_ID, {
    secure: false, timeoutMs: 300000, requestTimeoutMs: 120000,
    metadata: { suite: 'conch-agentenv-js-e2e' }, envs: { CONCH_E2E_VALUE: 'from-js' },
  });
  check((await box.getInfo()).sandboxId === box.sandboxId, 'getInfo ID');
  const result = await box.commands.run('printf "%s" "$CONCH_E2E_VALUE"');
  check(result.stdout === 'from-js' && result.exitCode === 0, 'command environment/output');
  const value = 'TypeScript 文件 roundtrip\n'.repeat(512);
  await box.files.write('/tmp/ts e2e %23.txt', value);
  check(await box.files.read('/tmp/ts e2e %23.txt') === value, 'file content');
  const paginator = Sandbox.list({ query: { metadata: { suite: 'conch-agentenv-js-e2e' } }, limit: 1 });
  const listed = [];
  while (paginator.hasNext) listed.push(...await paginator.nextItems());
  check(listed.some(sandbox => sandbox.sandboxId === box.sandboxId), 'list');
  let ptyData = '';
  const pty = await box.pty.create({ cols: 80, rows: 24, timeoutMs: 30000,
    onData: data => { ptyData += new TextDecoder().decode(data); } });
  await box.pty.resize(pty.pid, { cols: 100, rows: 35 });
  await box.pty.sendInput(pty.pid, new TextEncoder().encode("printf 'JS_PTY_OK\\n'; stty size; exit\n"));
  await pty.wait();
  check(ptyData.includes('JS_PTY_OK') && ptyData.includes('35 100'), `PTY: ${ptyData}`);
  check(await box.kill(), 'kill');
  console.log(JSON.stringify({ sdk: 'e2b-js-2.46.1', sandboxId: box.sandboxId,
    create: true, get: true, list: true, commands: true, files: true, pty: true, kill: true }));
  box = undefined;
} finally {
  if (box) await box.kill();
}
