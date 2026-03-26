#!/usr/bin/env bun
/**
 * Scientific Paper Ingestion Pipeline
 *
 * Extracts sections from a PDF, summarizes each via LLM, and upserts
 * into the context store as individual blocks (category: "papers").
 *
 * Usage:
 *   bun run ingest-paper.ts <pdf-path> [--title "Short Paper Title"] [--doi "10.xxx"] [--dry-run]
 *   bun run ingest-paper.ts ./papers/*.pdf --batch
 *
 * Dependencies: pdftotext (poppler-utils), Ollama, context-store webhook
 */
// ctx — Your AI's save game. By GottZ (github.com/GottZ/ctx/graphs/contributors)
// Paper ingestion pipeline for the GottZ 4-Way RRF knowledge store.
// Source: https://github.com/GottZ/ctx

import { $ } from "bun";
import { readFile, stat } from "fs/promises";
import { basename } from "path";
import { createHash } from "crypto";

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

const CTX_CONFIG_PATH = `${process.env.XDG_CONFIG_HOME ?? process.env.HOME + "/.config"}/ctx/config`;
const ctxConfig = Object.fromEntries(
  (await readFile(CTX_CONFIG_PATH, "utf-8")).split("\n")
    .filter(l => l && !l.startsWith("#"))
    .map(l => l.split("=").map(s => s.trim()) as [string, string])
);
const CTX_KEY = process.env.CTX_KEY ?? ctxConfig.CTX_KEY ?? "";
const WEBHOOK_BASE = process.env.CTX_BASE_URL ?? ctxConfig.CTX_BASE_URL ?? "http://localhost/webhook";
const OLLAMA_HOST = process.env.OLLAMA_HOST ?? "http://localhost:11434";
const OLLAMA_MODEL = "qwen3:4b-instruct";
const CATEGORY = "papers";
const MAX_BLOCK_CHARS = 1800; // target block size (leave room below 2K)
const MAX_SUMMARY_TOKENS = 400; // num_predict for summarization
const SCOPE = "private"; // default scope for paper blocks

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface Section {
  name: string;
  content: string;
  index: number;
}

interface PaperMeta {
  paper_id: string;
  title: string;
  doi?: string;
  authors?: string[];
  year?: number;
  journal?: string;
  source: string;
  ingest_version: number;
}

interface IngestResult {
  paper_id: string;
  title: string;
  sections: number;
  blocks_created: number;
  blob_stored: boolean;
  skipped: boolean;
  errors: string[];
}

// ---------------------------------------------------------------------------
// PDF Extraction (pdftotext)
// ---------------------------------------------------------------------------

async function extractText(pdfPath: string): Promise<string> {
  const result = await $`pdftotext -layout "${pdfPath}" -`.text();
  if (!result.trim()) {
    throw new Error(`pdftotext returned empty output for ${pdfPath}`);
  }
  return result;
}

// ---------------------------------------------------------------------------
// Paper ID (SHA-256 of first 4KB)
// ---------------------------------------------------------------------------

async function computePaperId(pdfPath: string): Promise<string> {
  const buf = await readFile(pdfPath);
  const chunk = buf.subarray(0, 4096);
  return createHash("sha256").update(chunk).digest("hex");
}

// ---------------------------------------------------------------------------
// Deduplication Check
// ---------------------------------------------------------------------------

async function paperExists(paperId: string): Promise<boolean> {
  try {
    const resp = await fetch(`${WEBHOOK_BASE}/context-search`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Context-Key": CTX_KEY,
      },
      body: JSON.stringify({
        category: CATEGORY,
        query: paperId,
        limit: 5,
      }),
    });

    const text = await resp.text();
    if (!text.trim()) return false; // empty response = no results

    const data = JSON.parse(text);
    if (!data.success || !data.results?.length) return false;
    return data.results.some((r: any) => r.metadata?.paper_id === paperId);
  } catch {
    // Network error or parse error — assume not exists
    console.warn("  Dedup check failed (network/parse error), proceeding with ingestion");
    return false;
  }
}

