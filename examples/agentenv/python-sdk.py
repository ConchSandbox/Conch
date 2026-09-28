"""Run against the two-node deployment described in docs/user/agentenv.md.

Install ``e2b==2.46.4`` and ``e2b-code-interpreter==2.5.0`` before running.
"""

import os
import uuid

from e2b_code_interpreter import Sandbox, SandboxQuery


template = os.environ["E2B_TEMPLATE_ID"]
run_id = str(uuid.uuid4())
boxes = []
cleanup_errors = []
try:
    for index in range(2):
        box = Sandbox.create(
            template,
            secure=False,
            timeout=300,
            request_timeout=120,
            metadata={"example": "conch-agentenv", "run": run_id},
            envs={"EXAMPLE_NODE_INDEX": str(index)},
        )
        boxes.append(box)
        print("created", box.sandbox_id, flush=True)

    paginator = Sandbox.list(query=SandboxQuery(metadata={"run": run_id}), limit=1)
    listed = []
    while paginator.has_next:
        listed.extend(item.sandbox_id for item in paginator.next_items())
    assert set(listed) == {box.sandbox_id for box in boxes}, listed

    for index, box in enumerate(boxes):
        assert box.get_info().sandbox_id == box.sandbox_id
        result = box.commands.run('printf "%s" "$EXAMPLE_NODE_INDEX"')
        assert result.exit_code == 0 and result.stdout == str(index), result
        box.files.write("/home/user/example.txt", "Conch + AgentENV\n")
        assert box.files.read("/home/user/example.txt") == "Conch + AgentENV\n"
        box.run_code("x = 1")
        execution = box.run_code("x += 1; x")
        assert execution.text == "2", execution
        print("commands/files/code-interpreter/get/list passed", box.sandbox_id, flush=True)
finally:
    for box in boxes:
        try:
            assert box.kill(), "sandbox was already absent before kill"
            print("killed", box.sandbox_id, flush=True)
        except Exception as error:
            cleanup_errors.append(error)
            print("cleanup failed", box.sandbox_id, error, flush=True)
if cleanup_errors:
    raise RuntimeError("sandbox cleanup failed") from cleanup_errors[0]
