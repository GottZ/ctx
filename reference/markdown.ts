/**
 * LLM Markdown Stream Parser — Reference Implementation
 *
 * Based on Claude Code source analysis (Session 21, 2026-04-07).
 * Implements all documented quirks for parsing LLM-generated markdown.
 *
 * Dependencies: marked (GFM tokenizer)
 * ctx block: 019d68a3-39a3-7daa-9b00-f39bd74e60d0
 *
 * @author GottZ (hire@gottz.de)
 * @license MPL-2.0
 */

import { marked, type Token, type Tokens } from "marked";

// ─── Configuration ──────────────────────────────────────────────────

/** Quirk: Strikethrough disabled — models use ~ for approximation (~100). */
let configured = false;
function configureMarked(): void {
  if (configured) return;
  configured = true;
  marked.use({
    tokenizer: { del() { return undefined; } },
  });
}

// ─── ANSI Helpers ───────────────────────────────────────────────────

const ESC = "\x1b[";
const RESET = `${ESC}0m`;
const BOLD = `${ESC}1m`;
const DIM = `${ESC}2m`;
const ITALIC = `${ESC}3m`;
const UNDERLINE = `${ESC}4m`;
const BLUE = `${ESC}34m`;

const ansi = {
  bold: (s: string) => `${BOLD}${s}${RESET}`,
  dim: (s: string) => `${DIM}${s}${RESET}`,
  italic: (s: string) => `${ITALIC}${s}${RESET}`,
  underline: (s: string) => `${UNDERLINE}${s}${RESET}`,
  blue: (s: string) => `${BLUE}${s}${RESET}`,
  boldItalicUnderline: (s: string) => `${BOLD}${ITALIC}${UNDERLINE}${s}${RESET}`,
};

// ─── OSC 8 Hyperlinks ───────────────────────────────────────────────

/**
 * Terminal hyperlink via OSC 8.
 * Quirk: Use only basic ANSI blue — RGB colors break across line wraps
 * when combined with OSC 8 (wrap-ansi bug).
 */
function hyperlink(url: string, text?: string): string {
  const display = text ?? url;
  return `\x1b]8;;${url}\x07${ansi.blue(display)}\x1b]8;;\x07`;
}

/**
 * GitHub issue reference linkification.
 * Quirk: Bare #NNN removed (current-repo guess unreliable).
 * Quirk: Skip when inside a link token (nested OSC 8 breaks outer link).
 * Pattern avoids dots in owner segment (GitHub usernames: alphanumeric + hyphens).
 */
const ISSUE_RE = /(^|[^\w./-])([A-Za-z0-9][\w-]*\/[A-Za-z0-9][\w.-]*)#(\d+)\b/g;

function linkifyIssueRefs(text: string, insideLink: boolean): string {
  if (insideLink) return text;
  return text.replace(ISSUE_RE, (_, prefix, repo, num) =>
    `${prefix}${hyperlink(`https://github.com/${repo}/issues/${num}`, `${repo}#${num}`)}`
  );
}

// ─── List Numbering ─────────────────────────────────────────────────

/** Depth-based list numbering: arabic → letters → roman → arabic. */
function listNumber(index: number, depth: number): string {
  const n = index + 1;
  switch (depth) {
    case 0: case 1: return `${n}`;
    case 2: return toLetter(n);
    case 3: return toRoman(n);
    default: return `${n}`;
  }
}

function toLetter(n: number): string {
  let s = "";
  while (n > 0) { n--; s = String.fromCharCode(97 + (n % 26)) + s; n = Math.floor(n / 26); }
  return s;
}

function toRoman(n: number): string {
  const vals = [1000, 900, 500, 400, 100, 90, 50, 40, 10, 9, 5, 4, 1];
  const syms = ["m", "cm", "d", "cd", "c", "xc", "l", "xl", "x", "ix", "v", "iv", "i"];
  let s = "";
  for (let i = 0; i < vals.length; i++) {
    while (n >= vals[i]) { s += syms[i]; n -= vals[i]; }
  }
  return s;
}

// ─── Token Formatting ───────────────────────────────────────────────

const BLOCKQUOTE_BAR = "\u258e"; // ▎ Left one-quarter block
const EOL = "\n";

