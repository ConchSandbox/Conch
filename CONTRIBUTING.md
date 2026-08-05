# Contributing to Conch

## Commit Messages

Every commit message must follow these rules:

1. Use a concise subject in one of these forms: `<type>: <summary>` or
   `<type>(<scope>): <summary>`. Choose a type and optional scope that
   describe the change.
2. Keep the subject and every body or trailer line at 72 characters or
   fewer.
3. Leave a blank line between the subject and the body.
4. Explain both the background of the change and how the commit solves
   the problem. Do not only list modified files or implementation steps.
5. Leave a blank line between the body and the trailers.
6. For each assistant used, add one trailer in the exact form
   `Assisted-by: <assistant>:<model>`.
   Omit this trailer when no assistant was used.
7. End with exactly one `Signed-off-by` trailer using the committer's
   Git identity. Place any `Assisted-by` trailers before it.

Use `git commit --signoff` to add the `Signed-off-by` trailer
automatically. Refer to the repository history for the expected commit
message style.

Before committing, review the complete message and amend it if any line
is longer than 72 characters, or if the background or solution is
unclear.
