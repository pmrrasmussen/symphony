# How we work

## Delivering Linear issues

1. Move the Linear issue to **In Progress** before implementation.
2. Work in an isolated Git worktree; do not make issue changes in the primary
   checkout.
3. Implement a focused solution and run the narrowest relevant validation,
   followed by broader checks when shared behavior changes.
4. Open a pull request that uses the **Why**, **What changed**, and **On Call**
   sections.
5. Merge only after required checks pass and any required review is complete.
6. Move the Linear issue to **Done** only after its pull request is merged.

Process dependent issues serially. Work on unrelated issues in parallel when
capacity permits, while keeping their changes and worktrees isolated.
