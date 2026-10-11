# Autonomous development

Looper uses one Go controller for sequential and parallel work. Start with
[README.md](README.md) for commands and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
for scheduling, verification, and recovery behavior.

Follow [MANIFESTO.md](MANIFESTO.md) when changing Looper: one purpose,
simple before easy, do not make me think, believe in the loop, and take
things away.

Keep the intended project outcome and exclusions in source requirement files.
Give tasks observable acceptance criteria. Keep executable checks independent
of worker summaries. Use milestone reviews to find missing complete workflows.

The historical research and migration drafts remain under `docs/research` and
`docs/archive`. They describe earlier versions and are not runtime instructions.