// ---------------------------------------------------------------------------
// LLM Calls (Ollama)
// ---------------------------------------------------------------------------

async function ollamaGenerate(system: string, prompt: string, maxTokens = 500): Promise<string> {
  const resp = await fetch(`${OLLAMA_HOST}/api/chat`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      model: OLLAMA_MODEL,
      messages: [
        { role: "system", content: system },
        { role: "user", content: prompt },
      ],
      stream: false,
      options: {
        temperature: 0,
        num_predict: maxTokens,
      },
    }),
  });
  const data = await resp.json() as any;
  return data.message?.content?.trim() || "";
}

// ---------------------------------------------------------------------------
// Section Extraction via LLM
// ---------------------------------------------------------------------------

const SECTION_SYSTEM = `<role>Scientific paper section extractor</role>
<task>
Split the given paper text into semantic sections.
Return ONLY a JSON array of objects with "name" and "content" fields.
Each section should correspond to a major heading in the paper.
</task>
<constraints>
1. Standard sections: Abstract, Introduction, Related Work, Methodology, Results, Discussion, Conclusion
2. If a section doesn't exist in the paper, omit it
3. Combine small subsections into their parent section
4. Strip references/bibliography entirely
5. Strip page headers, footers, page numbers
6. Keep table data as plain text
7. Maximum 8 sections per paper
8. Return ONLY valid JSON, no markdown fences, no explanation
</constraints>
<output_format>
[{"name":"Abstract","content":"..."},{"name":"Methodology","content":"..."}]
</output_format>`;

async function extractSections(rawText: string): Promise<Section[]> {
  // Truncate to ~30K chars to stay within Ollama context
  const truncated = rawText.slice(0, 30000);

  const response = await ollamaGenerate(
    SECTION_SYSTEM,
    truncated,
    4000 // sections can be long in aggregate
  );

  // Try to parse JSON from response
  let sections: Array<{ name: string; content: string }>;
  try {
    // Handle potential markdown code fences
    const jsonStr = response.replace(/```json?\n?/g, "").replace(/```/g, "").trim();
    sections = JSON.parse(jsonStr);
  } catch {
    console.warn("LLM section extraction failed, falling back to regex-based splitting");
    sections = regexSectionSplit(rawText);
  }

  // Validate and index
  return sections
    .filter((s) => s.name && s.content && s.content.length > 50)
    .map((s, i) => ({
      name: s.name,
      content: s.content,
      index: i,
    }));
}

// ---------------------------------------------------------------------------
// Fallback: Regex-Based Section Splitting
// ---------------------------------------------------------------------------

function regexSectionSplit(text: string): Array<{ name: string; content: string }> {
  // Common scientific paper headings (case-insensitive)
  const headingPattern =
    /^(?:\d+\.?\s*)?(Abstract|Introduction|Background|Related\s+Work|Literature\s+Review|Methodology|Methods?|Materials?\s+and\s+Methods?|Experimental?\s+Setup|Results?|Findings|Discussion|Analysis|Conclusion|Summary|Acknowledgements?|References?|Bibliography|Appendix)/im;

  const lines = text.split("\n");
  const sections: Array<{ name: string; content: string }> = [];
  let currentName = "Preamble";
  let currentLines: string[] = [];

  for (const line of lines) {
    const match = line.match(headingPattern);
    if (match && line.trim().length < 80) {
      // Looks like a heading
      if (currentLines.length > 0) {
        sections.push({
          name: currentName,
          content: currentLines.join("\n").trim(),
        });
      }
      currentName = match[1].trim();
      currentLines = [];
    } else {
      currentLines.push(line);
    }
  }

  // Last section
  if (currentLines.length > 0) {
    sections.push({
      name: currentName,
      content: currentLines.join("\n").trim(),
    });
  }

  // Filter out references/bibliography and preamble if too short
  return sections.filter(
    (s) =>
      !s.name.match(/^(References?|Bibliography|Acknowledgements?)$/i) &&
      !(s.name === "Preamble" && s.content.length < 100)
  );
}

