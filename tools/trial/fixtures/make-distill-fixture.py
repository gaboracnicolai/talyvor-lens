#!/usr/bin/env python3
"""Regenerates tools/trial/fixtures/distill-fixture.pdf — a minimal, real,
text-bearing PDF (correct xref offsets) that the distill-worker (ledongthuc/pdf)
converts to the markdown "TLVDISTILL spike fixture one two three".

⚠️  CHANGING THE BYTES CHANGES EVERY HASH ASSERTION. The pooled distill-cache key
and the distill_serve_attribution.content_hash both derive from these exact
bytes — scenarios (o)/(p) assert against them. Do NOT regenerate casually; if you
must, re-capture the content_hash in the README. Committed as a fixed file (not
generated at runtime) precisely so the bytes are stable forever.
"""
objs = [
    b"<< /Type /Catalog /Pages 2 0 R >>",
    b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
    b"<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 4 0 R >> >> /MediaBox [0 0 612 792] /Contents 5 0 R >>",
    b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
]
stream = b"BT /F1 24 Tf 72 700 Td (TLVDISTILL spike fixture one two three) Tj ET"
objs.append(b"<< /Length %d >>\nstream\n%s\nendstream" % (len(stream), stream))
pdf = b"%PDF-1.4\n"
offsets = []
for i, o in enumerate(objs, 1):
    offsets.append(len(pdf))
    pdf += b"%d 0 obj\n%s\nendobj\n" % (i, o)
xref_at = len(pdf)
pdf += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs) + 1)
for off in offsets:
    pdf += b"%010d 00000 n \n" % off
pdf += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF" % (len(objs) + 1, xref_at)
import os
open(os.path.join(os.path.dirname(__file__), "distill-fixture.pdf"), "wb").write(pdf)
print("wrote distill-fixture.pdf bytes=%d" % len(pdf))
