"""Create Node-local checkpoints through AgentENV using e2b==2.46.4.

Set E2B_API_KEY, E2B_API_URL, E2B_SANDBOX_URL and E2B_TEMPLATE_ID.
The sandbox is deleted after verification; the printed templates are retained
on its Node and can be inspected/removed with the native Conch template CLI.
"""

import json
import os
import uuid

from e2b import Sandbox


box = Sandbox.create(
    os.environ["E2B_TEMPLATE_ID"], secure=False, timeout=600, request_timeout=120
)
try:
    print(json.dumps({"sandboxID": box.sandbox_id}), flush=True)
    name = f"checkpoint-example-{uuid.uuid4()}:v1"
    before = box.get_info()
    for generation in (1, 2):
        box.files.write("/home/user/checkpoint.txt", str(generation))
        snapshot = box.create_snapshot(name=name, request_timeout=120)
        assert snapshot.snapshot_id == name and snapshot.names == [name], snapshot
        assert box.files.read("/home/user/checkpoint.txt") == str(generation)
        print(json.dumps({"generation": generation, "snapshotID": snapshot.snapshot_id,
                          "names": snapshot.names}), flush=True)

    anonymous = box.create_snapshot(request_timeout=120)
    assert anonymous.snapshot_id.startswith("checkpoint-") and anonymous.names == [], anonymous
    print(json.dumps({"snapshotID": anonymous.snapshot_id, "names": anonymous.names}), flush=True)
    after = box.get_info()
    assert after.sandbox_id == before.sandbox_id and after.end_at == before.end_at
    assert box.commands.run("printf checkpoint-ok").stdout == "checkpoint-ok"
finally:
    box.kill()