/**
 * Recursive token formatter. Converts marked tokens to ANSI strings.
 *
 * @param token      - marked token
 * @param listDepth  - current list nesting depth (for numbering + indent)
 * @param listIndex  - current item index in parent list (for ordered numbering)
 * @param insideLink - true when rendering children of a link token
 */
function formatToken(
  token: Token,
  listDepth: number = 0,
  listIndex: number | null = null,
  insideLink: boolean = false,
): string {
  switch (token.type) {
    // ── Block elements ────────────────────────────────────────────

    case "heading": {
      const t = token as Tokens.Heading;
      const text = t.tokens.map((c) => formatToken(c, listDepth, null, insideLink)).join("");
      // H1: bold + italic + underline. H2+: bold only.
      const styled = t.depth === 1 ? ansi.boldItalicUnderline(text) : ansi.bold(text);
      return styled + EOL + EOL;
    }

    case "paragraph": {
      const t = token as Tokens.Paragraph;
      const text = t.tokens.map((c) => formatToken(c, listDepth, null, insideLink)).join("");
      return text + EOL;
    }

    case "blockquote": {
      const t = token as Tokens.Blockquote;
      const inner = t.tokens.map((c) => formatToken(c, listDepth, null, insideLink)).join("");
      return inner
        .split(EOL)
        .map((line) =>
          line.trim() ? `${ansi.dim(BLOCKQUOTE_BAR)} ${ansi.italic(line)}` : ""
        )
        .join(EOL) + EOL;
    }

    case "code": {
      const t = token as Tokens.Code;
      // Language from token.lang, validated at render time.
      // Syntax highlighting omitted here — plug in highlight.js / cli-highlight.
      const lang = t.lang ?? "plaintext";
      return `\`\`\`${lang}${EOL}${t.text}${EOL}\`\`\`${EOL}`;
    }

    case "hr":
      return `---${EOL}`;

    case "list": {
      const t = token as Tokens.List;
      return t.items
        .map((item, i) => {
          const indent = "  ".repeat(listDepth);
          const bullet = t.ordered ? `${listNumber(i, listDepth)}.` : "-";
          const content = (item as Tokens.ListItem).tokens
            .map((c) => formatToken(c, listDepth + 1, i, insideLink))
            .join("")
            .replace(/\n$/, ""); // Remove trailing EOL from inner paragraph
          return `${indent}${bullet} ${content}`;
        })
        .join(EOL) + EOL;
    }

    case "table": {
      return formatTable(token as Tokens.Table);
    }

    case "space":
      return EOL;

    // ── Inline elements ───────────────────────────────────────────

    case "strong": {
      const t = token as Tokens.Strong;
      const text = t.tokens.map((c) => formatToken(c, listDepth, null, insideLink)).join("");
      return ansi.bold(text);
    }

    case "em": {
      const t = token as Tokens.Em;
      const text = t.tokens.map((c) => formatToken(c, listDepth, null, insideLink)).join("");
      return ansi.italic(text);
    }

    case "codespan": {
      const t = token as Tokens.Codespan;
      // Inline code: theme color, no syntax highlighting.
      return `\`${t.text}\``;
    }

    case "link": {
      const t = token as Tokens.Link;
      // Quirk: mailto links → plain text.
      if (t.href.startsWith("mailto:")) {
        return t.href.replace("mailto:", "");
      }
      const text = t.tokens.map((c) => formatToken(c, listDepth, null, true)).join("");
      return text !== t.href ? hyperlink(t.href, text) : hyperlink(t.href);
    }

    case "image": {
      const t = token as Tokens.Image;
      return `[${t.text}](${t.href})`;
    }

    case "text": {
      const t = token as Tokens.Text;
      let text = t.text;
      // Linkify GitHub issue references (owner/repo#123).
      text = linkifyIssueRefs(text, insideLink);
      // Handle nested tokens (e.g., text inside list items).
      if ("tokens" in t && Array.isArray(t.tokens)) {
        return (t.tokens as Token[]).map((c) => formatToken(c, listDepth, null, insideLink)).join("");
      }
      return text;
    }

    case "br":
      return EOL;

    case "escape": {
      // Markdown escapes (\) etc) already processed by marked.
      const t = token as Tokens.Escape;
      return t.text;
    }

    // Quirk: HTML is completely stripped.
    case "html":
      return "";

    // Fallback: unknown token types rendered as raw text.
    default:
      return "raw" in token ? (token as { raw: string }).raw : "";
  }
}

