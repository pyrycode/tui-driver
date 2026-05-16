# codebase/

One file per ticket: `<N>.md` where `<N>` is the GitHub issue number.

The directory listing IS the index — no separate index file. To find what a ticket touched, `ls codebase/` and open the matching file.

## What goes in a ticket file

- **Summary** — what was built/changed in one paragraph.
- **Patterns established** — reusable approaches that future tickets should know about (concurrency shapes, regex conventions, file layouts, naming). Skip if nothing new.
- **Lessons learned** — surprises, dead ends, mistakes, things that didn't work. This is where `docs/lessons.md` content lives now (that file froze 2026-05-11).
- **Links** — to spec, code paths, ADRs, feature docs, GitHub issue/PR.

## What does NOT go here

- Step-by-step process narration ("first I ran X, then Y") — not useful to future readers.
- Stuff that belongs in evergreen docs (`features/`, `architecture/`, `decisions/`) — link to those instead.
- Duplicates of the ticket spec — link to `docs/specs/architecture/<N>-*.md`.

## Why per-ticket files

Shared-append docs (one big `lessons.md`, one growing `PROJECT-MEMORY.md`) guarantee merge conflicts when two feature branches add to them on top of a marching-forward main. Per-ticket files eliminate the hot line. Frozen 2026-05-11 in the sibling agent-dispatcher-v2 project; same fix is the default here from day one.
