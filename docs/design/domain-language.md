# Conch Domain Language

Conch creates and manages isolated sandboxes from reusable templates. These terms distinguish reusable launch state from the artifacts and actions that produce it.

## Language

**Template**:
A reusable description of the state required to create a Sandbox. A Template may support either cold boot or resume boot.
_Avoid_: BootSource, source, checkpoint object

**Boot Mode**:
The way a Template starts a Sandbox: `cold` starts without saved memory state, while `resume` restores saved memory state.
_Avoid_: Template kind, checkpoint kind

**Template Origin**:
The process that produced a Template: `image` when built from an Image, or `checkpoint` when captured from a Sandbox.
_Avoid_: Template type, Template kind

**Checkpoint**:
An action that captures a Sandbox as a resume-capable Template. It is not an independently managed resource.
_Avoid_: Checkpoint object, checkpoint record

**Image**:
An OCI or Conch boot artifact used to build a Template. It is not itself the reusable Sandbox launch object.
_Avoid_: Cold Template

**Sandbox**:
An isolated runtime instance created from a Template.
_Avoid_: Template instance
