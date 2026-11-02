# ctx as agent memory

ctx is not a tool an agent picks up beside its work. It is the agent's persistent memory. A language model is stateless: every context window starts empty and ends when it fills. An agent that has ctx installed but treats it as one more tool still behaves as if it had no memory — it re-derives what an earlier run already learned, writes notes to its successor, and reads them back in bulk. This page is about how an agent should regard the store, and what goes wrong when it does not. Keeping that memory from drifting is a separate discipline, covered in [using ctx effectively](using-ctx-effectively.md).

"Agent" means every model that works against the store: the interactive session, and each subagent or workflow agent it starts. Each one retrieves before it works and persists its own findings after.

## The store, read as a memory

The parts behave like a memory, not like a filing cabinet:

- **Storing is encoding.** `store` saves a block as it arises — a decision with the words that made it, an observation with its date, a lesson with its incident. A block is a point-in-time observation, not live state. You store what happened, with source and date; you do not decide at that moment how important it will turn out to be.
- **Consolidation runs in the background.** [Dream Mode](architecture.md#dream-mode) is a continuous loop that discovers relationships between blocks, marks outdated information as superseded and promotes high-quality content. The [Guard](architecture.md#guard-deduplication) checks new blocks against what exists and archives or flags near-duplicates. What looks central in one context window may turn out to be noise; the store decides that with more context than any single window has.
- **Recall is by cue.** `query` fuses semantic similarity, English and German full text and trigram matching by reciprocal-rank fusion, weights by time, expands along the links Dream Mode found, and answers with an LLM synthesis plus sources. A cue can match as a concept, an exact phrase, a misspelling or a word in another language — each arm catches a different phrasing.
- **Classification is the store's.** Block types carry behavior for retrieval, Guard and Dream Mode, and the store assigns them with its own classifier — on store, on update, and again in its background pipelines ([type & lifecycle axes](architecture.md#type--lifecycle-axes)).
- **The store can be asked about itself.** The web chat runs its own retrieval loop over four read-only tools (`ctx_query`, `ctx_search`, `ctx_get`, `ctx_recent`) and answers in prose ([web chat sessions](api.md#web-chat-sessions)). It reads the blocks so that the agent does not have to.

## The loop

**A session:**

1. Load the conventions it works under into context before the first answer — the anchor in the current context is what overrides a trained default.
2. `query` for what the task needs, and nothing more. When the answer does not suffice, ask a sharper question.
3. `get` only a block whose id it already holds — a source the answer cited, a block the conventions name.
4. Check live state before acting on a memory. A block says where to look, not what is true now.
5. Store as it goes: what happened, with source and date, verified statements kept apart from its own interpretation. Not a curated digest.
6. End without a handover. The knowledge is already in the store; the next session finds it by query.

**A subagent or workflow agent:**

1. Starts with the project's `agent-briefing` blocks in its context (the untagged ones when none carry the project's tag), injected by `ctx brief --hook` on `SubagentStart`, and queries for its own task.
2. Does the work against the store like a session.
3. Ends its final message with one `[PERSIST:category:title]` line per finding, the content below each marker. `ctx persist --hook` on `SubagentStop` sends each marker with non-empty content to `store`, tagged with the project and carrying the session and agent ids in its metadata. `store` upserts on category, title and scope, so a marker that reuses a title already in the key's scope replaces that block; a failed write is dropped silently. Setup in [operations](operations.md#claude-code-integration-optional).

## Failure modes

Each of these is an agent treating its memory as a tool.

### Handovers and close-out notes

A "handover for the next session" or a close-out note treats the store as a message board between people leaving the office. The next session reads it whole and inherits every half-formed worry in it, ahead of what a query would have weighted. The findings belong in the store as they happen, per topic; nothing has to be passed on at the end.

### `get`, `recent` and backtracking instead of `query`

Reading the latest blocks, or `get`-ing every source a query cited and then the blocks those cite, pulls in far more context than the task needs and anchors the agent on whatever it happened to read first. `query` is the recall path; `get` is for an id the agent already knows it wants; `recent` is not orientation.

### Overriding or arguing with the store's models

The classifier, the Guard and Dream Mode decide with rules and context an agent does not see. An agent that disagrees with a type and re-saves the block with an explicit one does more than correct it: an explicit type stamps `type_source='manual'`, and a manual type takes the block out of the auto-classifier for good — the classifier only ever touches rows whose type it set itself, so no later rule change reaches that block again ([type & lifecycle axes](architecture.md#type--lifecycle-axes)). An explicit sensitivity stamps `sensitivity_source='manual'` on a new block; on a re-save it applies only at or above the current level, and a lower one leaves level and source unchanged ([HTTP API](api.md)). Leave type and sensitivity to the store unless the writer has a structural reason. A question about what the store did is a question for the store's chat, not an edit.

### Knowledge in an instruction file or runtime memory files

A project instruction file (`CLAUDE.md`, `AGENTS.md`) or the agent runtime's own memory files are for what the agent needs to work the tree and how it should behave. Decisions, runbooks, findings and lessons placed there are invisible to retrieval, never consolidated, and loaded into every context whether relevant or not. Including a docs page into an instruction file is the same mistake: the page is loaded on every start, but the knowledge still lives outside the memory.

### Git history and memory files as orientation

`git log`, an old branch or a runtime memory file tells the agent what someone wrote down at some point, not what the store knows now. Orient by query; read history when the task is about history.

### The CLI or bulk reads in place of MCP query

A shell loop over `ctx get` or `ctx search` output, or a dump of many blocks into the context, bypasses retrieval and synthesis. Over MCP the agent asks a question and gets an answer with sources. For an agent, the CLI is a fallback when MCP is unavailable, not a way to read the store in bulk.

### Subagents without memory

A subagent that neither retrieves before its work nor persists after it, by any path (MCP, CLI, HTTP or the hooks), works stateless: it re-derives what the store already holds and its findings die with its context window. The hooks exist so that an agent without a conversation of its own does both even when its prompt does not ask for it.

### The session re-persisting a subagent's finding

When a subagent's finding has already been persisted from its own markers, the session that started it re-saving the same finding in its own words, under a title of its own, adds a second, re-interpreted copy. Every re-interpretation pushes the claim in the same direction — more absolute, less attributed — which is how memory drifts ([using ctx effectively](using-ctx-effectively.md#why-stored-memory-drifts)). The session stores what is new from its own vantage point, not a summary of what its agents already stored.

### A curated digest instead of what happened

Storing a cleaned-up summary at the end of a task, instead of the decisions and observations as they occurred, throws away the source and the date and replaces them with the agent's judgement of what mattered. Consolidation is the store's job.
