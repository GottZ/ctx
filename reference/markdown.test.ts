/**
 * Markdown Stream Parser — Defect Tests
 *
 * Tests against common LLM markdown defects:
 * - Incomplete/unclosed syntax
 * - Streaming boundary edge cases
 * - False positive detection
 * - Unicode/emoji handling
 * - Nested structure edge cases
 * - SDK streaming quirks
 */

import {
  renderMarkdown,
  StreamingMarkdownParser,
  parseSSEFrames,
  StreamAccumulator,
  stripPromptXML,
  hasMarkdownSyntax,
  stripAnsi,
  hyperlink,
} from "./markdown";

let pass = 0;
let fail = 0;
const failures: string[] = [];

function assert(name: string, condition: boolean, detail?: string) {
  if (condition) {
    pass++;
    console.log(`  ✓ ${name}`);
  } else {
    fail++;
    const msg = detail ? `${name} — ${detail}` : name;
    failures.push(msg);
    console.log(`  ✗ ${msg}`);
  }
}

function assertContains(name: string, output: string, expected: string) {
  const clean = stripAnsi(output);
  assert(name, clean.includes(expected), `expected "${expected}" in "${clean.slice(0, 200)}"`);
}

function assertNotContains(name: string, output: string, unexpected: string) {
  const clean = stripAnsi(output);
  assert(name, !clean.includes(unexpected), `unexpected "${unexpected}" found in "${clean.slice(0, 200)}"`);
}

