package looper

import (
	"fmt"
	"strings"
)

// Contextual help for one command (AX-026). The overview in cli.go lists the
// surface; this block answers "how do I run this command" for each entry:
// usage, what it does, runnable examples, the documents it emits, and the
// semantics a caller cannot guess — pagination, streaming, exit codes, and
// waits. Tests cross-check every block against the parser and the contract
// registry, so the help cannot drift from the commands it describes.
type commandDoc struct {
	// usage is the argument shape without the leading program name.
	usage string
	// summary says what the command does, in two or three sentences.
	summary string
	// examples are complete invocations the parser accepts as written.
	examples []string
	// input, legacy, and v2 name contract documents relative to the
	// repository: an input schema the caller writes, and the output schema
	// each contract version serves. Empty means the command emits no such
	// document.
	input  string
	legacy string
	v2     string
	// notes carry the semantics a caller cannot guess.
	notes []string
}

const (
	legacySchemas = "docs/contracts/schemas/legacy/"
	v2Schemas     = "docs/contracts/schemas/v2/"
)

// commandDocs is the contextual help for every command the parser accepts.
// The keys are exactly the names in clientCommands; a test refuses a name on
// one list and not the other.
var commandDocs = map[string]commandDoc{
	"run": {
		usage:   "run [task-file] [options]",
		summary: "Start or resume a controller in the foreground. This is the default command: a bare `looper` runs it.",
		examples: []string{
			"looper run --once",
			"looper run --jobs 4 --check-jobs 2",
			"looper run alternate-tasks.json --agent claude",
		},
		notes: []string{
			"`--once` finishes after the current goal is checked or all remaining work is blocked.",
			"A concise launch line prints before any controller or agent starts; `--contract v2 --json` emits the same facts as one leading NDJSON `launch` event, then the journal.",
			"The first interactive run configures machine agent commands and project roles; `--yes` explicitly accepts defaults. Fresh unattended projects need an explicit agent or saved choice.",
			"SIGTERM drains like `looper stop`; SIGINT cancels with the work retained. Run `looper` again to resume.",
		},
	},
	"start": {
		usage:   "start [--plan <path>...] [options]",
		summary: "Start a detached controller and return when its run is saved and queryable. Attach with `looper tui` or `looper attach`.",
		examples: []string{
			"looper start",
			"looper start --plan docs/PLAN.md",
		},
		notes: []string{
			"Start succeeds only after the run is committed: `looper status` finds it immediately, even for a run that finishes at once.",
			"A failed startup returns structured evidence instead of a started run.",
			"While a controller owns the repository, `--plan` files are not submitted: the start refuses and names `looper add-plan` instead of spawning a second controller.",
			"Stop it with `looper stop`, optionally `--wait` for the durable drain result.",
		},
	},
	"tui": {
		usage:   "tui [--plan <path>...] [--foreground] [options]",
		summary: "Open the dashboard for controlling and observing a run. It starts a detached controller when none owns the repository, then attaches to it.",
		examples: []string{
			"looper tui",
			"looper tui --no-animation",
			"looper tui --foreground",
		},
		notes: []string{
			"`q` asks before it leaves: drain, interrupt with retained work, or detach while the run continues.",
			"`--foreground` runs the controller inside the dashboard process, so the run stops when the dashboard exits.",
			"While a controller owns the repository, `--plan` files submit through the add-plan request and the dashboard shows the receipt.",
			"The non-interactive fallback prints that receipt on stderr, so its followed event stream stays one document per line.",
		},
	},
	"attach": {
		usage:   "attach",
		summary: "Attach as a read-only observer of the saved run. It never starts a controller and never accepts `--plan`.",
		examples: []string{
			"looper attach",
		},
		notes: []string{
			"Stopping an observer does not drain the controller.",
			"Use `looper start` first when no controller runs.",
		},
	},
	"status": {
		usage:   "status [--ls <status>] [--json] [--full] [--contract legacy|v2]",
		summary: "Report the run's identity, ownership, active work, waiting reason, and continuation cursor. `--contract v2` returns the bounded compact document; `--full` keeps the complete state.",
		examples: []string{
			"looper status --contract v2 --json",
			"looper status --ls blocked",
			"looper status --contract v2 --full --json",
		},
		legacy: legacySchemas + "status.schema.json",
		v2:     v2Schemas + "status-compact.schema.json",
		notes: []string{
			"The compact document stays at or below 8 KiB whatever the backlog holds, and its `limits` and `detail` routes name what it omitted and where to read it.",
			"A piped plain status renders the same summary: liveness, in-flight work, the waiting reason, and the cursor. An interactive terminal keeps the status board.",
			"With no saved run the version 2 call exits 3 with the searched locations; `looper runs` lists every saved run.",
			"`detail.wait` names the finite wait that observes the next change from this cursor.",
		},
	},
	"runs": {
		usage:   "runs [--limit N] [--contract v2] [--json]",
		summary: "List the saved runs under the resolved base directory, newest activity first, with the command that selects each.",
		examples: []string{
			"looper runs --contract v2 --json",
			"looper runs --limit 5 --contract v2 --json",
		},
		v2: v2Schemas + "runs.schema.json",
		notes: []string{
			"`--limit` bounds the page (default 20, at most 100); `limits` reports what the byte budget or the bound omitted.",
			"Each entry names the project root, retained work, the last recorded outcome, controller freshness (`owning`, `stale`, `stopped`, `unknown`), and build evidence.",
			"The listing is read-only and scans only Looper-owned runtime directories; private agent session directories are never scanned. An empty base directory is a bounded result with a start hint.",
		},
	},
	"inspect": {
		usage:   "inspect <task-id> [--after N] [--checks-after N] [--limit N] [--json] [--full] [--contract legacy|v2]",
		summary: "Report one task's contract, current attempt, bounded history, timing, and evidence. `--contract v2` returns the bounded compact document.",
		examples: []string{
			"looper inspect T015 --contract v2 --json",
			"looper inspect T015 --contract v2 --after 3 --json",
			"looper inspect T015 --checks-after 412 --limit 20 --contract v2 --json",
			"looper inspect T015 --contract v2 --full --json",
		},
		legacy: legacySchemas + "inspect.schema.json",
		v2:     v2Schemas + "inspect-compact.schema.json",
		notes: []string{
			"`--after` continues attempt history after a one-based attempt number; `--checks-after` continues check history after a check row ID. Both are exclusive, so pages have no gaps and no duplicates.",
			"`--limit` bounds a page (default 10, at most 100); the byte budget may return fewer, which the page reports. `--full` returns the complete unpaged document.",
			"The compact document stays at or below 8 KiB; `detail` names the routes into the remaining history and the task's journal query.",
			"An unknown task ID exits 1 with the identifier it could not find.",
		},
	},
	"events": {
		usage:   "events [--after <event-id>] [--limit N] [--task <task-id>] [--kind <kind>] [--full] [--follow] [--json] [--contract legacy|v2]",
		summary: "Read the event journal. A finite query under version 2 returns one bounded page; `--follow` streams new events as NDJSON under both versions.",
		examples: []string{
			"looper events --contract v2 --json",
			"looper events --contract v2 --after 1202 --limit 50 --json",
			"looper events --contract v2 --task T015 --kind failure --json",
			"looper events --follow --json --after 1202",
			"looper events --full --json",
		},
		legacy: legacySchemas + "event-ndjson.schema.json",
		v2:     v2Schemas + "events-page.schema.json",
		notes: []string{
			"The bare version 2 query returns the newest 50 events, oldest first within that tail. `--limit` bounds the page (1 to 500); `--full` exports the complete history.",
			"`--after` is exclusive and continues from the cursor a status or page reported. A filtered page advances its continuation even when nothing matches, so filtering never stalls.",
			"`--task` matches the structured `task_id` the controller records, not message text. `--kind failure` returns every bounded failure summary a run recorded.",
			"Each item carries `task_id`, `attempt_id`, `controller_session`, `phase`, and `request_id` where they apply; records written before they existed stay readable and surface none.",
		},
	},
	"tail": {
		usage:   "tail [--after <event-id>] [--limit N] [--task <task-id>] [--kind <kind>] [--full] [--follow]",
		summary: "Read the event journal as plain text, one line per stored event. The same filters and bounds as `looper events` apply.",
		examples: []string{
			"looper tail",
			"looper tail --follow --after 1202",
			"looper tail --task T015 --limit 20",
		},
		notes: []string{
			"The bare command prints the newest 50 events, oldest first within that tail, and names the route to the older history it did not print.",
			"`--follow` keeps streaming new events until you stop it; stopping an observer does not drain the controller.",
		},
	},
	"doctor": {
		usage:   "doctor [--json] [--contract legacy|v2]",
		summary: "Report health findings with codes, severities, and suggested actions, plus the bounded pending-request summary of the saved run.",
		examples: []string{
			"looper doctor --json",
			"looper doctor --contract v2 --json",
		},
		legacy: legacySchemas + "doctor.schema.json",
		v2:     v2Schemas + "doctor.schema.json",
		notes: []string{
			"Under version 2 findings never change the exit code: a command that completed exits 0 while reporting an unhealthy run. The legacy doctor keeps exit 1 on error-severity findings.",
			"The version 2 document adds one bounded `waiting_on` list naming the reason each waiting request gives.",
			"The plain command also checks installation dependencies and the backlog.",
		},
	},
	"requests": {
		usage:   "requests [<id>] [--after <request-id>] [--limit N] [--contract v2] [--json]",
		summary: "Read the durable request records of every mutation — add-task, add-plan, cancel-task, amend-task, settings, requeue, recovery, and stop — one record by identifier or one bounded page.",
		examples: []string{
			"looper requests req-1c0ffee --contract v2 --json",
			"looper requests --contract v2 --json",
			"looper requests --after req-1c0ffee --limit 20 --contract v2 --json",
		},
		v2: v2Schemas + "request-record.schema.json",
		notes: []string{
			"Each record names the lifecycle state from docs/contracts/request-states.json, the age, the wait reason with its own age, the deciding session, the affected tasks, plan coverage, the outcome, and safe error evidence.",
			"The page is oldest first; `--after` is exclusive and the continuation command names the exact next call. One record query takes no page flags.",
			"A request no receipt or envelope names exits 1 with `request_not_found` and the number of records the run holds. With no saved run the call exits 3 with the searched locations.",
			"This command replaces reading the acknowledgement directory or the state database: it answers on a live or a stopped run alike.",
		},
	},
	"wait": {
		usage:   "wait (--cursor N | --task T [--status S] | --request ID | --run) --timeout <seconds> [--contract v2] [--json]",
		summary: "Observe one predicate for one explicit finite bound and return one outcome document. It replaces a polling loop with a single call.",
		examples: []string{
			"looper wait --cursor 1202 --timeout 30s --contract v2 --json",
			"looper wait --task T015 --status done --timeout 120s --contract v2 --json",
			"looper wait --request req-1c0ffee --timeout 30s --contract v2 --json",
			"looper wait --run --timeout 300s --contract v2 --json",
		},
		v2: v2Schemas + "wait.schema.json",
		notes: []string{
			"`--timeout` is required and names the finite bound; a bare number counts seconds and `30s` names its unit. The bound never defaults.",
			"The outcome is `satisfied`, `timeout`, or `cancelled`. A timeout is exit 5 and an interruption exit 6, and neither cancels anything: the request, task, and controller keep their state.",
			"The document always carries the store's committed cursor, so observation continues without a gap whatever the outcome.",
			"Targets bind as appropriate: the cursor to the retained store, the task and request to the saved run, and the run to one controller session, so a replacement controller never answers for the bound one.",
			"A condition that already holds returns at once. `looper events --follow` remains the streaming alternative.",
		},
	},
	"stop": {
		usage:   "stop [--wait] [--wait-timeout <seconds>] [--interrupt] [--contract legacy|v2] [--json]",
		summary: "Request a drain from the controller that owns the repository. Active work finishes; no new work starts.",
		examples: []string{
			"looper stop",
			"looper stop --wait --contract v2 --json",
			"looper stop --interrupt",
		},
		v2: v2Schemas + "request-receipt.schema.json",
		notes: []string{
			"The request binds to the owning session, so a stop without a live controller changes nothing and a later start runs normally.",
			"Version 2 returns the receipt naming the derived stop request identity. `--wait` waits bounded for the durable drain result, bound to that session; a timeout leaves the drain running.",
			"Nothing is stopped and nothing is written while no controller holds the repository lock.",
			"`--interrupt` stops at once, like Ctrl+C: child processes stop and partial work is retained for the next run. It does not combine with `--wait` or version 2.",
		},
	},
	"recovery": {
		usage:   "recovery --preview | recovery --apply <choice> --token <token> [--wait] [--idempotency-key <key>] [--contract v2] [--json]",
		summary: "Preview a retained-work situation read-only, then apply one enumerated choice through a checked operation.",
		examples: []string{
			"looper recovery --preview --contract v2 --json",
			"looper recovery --apply restore-task-file --token 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef --contract v2 --json",
		},
		v2: v2Schemas + "recovery-preview.schema.json",
		notes: []string{
			"`--preview` changes no file, database record, process, or Git ref. It compares the task file, branch, working tree, and retained publication against the saved contract, reports each startup guard verdict, and lists the supported choices with what each preserves or changes.",
			"`--apply` re-derives the token and refuses a stale preview without mutation. `restore-task-file` returns the accepted bytes; `import-backlog` applies the external edit; `discard-retained` stays unsupported.",
			"A live controller claims the recovery request at its boundary; a stopped run applies under the repository lock without launching agents and reports the decided record at once.",
			"Every retained-work guard error names `looper recovery --preview --contract v2 --json` as its first next action.",
		},
	},
	"preflight": {
		usage:   "preflight [--contract v2] [--json]",
		summary: "Show the execution configuration a start would use — executables, agents, models and their sources, launch mode, once semantics, verify and setup — without starting anything.",
		examples: []string{
			"looper preflight",
			"looper preflight --contract v2 --json",
		},
		v2: v2Schemas + "preflight.schema.json",
		notes: []string{
			"The command is read-only: no controller, agent, runtime directory, or saved record is created, and nothing is billed.",
			"Credentials, environment values, and raw agent arguments never enter the document; what was not verified stays in `unchecked`.",
			"Missing agent binaries and core tools exit 1 like doctor. Missing command executables, an unreadable saved run, and an owned repository are warnings that exit 0.",
		},
	},
	"version": {
		usage:   "version | --version [--json] [--contract legacy|v2]",
		summary: "Report the executable's build identity. `--contract v2` serves the capabilities document: contract versions, the command list, the request types, and the live controller's session and build.",
		examples: []string{
			"looper version",
			"looper --version --json",
			"looper version --contract v2 --json",
		},
		legacy: legacySchemas + "version.schema.json",
		v2:     v2Schemas + "capabilities.schema.json",
		notes: []string{
			"`looper version` and `looper --version` are the same command in two spellings.",
			"Identity comes from the executable's own build metadata. A build without Git metadata reports `unknown`; Looper never infers its revision from the project repository it runs in.",
			"Read `controller.request_types` before submitting to an older controller, and submit only the types it lists.",
		},
	},
	"add-task": {
		usage:   "add-task <file.json> [--wait] [--wait-timeout <seconds>] [--idempotency-key <key>] [--contract legacy|v2] [--json]",
		summary: "Submit new tasks to the active controller through the durable request transport. The command returns a receipt at once; the controller owns the outcome.",
		examples: []string{
			"looper add-task tasks.json --json",
			"looper add-task tasks.json --wait --contract v2 --json",
			"looper add-task tasks.json --idempotency-key release-3 --json",
		},
		input:  "docs/to-do.schema.json",
		legacy: legacySchemas + "settings-ack.schema.json",
		v2:     v2Schemas + "request-receipt.schema.json",
		notes: []string{
			"The input file holds one task object or an array of them. A minimal task carries `id`, `title`, `priority` (1 to 5), and `details`; `status` defaults to `todo` and may also be `blocked`. The controller validates the whole batch before adding it, and an invalid batch adds nothing.",
			"The receipt's `wait` object names the bounded wait that observes this request: `looper wait --contract v2 --request <id> --timeout 30s`.",
			"`--wait` waits bounded for the applied acknowledgement (default 30 seconds). A timeout reports the request still running; it never cancels or rejects.",
			"`--idempotency-key` (1 to 128 bytes, no control characters) makes a retry return the original request; a changed batch under the same key is a conflict.",
			"This command needs an active controller that supports `add-task`. Installing a new binary does not update a running controller.",
		},
	},
	"add-plan": {
		usage:   "add-plan <path> [...] [--wait] [--wait-timeout <seconds>] [--idempotency-key <key>] [--contract legacy|v2] [--json]",
		summary: "Submit additional Markdown requirements to the active controller. The planner turns them into tasks; the command returns a receipt at once.",
		examples: []string{
			"looper add-plan docs/PLAN.md --json",
			"looper add-plan docs/PLAN.md --wait --contract v2 --json",
		},
		legacy: legacySchemas + "plan-import-ack.schema.json",
		v2:     v2Schemas + "request-receipt.schema.json",
		notes: []string{
			"A satisfied `--wait` reports the added task identifiers, the covered requirements, adjustments, and gates (bounded to 60 seconds by default).",
			"`--idempotency-key` addresses the plan content, not its path: the same key and bytes returns the original import wherever the same content came from.",
			"Without a controller the command suggests the startup path: `looper --plan <path>`.",
		},
	},
	"cancel": {
		usage:   "cancel <task-id> [--cascade] [--wait] [--wait-timeout <seconds>] [--idempotency-key <key>] [--contract legacy|v2] [--json]",
		summary: "Withdraw open work through a checked controller request. The task revision the caller read must match; live dependents are refused by name until `--cascade` takes them along.",
		examples: []string{
			"looper cancel T014 --json",
			"looper cancel T014 --cascade --wait --contract v2 --json",
		},
		legacy: legacySchemas + "settings-ack.schema.json",
		v2:     v2Schemas + "request-receipt.schema.json",
		notes: []string{
			"Cancelled is not done: a cancelled task carries the `cancelled` status, never satisfies a dependency, and never re-enters the ready set.",
			"With work in flight the cancellation is decided and pending: the receipt waits with the reason `active_work`, the worker finishes first, and its verified result is never discarded.",
			"Read the outcome through `looper requests <id>`, `looper inspect <task-id>`, or the `cancel` events in the journal.",
			"This command needs an active controller that supports `cancel-task`.",
		},
	},
	"amend": {
		usage:   "amend <task-id> <task.json> [--wait] [--wait-timeout <seconds>] [--idempotency-key <key>] [--contract legacy|v2] [--json]",
		summary: "Amend one task contract through a checked controller request. The file carries the complete amended task, and the revision the caller read must match.",
		examples: []string{
			"looper amend T014 task.json --json",
			"looper amend T014 task.json --wait --contract v2 --json",
		},
		input:  "docs/to-do.schema.json",
		legacy: legacySchemas + "settings-ack.schema.json",
		v2:     v2Schemas + "request-receipt.schema.json",
		notes: []string{
			"The input file holds the one complete amended task object.",
			"Title, priority, details, steps, `depends_on`, files, blockers, tags, and `unblock_when` are amendable. Identity, status, and `created_at` are not, and control tags (`project-done`, `looper:operator`, `looper:deferred`, `looper:finding:*`) cannot be added or removed.",
			"Dependency cycles, unknown dependencies, and invalid write paths are refused before anything commits.",
			"Work that holds the task — running, verifying, or retained — is refused with the boundary named: wait for it to finish or cancel it first.",
			"A done task is never rewritten: the changed requirements create a follow-up task `<id>-amend-<n>` while the accepted result stays.",
			"This command needs an active controller that supports `amend-task`.",
		},
	},
	"config": {
		usage:   "config [--test-agent <profile>] [--reset] [--wait] [--wait-timeout <seconds>] [--idempotency-key <key>] [--contract legacy|v2] [--json]",
		summary: "Configure machine agent commands and project worker/reviewer profiles. Online project changes return an immediate receipt.",
		examples: []string{
			"looper config",
			"looper config --reset",
			"looper config --test-agent glm",
		},
		legacy: legacySchemas + "settings-ack.schema.json",
		v2:     v2Schemas + "request-receipt.schema.json",
		notes: []string{
			"The form edits machine agents at $XDG_CONFIG_HOME/looper/agents.json (default ~/.config/looper/agents.json) and project choices in runtime/profile.json. LOOPER_AGENTS_FILE selects a different machine catalog.",
			"`--reset` removes only the project profile without reading it. Machine command edits apply after controller restart.",
			"`--test-agent <profile>` explicitly runs a live structured-output test and consumes agent usage. Configuration and preflight never run that test implicitly.",
			"With --test-agent, --contract v2 --json returns schemas/v2/agent-test.schema.json instead of a settings receipt.",
			"Settings that a live controller can apply are submitted as a `settings` request and answered with the same receipt, wait, and caller key as every other mutation.",
		},
	},
	"help": {
		usage:   "help [<command>] | <command> --help",
		summary: "Show the command overview, or one command's own help block with its examples, documents, and semantics.",
		examples: []string{
			"looper help",
			"looper help status",
			"looper wait --help",
		},
		notes: []string{
			"A topic that names no command is a usage error, and an unknown command is refused the same way: `looper bogus` exits 2 without starting anything.",
			"Help is text. The machine-readable command surface is the capabilities document: `looper version --contract v2 --json`.",
		},
	},
}

