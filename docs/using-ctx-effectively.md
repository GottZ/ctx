# Using ctx effectively

Installing ctx gives an agent memory. Using it *well* takes discipline — because a memory shared across sessions has a failure mode a single chat doesn't: **drift**.

## Why stored memory drifts

Each time an LLM reads a note and re-saves or summarizes it, it re-interprets it through its own training biases. That isn't random noise — it's a directional filter that pushes the same way every pass: more conservative, more absolute, less attributed. Observations harden into recommendations, recommendations into rules, rules into dogma — and the certainty becomes untraceable.

A stored block is also a *point-in-time observation, not live state*. A note that was true when written ("we migrated off X") can stay true and still drive a wrong action (deleting X's still-running sibling service) — because the scope shifted and the note never said so. The note tells you where to look, not what's true right now.

## Discipline — put this in your agent's instructions

- **Load conventions into context before working — don't just file them away.** Effectiveness ranks training-weights > file-instructions > in-context anchors: only an anchor in the *current* context reliably overrides a trained default. A discipline doc that's never loaded gets silently re-undermined by each new session. (`ctx query` your project conventions at session start.)
- **Trace every stored claim to a source.** Save quote + date; keep verified user statements separate from your own interpretation. An interpretation re-saved as fact is how a "probably" disappears across three persistence layers.
- **Cross-check stored claims against live state before acting.** Before a destructive or status-dependent step, verify against the authoritative source — live config, a test, the actual file — not the note.
- **Don't gate on self-reported confidence.** Models are often just as sure when wrong. Gate on external truth: a test, the source, observed behavior.
- **Prefer external signals over self-reminders.** Naming a failure mode as a rule ("don't forget the tests") tends to re-evoke it; build a check instead — a test script, a grep on the output, a verifier against the raw data.

## Calibration

LLM defaults are tuned for a median user who must be protected from uninformed decisions. For an experienced operator with a defined target, the same training produces systematic distortion: judging against the current state instead of the target ("good enough for now"), preferring the familiar over the better option, asking permission on obvious next steps while making user-facing decisions unprompted, and presenting trained caution as judgement ("that's overkill") with no concrete risk named.

Compensating it is a one-time setup the agent should drive:

1. **Store the calibration as a block.** Have the agent write your conventions and observed failure modes into ctx — a dedicated "RLHF warnings" block is a good seed — so every future session can retrieve them instead of relearning them.
2. **Point your durable instructions at that block.** Your platform's personal-preference / custom-instruction field, or a project-level instruction file, should reference it. This is the step the agent should *prompt you* to do — it's the one layer the agent can't write for itself, and without it the block just sits there unread.
3. **Each session loads the anchor.** The durable instruction tells the agent to `ctx query` that block before working, so the calibration lands in *context* — the only layer that reliably overrides a trained default — instead of staying filed away.

State the *desired* behavior rather than the unwanted one (naming the bad behavior re-evokes it). This isn't about disabling safety — it's about re-aiming a calibration meant for someone else, and keeping that aim across sessions.

## Reference: the calibration this project runs on

ctx itself is built by AI agents against a published RLHF-warnings calibration — 22 axes of the failure modes above, with concrete before/after exemplars. It is the methodology reference behind this project's way of working:

**[gottz.de/warnings.md](https://gottz.de/warnings.md)** — the public mirror of the 22-axis calibration.