function assertEmpty(name: string, output: string) {
  const clean = stripAnsi(output).trim();
  assert(name, clean.length === 0, `expected empty but got "${clean.slice(0, 100)}"`);
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 1. Strikethrough / Tilde Quirk ===");
// ═══════════════════════════════════════════════════════════════════

{
  const out = renderMarkdown("The value is ~100 items");
  assertContains("Tilde preserved as text", out, "~100");
  assertNotContains("No strikethrough rendering", out, "~~");
}

{
  const out = renderMarkdown("Performance improved ~~significantly~~ somewhat");
  assertContains("Double tilde not parsed as strikethrough", out, "~~significantly~~");
}

{
  const out = renderMarkdown("Range: ~50-~100 requests/sec");
  assertContains("Multiple tildes in approximations", out, "~50");
  assertContains("Second tilde preserved", out, "~100");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 2. HTML Stripping ===");
// ═══════════════════════════════════════════════════════════════════

{
  const out = renderMarkdown("Before <details><summary>Click</summary>Hidden</details> After");
  assertNotContains("details tag stripped", out, "<details>");
  assertNotContains("summary tag stripped", out, "<summary>");
}

{
  const out = renderMarkdown("Line1<br>Line2<br/>Line3");
  assertNotContains("br tags stripped", out, "<br>");
  assertNotContains("self-closing br stripped", out, "<br/>");
}

{
  const out = renderMarkdown("<div class='dangerous'>XSS attempt</div>");
  assertNotContains("div with class stripped", out, "<div");
  assertNotContains("closing div stripped", out, "</div>");
}

{
  const out = renderMarkdown("Use `<script>` tags carefully");
  assertContains("HTML in inline code preserved", out, "<script>");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 3. Prompt XML Tag Stripping ===");
// ═══════════════════════════════════════════════════════════════════

{
  const out = renderMarkdown("<commit_analysis>internal stuff</commit_analysis>\nVisible text");
  assertContains("Visible text survives", out, "Visible text");
  assertNotContains("commit_analysis stripped", out, "internal stuff");
}

{
  const out = renderMarkdown("<context>system context</context>User content<function_analysis>fn info</function_analysis>");
  assertContains("User content survives", out, "User content");
  assertNotContains("context tag stripped", out, "system context");
  assertNotContains("function_analysis stripped", out, "fn info");
}

{
  // Non-prompt XML should survive (not in the strip list)
  const out = renderMarkdown("<custom_tag>should survive</custom_tag>");
  // HTML tags get stripped by the HTML handler, but the content should survive
  assertContains("Custom tag content survives", out, "should survive");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 4. Unclosed / Malformed Code Blocks ===");
// ═══════════════════════════════════════════════════════════════════

{
  const out = renderMarkdown("```python\nprint('hello')\n```");
  assertContains("Closed code block renders", out, "print('hello')");
  assertContains("Language tag present", out, "python");
}

{
  // Unclosed code block — marked treats as single token
  const out = renderMarkdown("```\nsome code\nmore code");
  assertContains("Unclosed fence still renders content", out, "some code");
}

{
  const out = renderMarkdown("Use `backtick` and ``double backtick`` inline");
  assertContains("Single backtick inline code", out, "backtick");
  assertContains("Double backtick inline code", out, "double backtick");
}

{
  // Backtick inside code
  const out = renderMarkdown("Run `` `echo hello` `` in bash");
  assertContains("Nested backtick in code", out, "`echo hello`");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 5. Heading Rendering ===");
// ═══════════════════════════════════════════════════════════════════

{
  const out = renderMarkdown("# Top Level");
  assertContains("H1 renders text", out, "Top Level");
  // H1 should have bold+italic+underline (ANSI codes)
  assert("H1 has bold", out.includes("\x1b[1m"), "expected bold ANSI code");
  assert("H1 has italic", out.includes("\x1b[3m"), "expected italic ANSI code");
  assert("H1 has underline", out.includes("\x1b[4m"), "expected underline ANSI code");
}

{
  const out = renderMarkdown("## Second Level");
  assertContains("H2 renders text", out, "Second Level");
  assert("H2 has bold", out.includes("\x1b[1m"), "expected bold");
  assert("H2 no underline", !out.includes("\x1b[4m"), "H2 should not have underline");
}

{
  // Hash in non-heading context
  const out = renderMarkdown("Issue #42 is important");
  assertContains("Hash in text not heading", out, "#42");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 6. List Depth Numbering ===");
// ═══════════════════════════════════════════════════════════════════

{
  const out = renderMarkdown("1. First\n2. Second\n3. Third");
  const clean = stripAnsi(out);
  assertContains("Ordered list item 1", clean, "1.");
  assertContains("Ordered list item 2", clean, "2.");
}

{
  const out = renderMarkdown("- Alpha\n- Beta\n- Gamma");
  const clean = stripAnsi(out);
  assertContains("Unordered uses hyphen", clean, "- Alpha");
}

{
  // Mixed markers should all become hyphens
  const md = "* Star item\n+ Plus item\n- Dash item";
  const out = renderMarkdown(md);
  const clean = stripAnsi(out);
  assertContains("Star becomes hyphen", clean, "- Star item");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 7. Blockquotes ===");
// ═══════════════════════════════════════════════════════════════════

{
  const out = renderMarkdown("> This is quoted");
  assertContains("Blockquote has bar", out, "\u258e");
  assertContains("Blockquote has content", stripAnsi(out), "This is quoted");
}

{
  const out = renderMarkdown("> Line 1\n>\n> Line 3");
  const clean = stripAnsi(out);
  assertContains("Multi-line blockquote first", clean, "Line 1");
  assertContains("Multi-line blockquote third", clean, "Line 3");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 8. Table Rendering ===");
// ═══════════════════════════════════════════════════════════════════

{
  const md = "| Col1 | Col2 |\n|------|------|\n| A    | B    |";
  const out = renderMarkdown(md);
  const clean = stripAnsi(out);
  assertContains("Table has box chars", clean, "┌");
  assertContains("Table has header", clean, "Col1");
  assertContains("Table has data", clean, "A");
}

{
  const md = "| Left | Center | Right |\n|:-----|:------:|------:|\n| L | C | R |";
  const out = renderMarkdown(md);
  const clean = stripAnsi(out);
  assertContains("Aligned table renders", clean, "Left");
}

{
  // Empty cells
  const md = "| A | B |\n|---|---|\n|   | X |";
  const out = renderMarkdown(md);
  assertContains("Empty cell table renders", stripAnsi(out), "X");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 9. Link Handling ===");
// ═══════════════════════════════════════════════════════════════════

{
  const out = renderMarkdown("[Click here](https://example.com)");
  assert("Link has OSC 8 start", out.includes("\x1b]8;;"), "expected OSC 8 sequence");
  assertContains("Link text rendered", stripAnsi(out), "Click here");
}

{
  const out = renderMarkdown("Contact: [email](mailto:test@example.com)");
  assertContains("Mailto becomes plain text", stripAnsi(out), "test@example.com");
  assertNotContains("No mailto: prefix", stripAnsi(out), "mailto:");
}

{
  // GitHub issue reference linkification
  const out = renderMarkdown("See anthropics/claude-code#100 for details");
  assert("Issue ref has OSC 8", out.includes("\x1b]8;;"), "expected hyperlink");
  assertContains("Issue ref text", stripAnsi(out), "anthropics/claude-code#100");
}

{
  // Bare #NNN should NOT be linkified (quirk: unreliable repo guess)
  const out = renderMarkdown("Fix #42 is important");
  assert("Bare #NNN not linkified", !out.includes("\x1b]8;;"), "should not have hyperlink for bare #42");
}

{
  // Issue ref INSIDE a link should not be double-linkified
  const out = renderMarkdown("[anthropics/claude-code#100](https://github.com/anthropics/claude-code/issues/100)");
  const osc8Count = (out.match(/\x1b\]8;;/g) || []).length;
  assert("No nested OSC 8", osc8Count <= 2, `expected ≤2 OSC 8 sequences but got ${osc8Count}`);
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 10. Emphasis Edge Cases ===");
// ═══════════════════════════════════════════════════════════════════

{
  const out = renderMarkdown("This is **bold** text");
  assert("Bold has ANSI bold", out.includes("\x1b[1m"), "expected bold code");
  assertContains("Bold text content", stripAnsi(out), "bold");
}

{
  const out = renderMarkdown("This is *italic* text");
  assert("Italic has ANSI italic", out.includes("\x1b[3m"), "expected italic code");
}

{
  // Asterisks in math/multiplication — should not be emphasis
  const out = renderMarkdown("Calculate 5 * 3 * 2 = 30");
  assertContains("Math asterisks preserved", stripAnsi(out), "5 * 3 * 2");
}

{
  // Underscores in identifiers — should not be emphasis
  const out = renderMarkdown("Use my_variable_name in the code");
  assertContains("Underscores in identifiers", stripAnsi(out), "my_variable_name");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 11. Horizontal Rules ===");
// ═══════════════════════════════════════════════════════════════════

{
  const out = renderMarkdown("Above\n\n---\n\nBelow");
  assertContains("HR renders as dashes", stripAnsi(out), "---");
  assertContains("Content above HR", stripAnsi(out), "Above");
  assertContains("Content below HR", stripAnsi(out), "Below");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 12. Fast Path Detection ===");
// ═══════════════════════════════════════════════════════════════════

{
  assert("Plain text: no markdown", !hasMarkdownSyntax("Hello world"), "should not detect markdown");
  assert("Has heading", hasMarkdownSyntax("# Heading"), "should detect #");
  assert("Has bold", hasMarkdownSyntax("**bold**"), "should detect *");
  assert("Has code", hasMarkdownSyntax("`code`"), "should detect backtick");
  assert("Has table", hasMarkdownSyntax("| col |"), "should detect pipe");
  assert("Has list", hasMarkdownSyntax("1. item"), "should detect ordered list");
  assert("Long plain text", !hasMarkdownSyntax("a".repeat(1000)), "1000 chars no markdown");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 13. Streaming Parser ===");
// ═══════════════════════════════════════════════════════════════════

{
  const parser = new StreamingMarkdownParser();

  // Feed heading in chunks
  let out = parser.feed("# Hel");
  out = parser.feed("lo World\n\n");
  out = parser.feed("Some text after.");
  const final = parser.finish();

  assertContains("Streaming: heading complete", stripAnsi(final), "Hello World");
  assertContains("Streaming: text after heading", stripAnsi(final), "Some text after.");
}

{
  const parser = new StreamingMarkdownParser();

  // Incomplete code block during streaming
  let out = parser.feed("```python\n");
  out = parser.feed("def hello():\n");
  out = parser.feed("    print('hi')\n");
  // Code fence still unclosed — should be in unstable suffix
  assertContains("Streaming: partial code visible", stripAnsi(out), "def hello()");

  // Close the fence
  out = parser.feed("```\n\nDone.");
  const final = parser.finish();
  assertContains("Streaming: code block closed", stripAnsi(final), "def hello()");
  assertContains("Streaming: text after code", stripAnsi(final), "Done.");
}

{
  const parser = new StreamingMarkdownParser();

  // Feed table row by row
  parser.feed("| A | B |\n");
  parser.feed("|---|---|\n");
  parser.feed("| 1 | 2 |\n");
  parser.feed("\nAfter table.");
  const final = parser.finish();
  assertContains("Streaming: table data", stripAnsi(final), "1");
  assertContains("Streaming: post-table text", stripAnsi(final), "After table");
}

{
  // Streaming with XML tags that get stripped mid-stream
  const parser = new StreamingMarkdownParser();
  parser.feed("<context>hidden</context>");
  const out = parser.feed("Visible");
  assertContains("Streaming: XML stripped, text visible", stripAnsi(out), "Visible");
  assertNotContains("Streaming: XML content hidden", stripAnsi(out), "hidden");
}

{
  // Reset test
  const parser = new StreamingMarkdownParser();
  parser.feed("# First message");
  parser.finish();
  parser.reset();
  parser.feed("# Second message");
  const final = parser.finish();
  assertContains("Streaming: reset works", stripAnsi(final), "Second message");
  assertNotContains("Streaming: first message gone", stripAnsi(final), "First message");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 14. SSE Frame Parser ===");
// ═══════════════════════════════════════════════════════════════════

{
  const { frames, remaining } = parseSSEFrames("event: message\ndata: hello\n\n");
  assert("SSE: one complete frame", frames.length === 1, `got ${frames.length}`);
  assert("SSE: event parsed", frames[0].event === "message", `got ${frames[0].event}`);
  assert("SSE: data parsed", frames[0].data === "hello", `got ${frames[0].data}`);
  assert("SSE: no remaining", remaining === "", `got "${remaining}"`);
}

{
  const { frames, remaining } = parseSSEFrames("data: part1\n\ndata: incomp");
  assert("SSE: one complete + incomplete", frames.length === 1, `got ${frames.length}`);
  assert("SSE: remaining buffered", remaining === "data: incomp", `got "${remaining}"`);
}

{
  // Multi-line data
  const { frames } = parseSSEFrames("data: line1\ndata: line2\n\n");
  assert("SSE: multi-line data joined", frames[0].data === "line1\nline2", `got "${frames[0].data}"`);
}

{
  // ID field
  const { frames } = parseSSEFrames("id: 42\ndata: test\n\n");
  assert("SSE: id parsed", frames[0].id === "42", `got ${frames[0].id}`);
}

{
  // Empty frames filtered
  const { frames } = parseSSEFrames("\n\n\n\ndata: real\n\n");
  assert("SSE: empty frames filtered", frames.length === 1, `got ${frames.length}`);
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 15. SDK Stream Accumulator ===");
// ═══════════════════════════════════════════════════════════════════

{
  const acc = new StreamAccumulator();

  // Simulate text block streaming
  acc.onBlockStart(0, "text");
  const r1 = acc.onDelta(0, "text_delta", "Hello ");
  assert("Accumulator: first delta returns string", typeof r1 === "string", `got ${typeof r1}`);

  const r2 = acc.onDelta(0, "text_delta", "**world**");
  assert("Accumulator: second delta returns string", typeof r2 === "string", `got ${typeof r2}`);
  assertContains("Accumulator: bold rendered", stripAnsi(r2!), "world");

  const final = acc.onBlockStop(0);
  assertContains("Accumulator: final has content", stripAnsi(final), "Hello");
}

{
  const acc = new StreamAccumulator();

  // Thinking block — deltas should not render markdown
  acc.onBlockStart(0, "thinking");
  const r = acc.onDelta(0, "thinking_delta", "Let me think about **this**...");
  assert("Accumulator: thinking delta returns null", r === null, `got ${r}`);
  assert("Accumulator: thinking accumulated", acc.blocks[0].thinking.includes("think about"), `got ${acc.blocks[0].thinking}`);
}

{
  const acc = new StreamAccumulator();

  // Tool use — JSON accumulation
  acc.onBlockStart(0, "tool_use");
  acc.onDelta(0, "input_json_delta", '{"file_pa');
  acc.onDelta(0, "input_json_delta", 'th": "/tmp/test"}');
  assert("Accumulator: JSON accumulated", acc.blocks[0].input === '{"file_path": "/tmp/test"}', `got ${acc.blocks[0].input}`);
}

{
  const acc = new StreamAccumulator();

  // Signature quirk — should be initialized even without delta
  acc.onBlockStart(0, "thinking");
  assert("Accumulator: signature initialized", acc.blocks[0].signature === "", `got "${acc.blocks[0].signature}"`);
}

{
  const acc = new StreamAccumulator();

  // Unknown delta type — should return null
  acc.onBlockStart(0, "text");
  const r = acc.onDelta(0, "unknown_delta_type", "data");
  assert("Accumulator: unknown delta returns null", r === null, `got ${r}`);
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 16. Mixed / Complex Documents ===");
// ═══════════════════════════════════════════════════════════════════

{
  const md = `# Project Status

## Overview

The system is running at ~95% capacity with **450+ blocks**.

> Important: Write Guard thresholds were adjusted.

### Details

| Metric | Value |
|--------|-------|
| Blocks | 450   |
| Links  | 44    |

1. First point
2. Second point
   - Sub item
   - Another sub

\`\`\`go
func main() {
    fmt.Println("hello")
}
\`\`\`

See anthropics/claude-code#100 for reference.

---

*End of report.*`;

  const out = renderMarkdown(md);
  const clean = stripAnsi(out);

  assertContains("Complex: heading", clean, "Project Status");
  assertContains("Complex: tilde preserved", clean, "~95%");
  assertContains("Complex: bold", clean, "450+ blocks");
  assertContains("Complex: blockquote", clean, "Write Guard");
  assertContains("Complex: table data", clean, "450");
  assertContains("Complex: ordered list", clean, "1.");
  assertContains("Complex: sub list", clean, "Sub item");
  assertContains("Complex: code block", clean, "fmt.Println");
  assertContains("Complex: issue ref", clean, "anthropics/claude-code#100");
  assertContains("Complex: HR", clean, "---");
  assertContains("Complex: italic", clean, "End of report");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 17. Escape Sequences ===");
// ═══════════════════════════════════════════════════════════════════

{
  const out = renderMarkdown("Use \\*asterisks\\* literally");
  assertContains("Escaped asterisks", stripAnsi(out), "*asterisks*");
}

{
  const out = renderMarkdown("Path: C:\\\\Users\\\\GottZ");
  assertContains("Escaped backslashes", stripAnsi(out), "C:\\Users\\GottZ");
}

// ═══════════════════════════════════════════════════════════════════
console.log("\n=== 18. Edge Cases: Empty / Whitespace ===");
// ═══════════════════════════════════════════════════════════════════

{
  const out = renderMarkdown("");
  assert("Empty string renders empty", stripAnsi(out).trim() === "", `got "${stripAnsi(out)}"`);
}

{
  const out = renderMarkdown("   \n\n   \n");
  assert("Whitespace-only renders empty-ish", stripAnsi(out).trim().length === 0, `got "${stripAnsi(out).trim()}"`);
}

{
  const out = renderMarkdown("Normal text\n\n\n\n\nWith many blank lines");
  assertContains("Multiple blanks: first part", stripAnsi(out), "Normal text");
  assertContains("Multiple blanks: second part", stripAnsi(out), "many blank lines");
}

// ═══════════════════════════════════════════════════════════════════
// Summary
// ═══════════════════════════════════════════════════════════════════

console.log(`\n${"=".repeat(60)}`);
console.log(`  RESULTS: ${pass} passed, ${fail} failed (${pass + fail} total)`);
console.log(`${"=".repeat(60)}`);

if (failures.length > 0) {
  console.log("\n  FAILURES:");
  for (const f of failures) {
    console.log(`    ✗ ${f}`);
  }
}

console.log();
process.exit(fail > 0 ? 1 : 0);