// ─── Table Formatting ───────────────────────────────────────────────

/**
 * Table renderer with horizontal (box-drawing) and vertical (key-value) modes.
 * Quirk: SAFETY_MARGIN = 4 against resize flicker.
 * Quirk: Vertical fallback when terminal too narrow.
 */
const MIN_COL_WIDTH = 3;
const SAFETY_MARGIN = 4;

function formatTable(token: Tokens.Table): string {
  const termWidth = process.stdout.columns || 80;
  const cols = token.header.length;

  // Calculate column widths from content.
  const widths: number[] = new Array(cols).fill(MIN_COL_WIDTH);
  for (let c = 0; c < cols; c++) {
    const headerText = token.header[c].tokens.map((t) => formatToken(t)).join("");
    widths[c] = Math.max(widths[c], stripAnsi(headerText).length);
    for (const row of token.rows) {
      const cellText = row[c].tokens.map((t) => formatToken(t)).join("");
      widths[c] = Math.max(widths[c], stripAnsi(cellText).length);
    }
  }

  // Check if horizontal fits (cols + separators + padding).
  const totalWidth = widths.reduce((a, b) => a + b, 0) + cols * 3 + 1;
  if (totalWidth + SAFETY_MARGIN > termWidth) {
    return formatTableVertical(token);
  }

  return formatTableHorizontal(token, widths);
}

function formatTableHorizontal(token: Tokens.Table, widths: number[]): string {
  const cols = token.header.length;
  const aligns = token.align ?? new Array(cols).fill(null);
  const lines: string[] = [];

  const hr = (left: string, mid: string, right: string) =>
    `${left}${widths.map((w) => "─".repeat(w + 2)).join(mid)}${right}`;

  lines.push(hr("┌", "┬", "┐"));

  // Header row.
  const headerCells = token.header.map((cell, i) => {
    const text = cell.tokens.map((t) => formatToken(t)).join("");
    return padAligned(text, widths[i], "center");
  });
  lines.push(`│ ${headerCells.join(" │ ")} │`);
  lines.push(hr("├", "┼", "┤"));

  // Data rows.
  for (const row of token.rows) {
    const cells = row.map((cell, i) => {
      const text = cell.tokens.map((t) => formatToken(t)).join("");
      return padAligned(text, widths[i], aligns[i] ?? "left");
    });
    lines.push(`│ ${cells.join(" │ ")} │`);
  }

  lines.push(hr("└", "┴", "┘"));
  return lines.join(EOL) + EOL;
}

function formatTableVertical(token: Tokens.Table): string {
  const lines: string[] = [];
  const headers = token.header.map((h) => h.tokens.map((t) => formatToken(t)).join(""));

  for (const row of token.rows) {
    for (let i = 0; i < headers.length; i++) {
      const value = row[i].tokens.map((t) => formatToken(t)).join("");
      lines.push(`${ansi.bold(headers[i])}: ${value}`);
    }
    lines.push("---");
  }

  return lines.join(EOL) + EOL;
}

function padAligned(text: string, width: number, align: string): string {
  const stripped = stripAnsi(text);
  const pad = Math.max(0, width - stripped.length);
  switch (align) {
    case "center": {
      const left = Math.floor(pad / 2);
      return " ".repeat(left) + text + " ".repeat(pad - left);
    }
    case "right":
      return " ".repeat(pad) + text;
    default:
      return text + " ".repeat(pad);
  }
}

// ─── ANSI Strip ─────────────────────────────────────────────────────

const ANSI_RE = /\x1b\[[0-9;]*m|\x1b\]8;;[^\x07]*\x07/g;
function stripAnsi(s: string): string {
  return s.replace(ANSI_RE, "");
}

// ─── Token Cache (LRU) ─────────────────────────────────────────────

/**
 * LRU token cache. Messages are immutable — same content = same tokens.
 * Quirk: Max 500 entries. Keyed by content hash, not full string.
 */
const TOKEN_CACHE_MAX = 500;
const tokenCache = new Map<string, Token[]>();

