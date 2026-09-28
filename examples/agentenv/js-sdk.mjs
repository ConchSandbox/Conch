// JavaScript/TypeScript SDK; see docs/user/agentenv.md for environment variables.
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { Sandbox } from 'e2b';

const template = process.env.E2B_TEMPLATE_ID;
assert(template, 'E2B_TEMPLATE_ID must name the preloaded Conch template');
const run = randomUUID();
const boxes = [];
const cleanupErrors = [];
try {
  for (let index = 0; index < 2; index++) {
    const box = await Sandbox.create(template, {
      secure: false,
      timeoutMs: 300_000,
      requestTimeoutMs: 120_000,
      metadata: { example: 'conch-agentenv', run },
      envs: { EXAMPLE_NODE_INDEX: String(index) },
    });
    boxes.push(box);
    console.log('created', box.sandboxId);
  }

  const paginator = Sandbox.list({ query: { metadata: { run } }, limit: 1 });
  const listed = [];
  while (paginator.hasNext) {
    listed.push(...(await paginator.nextItems()).map(item => item.sandboxId));
  }
  assert.deepEqual(new Set(listed), new Set(boxes.map(box => box.sandboxId)));

  for (const [index, box] of boxes.entries()) {
    assert.equal((await box.getInfo()).sandboxId, box.sandboxId);
    const result = await box.commands.run('printf "%s" "$EXAMPLE_NODE_INDEX"');
    assert.equal(result.exitCode, 0);
    assert.equal(result.stdout, String(index));
    await box.files.write('/home/user/example.txt', 'Conch + AgentENV\n');
    assert.equal(await box.files.read('/home/user/example.txt'), 'Conch + AgentENV\n');
    console.log('commands/files/get/list passed', box.sandboxId);
  }
} finally {
  for (const box of boxes) {
    try {
      assert.equal(await box.kill(), true, 'sandbox was already absent before kill');
      console.log('killed', box.sandboxId);
    } catch (error) {
      cleanupErrors.push(error);
      console.error('cleanup failed', box.sandboxId, error);
    }
  }
}
if (cleanupErrors.length) throw new AggregateError(cleanupErrors, 'sandbox cleanup failed');