// renderCommandHelp formats one command's contextual help block. The name is
// validated during parsing, so the block always exists.
func renderCommandHelp(name string) string {
	doc := commandDocs[name]
	var b strings.Builder
	fmt.Fprintf(&b, "looper %s\n\n", name)
	fmt.Fprintf(&b, "Usage: looper %s\n\n", doc.usage)
	b.WriteString(wrapHelp(doc.summary, 78))
	b.WriteString("\n\nExamples:\n")
	for _, example := range doc.examples {
		fmt.Fprintf(&b, "  %s\n", example)
	}
	b.WriteString("\nDocuments:\n")
	if doc.input != "" {
		fmt.Fprintf(&b, "  input:  %s\n", doc.input)
	}
	switch {
	case doc.legacy != "" && doc.v2 != "":
		fmt.Fprintf(&b, "  legacy: %s\n", doc.legacy)
		fmt.Fprintf(&b, "  v2:     %s (--contract v2)\n", doc.v2)
	case doc.legacy != "":
		fmt.Fprintf(&b, "  legacy: %s\n", doc.legacy)
	case doc.v2 != "":
		fmt.Fprintf(&b, "  v2:     %s (--contract v2)\n", doc.v2)
	default:
		b.WriteString("  none: this command emits no contract document\n")
	}
	if len(doc.notes) > 0 {
		b.WriteString("\nSemantics:\n")
		for _, note := range doc.notes {
			for i, line := range strings.Split(wrapHelp(note, 76), "\n") {
				if i == 0 {
					b.WriteString("  - " + line + "\n")
					continue
				}
				b.WriteString("    " + line + "\n")
			}
		}
	}
	b.WriteString("\nExit codes follow docs/contracts/exit-codes.json: 0 completed, 1 refused\nor failed, 2 usage, 3 nothing to read, 4 incompatible, 5 wait timeout,\n6 cancelled (codes 3 to 6 apply to calls that select --contract v2).\nUnder --contract v2 --json, one document per call: the schema-validated\nsuccess document or the version 2 error document.\n")
	return b.String()
}

