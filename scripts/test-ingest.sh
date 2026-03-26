#!/bin/bash
# Quick validation of the paper ingestion pipeline
# Creates a minimal test PDF, runs --dry-run, and verifies output

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TEST_PDF="/tmp/test-paper.pdf"

echo "=== Creating test PDF ==="
# Use pdftotext's sibling tool (or python) to create a minimal PDF
python3 -c "
# Minimal PDF with scientific paper structure
content = '''%PDF-1.4
1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj
2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj
3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj
5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj
4 0 obj
<</Length 850>>
stream
BT
/F1 16 Tf
100 750 Td
(Attention Is All You Need) Tj
/F1 10 Tf
100 730 Td
(Vaswani et al., 2017, NeurIPS) Tj
100 700 Td
(Abstract) Tj
/F1 9 Tf
100 685 Td
(The dominant sequence transduction models are based on complex recurrent or) Tj
100 673 Td
(convolutional neural networks. We propose a new simple network architecture,) Tj
100 661 Td
(the Transformer, based solely on attention mechanisms, dispensing with recurrence) Tj
100 649 Td
(and convolutions entirely. The Transformer achieves 28.4 BLEU on WMT 2014.) Tj
100 620 Td
(Introduction) Tj
100 605 Td
(Recurrent neural networks have been firmly established as state of the art.) Tj
100 580 Td
(Methodology) Tj
100 565 Td
(We use multi-head self-attention with scaled dot-product attention.) Tj
100 550 Td
(The attention function maps a query and key-value pairs to an output.) Tj
100 520 Td
(Results) Tj
100 505 Td
(On WMT 2014 English-to-German, the big transformer model outperforms) Tj
100 493 Td
(the best previously reported models by more than 2.0 BLEU, achieving 28.4.) Tj
100 463 Td
(Conclusion) Tj
100 448 Td
(The Transformer is the first transduction model relying entirely on) Tj
100 436 Td
(self-attention, replacing recurrent layers most commonly used.) Tj
ET
endstream
endobj
xref
0 6
0000000000 65535 f
0000000009 00000 n
0000000058 00000 n
0000000115 00000 n
0000000296 00000 n
0000000230 00000 n
trailer<</Size 6/Root 1 0 R>>
startxref
1198
%%EOF'''
with open('$TEST_PDF', 'w') as f:
    f.write(content)
"

echo "=== Verifying pdftotext extraction ==="
pdftotext -layout "$TEST_PDF" - | head -20

echo ""
echo "=== Running ingest-paper.ts --dry-run ==="
cd "$SCRIPT_DIR"
bun run ingest-paper.ts "$TEST_PDF" --title "Attention Is All You Need" --dry-run

echo ""
echo "=== Test complete ==="
rm -f "$TEST_PDF"
