## What this changes

<!-- What the change does, and what it was like before if that is not obvious
from the subject. -->

## Why

<!-- The spec section behind it, if there is one. A change that contradicts a
decision in `spec/` wants that file updated in the same pull request, in its
own commit ahead of the code. -->

## Checks

- [ ] `just check` passes locally
- [ ] Integration tests run if the change touches query execution, sources or
      the scheduler (`just integration-clean`)
- [ ] `just generate` run if a flag or a check was added, renamed or removed
- [ ] The README `Status` section still describes what the tree does