function cachedLex(content: string): Token[] {
  const key = simpleHash(content);
  const cached = tokenCache.get(key);
  if (cached) {
    // Promote to MRU.
    tokenCache.delete(key);
    tokenCache.set(key, cached);
    return cached;
  }

  const tokens = marked.lexer(content);
  tokenCache.set(key, tokens);

  // Evict oldest if over capacity.
  if (tokenCache.size > TOKEN_CACHE_MAX) {
    const oldest = tokenCache.keys().next().value;
    if (oldest !== undefined) tokenCache.delete(oldest);
  }

  return tokens;
}

function simpleHash(s: string): string {
  let h = 0;
  for (let i = 0; i < s.length; i++) {
    h = ((h << 5) - h + s.charCodeAt(i)) | 0;
  }
  return h.toString(36);
}

// ─── Fast Path: Markdown Syntax Detection ───────────────────────────

/**
 * Quick pre-check to skip marked.lexer() for plain text (~3ms saved).
 * Quirk: Only samples first 500 chars.
 */
const MD_MARKERS = /[#*`|[\]>\\~_-]|\n\n|\d+\. /;

function hasMarkdownSyntax(s: string): boolean {
  return MD_MARKERS.test(s.length > 500 ? s.slice(0, 500) : s);
}

// ─── XML Tag Stripping ──────────────────────────────────────────────

/**
 * Strip prompt-internal XML tags before markdown processing.
 * These are Claude-specific system tags, not user content.
 */
const PROMPT_XML_RE =
  /<(commit_analysis|context|function_analysis|pr_analysis)>.*?<\/\1>\n?/gs;

function stripPromptXML(content: string): string {
  return content.replace(PROMPT_XML_RE, "");
}

// ─── Static Rendering ───────────────────────────────────────────────

/**
 * Render markdown content to ANSI string (non-streaming).
 * Full pipeline: strip XML → detect syntax → lex → format.
 */
export function renderMarkdown(content: string): string {
  configureMarked();

  const stripped = stripPromptXML(content);

  // Fast path: no markdown syntax detected.
  if (!hasMarkdownSyntax(stripped)) {
    return stripped;
  }

  const tokens = cachedLex(stripped);
  return tokens.map((t) => formatToken(t)).join("").trim();
}

// ─── Streaming Parser ───────────────────────────────────────────────

/**
 * Streaming markdown parser with stable prefix / unstable suffix splitting.
 *
 * Architecture (from CC source):
 * - Split at last completed top-level block boundary.
 * - Stable prefix: finalized, never re-parsed.
 * - Unstable suffix: re-lexed per delta as it grows.
 * - Boundary only advances monotonically (never backward).
 * - marked.lexer() treats unclosed code fences as single token → safe boundary.
 * - Complexity: O(suffix length) per delta, not O(total).
 */
export class StreamingMarkdownParser {
  private accumulated = "";
  private stablePrefix = "";
  private stableRendered = "";

  /** Feed a new text delta from the stream. Returns full rendered output. */
  feed(delta: string): string {
    configureMarked();
    this.accumulated += delta;

    const stripped = stripPromptXML(this.accumulated);

    // Fast path.
    if (!hasMarkdownSyntax(stripped)) {
      return stripped;
    }

    // Quirk: If stripped doesn't start with our stable prefix, reset.
    // Happens when closing XML tags change the stripped output.
    if (!stripped.startsWith(this.stablePrefix)) {
      this.stablePrefix = "";
      this.stableRendered = "";
    }

    // Lex only the unstable suffix.
    const boundary = this.stablePrefix.length;
    const suffix = stripped.substring(boundary);
    const tokens = marked.lexer(suffix);

    // Find last content token — everything before it is now stable.
    let lastContentIdx = tokens.length - 1;
    while (lastContentIdx >= 0 && tokens[lastContentIdx].type === "space") {
      lastContentIdx--;
    }

    // Advance stable boundary.
    let advance = 0;
    for (let i = 0; i < lastContentIdx; i++) {
      advance += tokens[i].raw.length;
    }

    if (advance > 0) {
      // Newly stable tokens — render and append to stable output.
      const newStableTokens = tokens.slice(0, lastContentIdx);
      const newStableText = newStableTokens
        .map((t) => formatToken(t))
        .join("");

      this.stablePrefix = stripped.substring(0, boundary + advance);
      this.stableRendered += newStableText;
    }

    // Render unstable suffix (the growing last block).
    const unstableSuffix = stripped.substring(this.stablePrefix.length);
    let unstableRendered = "";
    if (unstableSuffix.trim()) {
      const unstableTokens = marked.lexer(unstableSuffix);
      unstableRendered = unstableTokens
        .map((t) => formatToken(t))
        .join("");
    }

    return (this.stableRendered + unstableRendered).trim();
  }

  /** Get the final rendered output. Call after stream completes. */
  finish(): string {
    return renderMarkdown(this.accumulated);
  }

  /** Reset parser state for a new message. */
  reset(): void {
    this.accumulated = "";
    this.stablePrefix = "";
    this.stableRendered = "";
  }
}

// ─── SSE Frame Parser ───────────────────────────────────────────────

/**
 * SSE (Server-Sent Events) frame parser.
 * Quirk: Delimiter is \n\n (double newline). Incomplete frames buffered.
 */
export interface SSEFrame {
  event?: string;
  data: string;
  id?: string;
}

export function parseSSEFrames(buffer: string): {
  frames: SSEFrame[];
  remaining: string;
} {
  const frames: SSEFrame[] = [];
  const parts = buffer.split("\n\n");

  // Last part is potentially incomplete — keep as remaining.
  const remaining = parts.pop() ?? "";

  for (const part of parts) {
    if (!part.trim()) continue;

    const frame: SSEFrame = { data: "" };
    const lines = part.split("\n");

    for (const line of lines) {
      if (line.startsWith("event:")) {
        frame.event = line.substring(6).trim();
      } else if (line.startsWith("data:")) {
        frame.data += (frame.data ? "\n" : "") + line.substring(5).trim();
      } else if (line.startsWith("id:")) {
        frame.id = line.substring(3).trim();
      }
    }

    if (frame.data || frame.event) {
      frames.push(frame);
    }
  }

  return { frames, remaining };
}

// ─── SDK Streaming Accumulator ──────────────────────────────────────

/**
 * Content block accumulator for Anthropic SDK streaming.
 *
 * Quirk: SDK sometimes sends text in content_block_start AND again in
 * content_block_delta. Initialize text='' and ignore start payload.
 *
 * Quirk: SDK mutates content blocks directly. Clone at content_block_start
 * and accumulate manually for prompt cache consistency.
 *
 * Quirk: Initialize signature='' for thinking blocks even if
 * signature_delta never arrives.
 */
export interface ContentBlock {
  type: "text" | "tool_use" | "thinking";
  text: string;
  thinking: string;
  signature: string;
  input: string; // tool_use JSON accumulator
}

export class StreamAccumulator {
  blocks: ContentBlock[] = [];
  private parser = new StreamingMarkdownParser();

  /** Handle content_block_start — clone and initialize empty. */
  onBlockStart(index: number, type: ContentBlock["type"]): void {
    this.blocks[index] = {
      type,
      text: "",        // Quirk: ignore text from start event
      thinking: "",
      signature: type === "thinking" ? "" : "", // Quirk: always initialize
      input: "",
    };
  }

  /** Handle content_block_delta — accumulate into block. */
  onDelta(
    index: number,
    deltaType: string,
    content: string,
  ): string | null {
    const block = this.blocks[index];
    if (!block) return null;

    switch (deltaType) {
      case "text_delta":
        block.text += content;
        return this.parser.feed(block.text);

      case "thinking_delta":
        block.thinking += content;
        return null;

      case "signature_delta":
        block.signature = content;
        return null;

      case "input_json_delta":
        block.input += content;
        return null;

      default:
        return null;
    }
  }

  /** Handle content_block_stop — finalize rendering. */
  onBlockStop(index: number): string {
    const block = this.blocks[index];
    if (!block || block.type !== "text") return "";
    const result = this.parser.finish();
    this.parser.reset();
    return result;
  }
}

// ─── Exports ────────────────────────────────────────────────────────

export {
  configureMarked,
  formatToken,
  stripPromptXML,
  hasMarkdownSyntax,
  cachedLex,
  hyperlink,
  stripAnsi,
};