// ---------------------------------------------------------------------------
// Section Summarization
// ---------------------------------------------------------------------------

const SUMMARY_SYSTEM = `<role>Scientific content summarizer</role>
<task>
Summarize the given section of a scientific paper.
Preserve key data points, findings, and technical details.
Output plain text, no markdown headings or bullet formatting.
</task>
<constraints>
1. Maximum 1500 characters
2. Preserve exact numbers, metrics, p-values, effect sizes
3. Preserve author claims and caveats
4. Do not add interpretation beyond what the text states
5. Write in the same language as the input
6. If the section is already concise (<1500 chars), return it cleaned up without summarizing
</constraints>`;

async function summarizeSection(section: Section): Promise<string> {
  // If already short enough, just clean it
  if (section.content.length <= MAX_BLOCK_CHARS) {
    return section.content.replace(/\s+/g, " ").trim();
  }

  const summary = await ollamaGenerate(
    SUMMARY_SYSTEM,
    `Section: ${section.name}\n\n${section.content.slice(0, 12000)}`,
    MAX_SUMMARY_TOKENS
  );

  if (!summary || summary.length < 50) {
    // Fallback: truncate with ellipsis
    return section.content.slice(0, MAX_BLOCK_CHARS).trim() + "...";
  }

  return summary;
}

// ---------------------------------------------------------------------------
// Metadata Extraction via LLM
// ---------------------------------------------------------------------------

const META_SYSTEM = `<role>Bibliographic metadata extractor</role>
<task>
Extract metadata from the beginning of a scientific paper.
Return ONLY a JSON object.
</task>
<constraints>
1. Return ONLY valid JSON, no explanation
2. Fields: title (string), authors (string array of "Last, First"), year (number), journal (string or null), doi (string or null)
3. If a field cannot be determined, use null
4. Title should be the full paper title, not abbreviated
</constraints>`;

async function extractMetadata(
  rawText: string,
  overrideTitle?: string,
  overrideDoi?: string
): Promise<Partial<PaperMeta>> {
  const firstPage = rawText.slice(0, 3000);

  const response = await ollamaGenerate(META_SYSTEM, firstPage, 300);

  let meta: any = {};
  try {
    const jsonStr = response.replace(/```json?\n?/g, "").replace(/```/g, "").trim();
    meta = JSON.parse(jsonStr);
  } catch {
    console.warn("Metadata extraction failed, using defaults");
  }

  return {
    title: overrideTitle || meta.title || "Untitled Paper",
    doi: overrideDoi || meta.doi || undefined,
    authors: meta.authors || undefined,
    year: meta.year || undefined,
    journal: meta.journal || undefined,
  };
}

// ---------------------------------------------------------------------------
// Context Store Upsert (via webhook)
// ---------------------------------------------------------------------------

async function upsertBlock(
  title: string,
  content: string,
  tags: string[],
  metadata: Record<string, unknown>
): Promise<boolean> {
  const resp = await fetch(`${WEBHOOK_BASE}/context-store`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Context-Key": CTX_KEY,
    },
    body: JSON.stringify({
      category: CATEGORY,
      title,
      content,
      tags,
      scope: SCOPE,
      metadata,
    }),
  });

  const data = await resp.json() as any;
  if (!data.success && !data.id) {
    console.error(`  Upsert failed for "${title}":`, data);
    return false;
  }
  return true;
}

// ---------------------------------------------------------------------------
// Blob Store (original PDF)
// ---------------------------------------------------------------------------

async function storePdfBlob(
  pdfPath: string,
  paperTitle: string,
  paperId: string
): Promise<boolean> {
  const buf = await readFile(pdfPath);
  const base64 = buf.toString("base64");
  const filename = basename(pdfPath);

  const resp = await fetch(`${WEBHOOK_BASE}/blob-store`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Context-Key": CTX_KEY,
    },
    body: JSON.stringify({
      category: CATEGORY,
      title: `${paperTitle} — PDF`,
      filename,
      mime_type: "application/pdf",
      file: base64,
      scope: SCOPE,
      tags: ["paper", "pdf", "source"],
      metadata: { paper_id: paperId },
    }),
  });

  const data = await resp.json() as any;
  return !!data.success || !!data.id;
}

