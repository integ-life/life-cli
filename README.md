# life-cli

`life-cli` is a standalone Go CLI for recording local status logs, syncing them to Integ.Life, and recovering server-backed AI task decisions.

It is intentionally independent from the main `life-on-golang` backend codebase. The sync payload is defined locally in this project, and local persistence uses SQLite.

## Features

- Save a status log locally first
- Best-effort sync to `/api/notes/sync`
- Keep working offline when the network or token is unavailable
- Retry unsynced records later with `sync`
- Edit `#ai-worklog` Notes as local Markdown files
- Pull and push worklog Notes without overwriting two-sided changes
- Manage todos and todo lists locally with best-effort sync
- Track AI task runs and progress events locally first
- Save bounded server-backed context checkpoints with conditional revisions
- Ask one durable question, recover its reply after restart, and claim it once with an explicit operation key

## Install

### Download Pre-built Binary

Visit the [Releases page](https://github.com/integ-life/life-cli/releases) and download the binary for your platform:

- **macOS Intel (x86_64)**: `life-*-darwin-amd64`
- **macOS Apple Silicon (ARM64)**: `life-*-darwin-arm64`
- **Linux x86_64**: `life-*-linux-amd64`
- **Linux ARM64**: `life-*-linux-arm64`
- **Windows x86_64**: `life-*-windows-amd64.exe`

```bash
# Make the binary executable (macOS/Linux only)
chmod +x life-*

# Verify the download
./life --version

# Optionally move to a location in your PATH
mv life-* /usr/local/bin/life
```

### Build from Source

```bash
go build -o life ./cmd/life
```

### Build for Multiple Architectures

```bash
./build.sh v1.0.0
```

This creates binaries for all supported platforms in the `./build` directory and generates SHA256 checksums.

## Usage

```bash
./life "Refactored the sync handler"
./life ai "GPT-4 is getting interesting"
./life work "deep work session" --mood focused
./life ai start --project goal:<uuid> --todo <uuid> --title "Implement sync" --agent codex --json
./life ai progress --run <run_uuid> --phase coding --summary "Wrote local storage"
./life ai complete --run <run_uuid> --summary "Finished implementation"
./life note new --title "Implement cached AI worklogs"
./life note path <note_uuid_or_prefix>
./life note sync
./life list add Work --color blue --icon briefcase
./life todo add --list Work "Write release notes"
./life todo list --open
./life todo done <todo_uuid_or_prefix>
./life sync
```

### Commands

- `life '<content>'` - Record a thought with default category
- `life <category> '<content>'` - Record with a specific category
- `life ... --mood <mood>` - Attach a mood to the entry
- `life login` - Approve device login in your browser and save an API token (works on headless / non-TTY machines)
- `life login --password` - Use the legacy interactive username/password login
- `life auth status` - Verify the active login without displaying the token
- `life logout` - Remove saved API token
- `life sync` - Push all pending entries to the server
- `life list add/list/update/delete` - Manage todo lists
- `life todo add/list/show/update/done/delete` - Manage todos
- `life ai start/progress/heartbeat/event/block/complete/status/resume/resolve` - Track AI task execution progress
- `life ai context --run <uuid> [--file checkpoint.json] --json` - Read or conditionally save a server-backed checkpoint
- `life ai ask --run <uuid> --id <stable-id> --question <text> --option id=label --option id=label [--wait 30] --json` - Save or recover a durable question, then optionally contact Bridge
- `life ai decision --run <uuid> --id <stable-id> [--operation-key <key>] [--handoff-file handoff.json] --json` - Read a reply or explicitly claim it once
- `life note new/list/path/dir/sync/resolve` - Manage cached AI worklog Notes
- `life --version` - Show version

## Browser device login

Run `life login`. It prints an approval URL, an eight-character code, and a prefilled link. Open either link on a device with a browser, sign in to Integ.Life if needed, verify that the code and device name match, and choose Approve. The CLI waits for approval and saves the token to `~/.integlife/token` with mode `0600`. No password is requested by the default CLI flow.

The approval request expires after ten minutes. Polling starts at five seconds and slows down when the server requests it. Ctrl+C cancels waiting. A browser opens automatically when a local graphical session is available; on SSH or a headless machine, open the printed link on another device. Failure to open a browser does not fail login.

Each approved login creates a named, revocable `tok_` API token, with no fixed expiry, using the existing Integ.Life API token mechanism. Revoke a lost or unused device under **Settings → API tokens** in the web app. This token grants the same account API access as existing CLI tokens. `life logout` removes only the local saved token; it does not revoke server access or clear an environment override. `life auth status` verifies the active token against the API and exits nonzero if it is missing, invalid, revoked, or cannot be checked. An `INTEGLIFE_API_TOKEN` override continues to take precedence after login.

`life login --password` keeps the legacy username/password flow for interactive terminals. The server must support `/cli/device/code` and `/cli/device/token` for default device login; there is no automatic password fallback.

The web app uses hash routes, so approval links use `https://app.integ.life/#/device` (with `?code=XXXX-XXXX` after the fragment for a prefilled code). Custom servers derive this URL from their `APP_BASE_URL` setting.

## Environment variables

Primary variables:

- `INTEGLIFE_API_URL` default: `https://api.integ.life`
- `INTEGLIFE_API_TOKEN` bearer token override; takes precedence over the token saved by `life login`
- `INTEGLIFE_DB_PATH` default: `~/.integlife/integlife.db`
- `LIFE_AI_SESSION_ID` optional AI run session id used by `life ai resume`
- `LIFE_BRIDGE_URL` optional Bridge base URL; HTTPS is required except for loopback development
- `LIFE_BRIDGE_TOKEN` optional bearer token for Bridge's `work` channel; kept in memory, never saved or printed

## Behavior

- `log` writes to the local SQLite database immediately.
- If a token is configured, `log` also tries to sync the new record right away.
- `sync` sends every unsynced record in timestamp order.
- Todo and list writes are local first. If a token is configured, the CLI tries to sync through `/api/notes/sync` immediately and leaves failed records pending.
- AI task events use a canonical SHA-256 payload hash. `metadata-json` must not contain JSON numbers; use strings for numeric values.
- AI worklog Markdown files live beside the database in `~/.integlife/notes/ai-tasks/YYYY/MM/`. `life note new` creates a local pending file grouped by creation year and month; edit it directly, then run `life note sync`.
- Only remote Notes containing `#ai-worklog` are pulled into this cache. Sync reads files already registered in the local database. It does not discover new Markdown files, infer UUIDs from frontmatter, detect local filesystem renames, or transfer attachments. Keep registered file paths stable; moving a cached file locally is treated as deletion of its original path. This cache is not a complete Obsidian vault sync client.
- `life note sync` invokes the same pending sync as `life sync`, including status logs, todos, lists, replies, AI runs and events. Deleting an active cached file soft-deletes that Note on the next sync. Recreating a deleted path does not resurrect its previous UUID; unregistered files remain untouched.
- The sync baseline includes content, deletion state and path. A remote rename moves a clean cached Note only after checking that its destination is available. Active Notes, unrelated files and symlinks block an overwrite; a server deletion releases its path before another UUID can reuse it.
- If both copies changed, sync keeps the local file and persists the server content, path and optional sync version. It writes `.remote.md` when that sidecar path is available, prints the conflict, and exits non-zero. `life note list --conflicts --json` includes `notes_conflicts` with the recorded `current`, immutable `attempted` upload and optional server `conflict_copy` UUID/path. Merge the files and run `life note resolve <uuid> --local`, or use `--remote` to accept the server content and path. A blocked destination leaves the conflict unresolved.
- Server-managed Notes under `vaults/<vault_uuid>/` preserve opaque `sync_etag` and `sync_base_path` values. Uploads send that accepted baseline and require a matching explicit server acknowledgement before becoming synced. Conflict resolution refreshes the baseline from the server shadow. Ordinary worklog Notes remain compatible with legacy sync responses.
- A successful upload acknowledges the exact Note snapshot sent in that request. An edit saved by another CLI process while the request is in flight remains pending; the accepted baseline advances only when the draft still uses the same server version and path baseline.
- Sync failures do not delete local data.
- `ai context`, `ai ask`, and `ai decision` require an authenticated Life API response. They do not save an offline substitute or create a local SQLite database.
- `life ai resume` restores run tracking. The caller must separately resume its agent session or start a new session from a handoff file.

## Durable AI checkpoint and decisions

Start and sync a run using the existing `life ai start` flow. A new decision requires a checkpoint that has a goal and next step. Keep summaries compact; do not include credentials or full conversations. The checkpoint is limited to 32 KiB.

Save this as `checkpoint.json`, replacing the session reference with your caller's actual session identifier when available:

```json
{
  "schema_version": 1,
  "goal": "Write release notes for the current change",
  "summary": "Implementation and tests passed; choose the release note format",
  "next_step": "Write release notes using the selected format and report the artifact",
  "constraints": ["Use English", "Describe verified user-facing behavior"],
  "acceptance_criteria": ["One release note artifact reflects the selected format"],
  "source_refs": [{"id": "release-plan", "title": "Release plan", "version": "1"}],
  "artifact_refs": [],
  "runner_session_ref": ""
}
```

```bash
life ai context --run "$RUN_UUID" --file checkpoint.json --json
life ai context --run "$RUN_UUID" --json
life ai ask --run "$RUN_UUID" --id release-note-format \
  --question "Which release note format should I write?" \
  --option brief=Brief --option detailed=Detailed \
  --wait 30 --handoff-file handoff.json --json
```

Context writes use the fetched checkpoint revision as `expected_revision`; pass `--expected-revision <revision>` to require the revision you previously reviewed. An HTTP success without the requested run, revision, schema and saved checkpoint contents is rejected. Context reads request three recent events, preserve `truncated` and `next_cursor`, and cap the complete response at 2 MiB. Use the run events API for additional history.

The question is persisted before any Bridge request. Its stable ID binds the original options, checkpoint revision, absolute expiry and deterministic Bridge message ID. Repeating the identical `ask` recovers that decision. Changing its question, choices or expiry requires a new ID. A changed checkpoint makes the old proposal stale.

When Bridge is unconfigured or unavailable, the result contains the authenticated Integ.Life task URL. Open it on your phone to view the exact run and answer there. A wait of 0 performs one transport attempt; a wait of 1 to 60 seconds polls for a reply. Timeout, expiry, cancellation and transport failure never select a default option. Pending state returns successfully with `should_execute: false`, so callers must inspect the JSON result.

After the caller exits, recover the same decision in a new process. When configured, `decision` reads the persisted Bridge message and imports an allowed reply; it never submits a new notification. If the original Bridge submission failed, repeat the identical `ask` to retry submission with the same ID:

```bash
life ai decision --run "$RUN_UUID" --id release-note-format \
  --handoff-file handoff.json --json

# Claim only after inspecting the actual answer and its allowed operation.
life ai decision --run "$RUN_UUID" --id release-note-format \
  --operation-key write-release-notes-v1 --handoff-file handoff.json --json
```

`consumed: true` and `should_execute: true` mean the first claim to **handle the selected option** was acknowledged. Inspect `answer.option_id`: a decline must stop the proposed action, and a format choice permits only that selected format. The CLI performs no action itself. A repeated claim with the same operation key returns both flags as false, including after a later checkpoint update or expiry; it supports reconciling the original result. Another operation key cannot reuse the reply. A lost response or crash after claiming requires the caller to inspect the original artifact or external result before retrying. Consumption is evidence of a claim, not completed work.

The `handoff` JSON object and optional private `--handoff-file` contain the checkpoint, decision, current context revision, Codex session reference, and an argument array for recovery. The decision carries its own original checkpoint revision. Reopen your Codex session using your runner's existing resume flow, or have a new session read this file, recover the decision and continue the stated next step. This CLI does not launch Codex, wake an exited session, or deserialize model state. Record the resulting artifact with `life ai event --type artifact` or `life ai complete` and verify its sync status.

### Optional Bridge wire contract

Set both `LIFE_BRIDGE_URL` and `LIFE_BRIDGE_TOKEN` to enable the adapter. It uses `GET /v1/messages/{id}` before `POST /v1/messages`, reuses the persisted ID, rejects redirects, and requires an exact acknowledged message ID. The channel is `work`; the topic is the run UUID and `ref` is `https://app.integ.life/#/todo/<todo_uuid>?run=<run_uuid>`.

The Bridge v0.1 product document does not define the complete response schema. This adapter explicitly requires the following additions, which must be implemented by a compatible Bridge server:

- A caller-supplied `id` is idempotent with immutable payload conflict detection.
- `reply.options` contains objects `{ "id": "brief", "label": "Brief" }`, preserving stable IDs independently of labels.
- An optional `expires_at` is an absolute RFC3339 timestamp so retrying cannot alter the original expiry.
- POST and GET return `{ "id": "life-...", "status": "queued" }`; valid states are `queued`, `delivered`, `seen`, `answered`, `expired` and `cancelled`.
- An answered result includes `reply: { "id": "reply-...", "option_id": "brief", "text": "optional comment", "answered_at": "RFC3339" }`. A label, index or arbitrary text cannot substitute for an allowed `option_id`.

Bridge answers are imported through the authenticated Life API with `source: "bridge_transport"`. Life records the authenticated caller and transport annotation; an API token import is not proof that a human used a phone or watch. Actual Bridge service compatibility and device delivery/reply require separate runtime verification. The Integ.Life web answer path is the supported fallback in this release.

## API compatibility

This project targets the existing LifeOnGolang `/api/notes/sync` contract for `notes`, `status_logs`, `todo_lists`, `todos`, `ai_task_runs`, and `ai_task_events`, but it does not import any code from that repository.
