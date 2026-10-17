# Fork Memory Architecture

This document records a proposed memory architecture for the fork. It is not a
new runtime feature yet. The goal is to give the model operational continuity
without keeping the entire project history loaded in the active context window.

## Principle

The model should not need the full raw history for every turn. It should receive
only the minimum reliable working context needed for the current task, while the
full canonical record remains available for audit and recovery.

This is not intended to humanize the model. It is intended to reduce
out-of-context guesses by giving the model a dependable working thread:

- current objective;
- confirmed decisions;
- relevant constraints;
- important paths and commands;
- known failures and why they failed;
- evidence that supports current assumptions;
- immediate next steps.

## Layers

```text
INDEX
  cheap, small, always safe to inspect
  points to topics, checkpoints, evidence and canonical records

HOW TO
  reusable procedures
  build/run/test commands, debug workflows, import/link steps

LEARNINGS
  project conclusions
  what worked, what failed, why it failed, risks to avoid

EVIDENCE
  concrete proof
  logs, paths, hashes, manifests, code references, test results

CANONICAL
  complete raw record
  chat history, source files, logs, commits, manifests, runtime output
```

Recommended lookup flow:

```text
INDEX -> HOW TO -> LEARNINGS -> EVIDENCE -> CANONICAL
```

The model should usually work from `INDEX + HOW TO + LEARNINGS`. It should pull
`EVIDENCE` when it needs to verify a claim. It should return to `CANONICAL` when
there is conflict, uncertainty, regression investigation or audit work.

## Current State

The fork already has pieces that point in this direction:

- Full chat history is stored canonically.
- Non-destructive context compaction checkpoints exist for chat continuation.
- Context compaction stores covered message count, fingerprint, summary content,
  estimated tokens before and estimated tokens after.
- Repeated compaction consolidates the previous checkpoint plus the newly covered
  canonical delta instead of regenerating from message zero forever.
- Failed compaction does not mutate canonical history.
- Context trace logging can show when compaction was skipped, projected, created
  or failed, without logging message text by default.
- `app/Changes.md` is being used as a human-maintained change and decision log.

## Missing V1 Pieces

To turn this into a proper feature, the fork still needs:

- `INDEX`: a small searchable index of memory records by topic, model, file,
  subsystem, command, error signature and date.
- `HOW TO`: stored procedures for common local workflows, for example starting
  the fork, rebuilding the payload, linking GGUFs, validating CUDA runtimes and
  checking model manifests.
- `LEARNINGS`: compact records for durable conclusions, such as "C: is
  disposable", "Linked GGUF requires hardlinkable same-volume storage", or
  "Qwen3.5 vision requires the completion multimodal path with mmproj".
- `EVIDENCE`: structured pointers to supporting logs, code lines, test output,
  manifest paths and command outputs.
- Retrieval policy: rules for when a Send should inject memory and how much.
- Source labels: every injected memory should distinguish memory-derived facts
  from facts verified in the current runtime/workspace.
- Invalidation policy: records tied to files, manifests, models or settings need
  refresh markers when those sources change.

## Suggested V1 Storage

Keep the first implementation boring and auditable:

```text
memory/
  index.jsonl
  howto/
    *.md
  learnings/
    *.md
  evidence/
    *.jsonl
  checkpoints/
    *.jsonl
```

Each record should include:

- stable id;
- type: `index`, `howto`, `learning`, `evidence`, `checkpoint`;
- title;
- tags;
- source paths or chat ids;
- created/updated timestamps;
- confidence/source label;
- short content;
- optional evidence references.

## Injection Rules

Memory injection should be conservative:

- Do not inject the whole memory store.
- Prefer small summaries and pointers.
- Do not let memory override current user instructions.
- Do not let memory override current source/runtime evidence.
- Do not hide uncertainty; mark stale or unverified memory as such.
- Do not log or inject private/raw content unless required for the task.

## Relationship To Ollama System Prompts

Ollama does not have one universal global system prompt for every request in the
same sense as an application-level agent instruction.

The current code paths use several sources:

- `/api/generate` has a `system` field that can override the model default
  system message for that request.
- Modelfiles can define `SYSTEM`, `TEMPLATE`, `MESSAGE` and parameters that are
  stored in the model manifest/config.
- `/api/chat` combines model-defined messages with request messages.
- If the first request message is not a `system` message and the model has
  `m.System`, the server prepends a `system` message with that content.
- The desktop chat stores and sends normal chat messages; any memory injection
  should therefore be an explicit app-layer decision, not an assumed hidden
  Ollama global prompt.

For this fork, the recommended approach is to treat memory as an app-layer
context source that can add an explicit system/developer-style context block
when needed, while preserving the canonical chat and model-native template flow.