// ---------------------------------------------------------------------------
// Main Ingestion Pipeline
// ---------------------------------------------------------------------------

async function ingestPaper(
  pdfPath: string,
  options: {
    title?: string;
    doi?: string;
    dryRun?: boolean;
    force?: boolean;
  } = {}
): Promise<IngestResult> {
  const errors: string[] = [];

  console.log(`\n${"=".repeat(60)}`);
  console.log(`Processing: ${pdfPath}`);
  console.log(`${"=".repeat(60)}`);

  // 1. Compute paper ID
  const paperId = await computePaperId(pdfPath);
  console.log(`  Paper ID: ${paperId.slice(0, 16)}...`);

  // 2. Dedup check
  if (!options.force) {
    const exists = await paperExists(paperId);
    if (exists) {
      console.log("  SKIPPED: Paper already ingested (use --force to re-ingest)");
      return {
        paper_id: paperId,
        title: options.title || "unknown",
        sections: 0,
        blocks_created: 0,
        blob_stored: false,
        skipped: true,
        errors,
      };
    }
  }

  // 3. Extract raw text
  console.log("  Extracting text with pdftotext...");
  const rawText = await extractText(pdfPath);
  console.log(`  Raw text: ${rawText.length} chars`);

  // 4. Extract metadata
  console.log("  Extracting metadata...");
  const meta = await extractMetadata(rawText, options.title, options.doi);
  const shortTitle = truncateTitle(meta.title || "Untitled");
  console.log(`  Title: ${shortTitle}`);
  console.log(`  Authors: ${meta.authors?.join("; ") || "unknown"}`);
  console.log(`  Year: ${meta.year || "unknown"}`);

  // 5. Extract sections
  console.log("  Splitting into sections...");
  const sections = await extractSections(rawText);
  console.log(`  Found ${sections.length} sections: ${sections.map((s) => s.name).join(", ")}`);

  if (sections.length === 0) {
    errors.push("No sections extracted");
    return {
      paper_id: paperId,
      title: shortTitle,
      sections: 0,
      blocks_created: 0,
      blob_stored: false,
      skipped: false,
      errors,
    };
  }

  if (options.dryRun) {
    console.log("\n  DRY RUN — would create these blocks:");
    for (const s of sections) {
      console.log(`    - "${shortTitle} — ${s.name}" (${s.content.length} chars raw)`);
    }
    return {
      paper_id: paperId,
      title: shortTitle,
      sections: sections.length,
      blocks_created: 0,
      blob_stored: false,
      skipped: false,
      errors,
    };
  }

  // 6. Summarize and upsert each section
  let blocksCreated = 0;
  const totalSections = sections.length;

  for (const section of sections) {
    const blockTitle = `${shortTitle} — ${section.name}`;
    console.log(`  [${section.index + 1}/${totalSections}] ${blockTitle}...`);

    // Summarize
    const summary = await summarizeSection(section);
    console.log(`    ${section.content.length} chars → ${summary.length} chars`);

    // Build per-block metadata
    const blockMeta = {
      paper_id: paperId,
      doi: meta.doi,
      authors: meta.authors,
      year: meta.year,
      journal: meta.journal,
      section: section.name.toLowerCase().replace(/\s+/g, "_"),
      section_index: section.index,
      total_sections: totalSections,
      source: `paper-ingest-${new Date().toISOString().slice(0, 10)}`,
      ingest_version: 1,
    };

    // Build tags
    const tags = [
      "paper",
      section.name.toLowerCase().replace(/\s+/g, "-"),
      ...(meta.year ? [`y${meta.year}`] : []),
    ];

    // Upsert
    const ok = await upsertBlock(blockTitle, summary, tags, blockMeta);
    if (ok) {
      blocksCreated++;
    } else {
      errors.push(`Failed to upsert: ${blockTitle}`);
    }

    // Small delay to avoid overwhelming Ollama
    await Bun.sleep(200);
  }

  // 7. Store original PDF as blob
  console.log("  Storing original PDF as blob...");
  let blobStored = false;
  try {
    blobStored = await storePdfBlob(pdfPath, shortTitle, paperId);
    console.log(`  Blob stored: ${blobStored}`);
  } catch (e: any) {
    errors.push(`Blob storage failed: ${e.message}`);
    console.warn(`  Blob storage failed: ${e.message}`);
  }

  // 8. Summary
  console.log(`\n  DONE: ${blocksCreated}/${totalSections} blocks created, blob: ${blobStored}`);
  if (errors.length) {
    console.warn(`  ERRORS: ${errors.join("; ")}`);
  }

  return {
    paper_id: paperId,
    title: shortTitle,
    sections: totalSections,
    blocks_created: blocksCreated,
    blob_stored: blobStored,
    skipped: false,
    errors,
  };
}