// wrapHelp keeps help lines near the width the overview uses. A backtick
// span stays one unit, so wrapping never splits a command or a field name
// the caller is meant to copy.
func wrapHelp(text string, width int) string {
	var b strings.Builder
	line := ""
	for _, token := range helpTokens(text) {
		if line == "" {
			line = token
			continue
		}
		if len(line)+1+len(token) > width {
			b.WriteString(line + "\n")
			line = token
			continue
		}
		line += " " + token
	}
	b.WriteString(line)
	return b.String()
}

// helpTokens splits text into wrap units: `code` spans stay whole and
// everything else splits on spaces.
func helpTokens(text string) []string {
	var tokens []string
	for text != "" {
		start := strings.IndexByte(text, '`')
		if start < 0 {
			return append(tokens, strings.Fields(text)...)
		}
		if start > 0 {
			tokens = append(tokens, strings.Fields(text[:start])...)
		}
		rest := text[start+1:]
		end := strings.IndexByte(rest, '`')
		if end < 0 {
			return append(tokens, strings.Fields(text[start:])...)
		}
		tokens = append(tokens, text[start:start+end+2])
		text = text[start+end+2:]
		// Punctuation hugging a span stays with it, so wrapping cannot
		// produce "`id` ,".
		for text != "" && strings.IndexByte(",.;:!?", text[0]) >= 0 {
			tokens[len(tokens)-1] += text[:1]
			text = text[1:]
		}
	}
	return tokens
}
