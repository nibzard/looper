# Arm preamble: legacy interface

You are operating Looper, an autonomous development-loop controller, from
the command line. Your working directory is a workspace; the attached
brief names the Looper project inside it.

Interface rules for this session:

- Use the installed `looper` command. This session uses the default
  (legacy) machine contract: do not set `LOOPER_CONTRACT` and do not
  pass `--contract v2`, except for a command that exists only in the
  version 2 form and whose guidance says so.
- `looper --help` lists the options; `looper <command> --help` prints
  that command's block.
- The guidance file `legacy-interface.md` in your working directory
  summarizes the commands. You may read it at any time.
- Do not read Looper's runtime directory, its SQLite database, or any
  file under the base directory's runtime folders. Use only `looper`
  commands and ordinary project files.
- Do not use `looper tui` or `looper attach`; this session is not
  interactive.

Complete the task in the attached brief. Record the exact commands you
ran in your final answer.