// ---------------------------------------------------------------------------
// Utility
// ---------------------------------------------------------------------------

function truncateTitle(title: string, maxLen = 60): string {
  // Remove line breaks and excessive whitespace
  const clean = title.replace(/\s+/g, " ").trim();
  if (clean.length <= maxLen) return clean;
  return clean.slice(0, maxLen - 3).trim() + "...";
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

async function main() {
  const args = process.argv.slice(2);
  const flags = {
    title: undefined as string | undefined,
    doi: undefined as string | undefined,
    dryRun: args.includes("--dry-run"),
    force: args.includes("--force"),
    batch: args.includes("--batch"),
  };

  // Parse --title and --doi
  const titleIdx = args.indexOf("--title");
  if (titleIdx !== -1 && args[titleIdx + 1]) {
    flags.title = args[titleIdx + 1];
  }
  const doiIdx = args.indexOf("--doi");
  if (doiIdx !== -1 && args[doiIdx + 1]) {
    flags.doi = args[doiIdx + 1];
  }

  // Collect PDF paths (everything that's not a flag)
  const pdfPaths = args.filter(
    (a) => !a.startsWith("--") && (a.endsWith(".pdf") || a.endsWith(".PDF"))
  );

  if (pdfPaths.length === 0) {
    console.error("Usage: bun run ingest-paper.ts <pdf> [--title ...] [--doi ...] [--dry-run] [--force] [--batch]");
    process.exit(1);
  }

  // Validate files exist
  for (const p of pdfPaths) {
    try {
      await stat(p);
    } catch {
      console.error(`File not found: ${p}`);
      process.exit(1);
    }
  }

  // Ingest
  const results: IngestResult[] = [];
  for (const pdfPath of pdfPaths) {
    try {
      const result = await ingestPaper(pdfPath, {
        // Only use explicit title for single-file mode
        title: pdfPaths.length === 1 ? flags.title : undefined,
        doi: pdfPaths.length === 1 ? flags.doi : undefined,
        dryRun: flags.dryRun,
        force: flags.force,
      });
      results.push(result);
    } catch (e: any) {
      console.error(`FATAL: ${pdfPath}: ${e.message}`);
      results.push({
        paper_id: "error",
        title: basename(pdfPath),
        sections: 0,
        blocks_created: 0,
        blob_stored: false,
        skipped: false,
        errors: [e.message],
      });
    }
  }

  // Final report
  console.log(`\n${"=".repeat(60)}`);
  console.log("INGESTION REPORT");
  console.log(`${"=".repeat(60)}`);
  for (const r of results) {
    const status = r.skipped ? "SKIP" : r.errors.length ? "WARN" : "OK";
    console.log(`  [${status}] ${r.title}: ${r.blocks_created} blocks, blob: ${r.blob_stored}`);
  }

  const totalBlocks = results.reduce((sum, r) => sum + r.blocks_created, 0);
  const totalErrors = results.reduce((sum, r) => sum + r.errors.length, 0);
  console.log(`\n  Total: ${results.length} papers, ${totalBlocks} blocks, ${totalErrors} errors`);
}

main();
