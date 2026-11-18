# Writes minimal PDF 1.4 files: each page a list of text lines in Helvetica,
# or (text=None) a filled shape with no text.
import sys
def pdf(pages, path):
    objs = []
    def add(b): objs.append(b); return len(objs)
    font = add(b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
    pages_id = len(objs) + 1 + 2*len(pages)
    kids = []
    for lines in pages:
        if lines is None:
            content = b"0.1 0.4 0.7 rg 100 300 412 300 re f 0.9 0.6 0.1 rg 200 400 m 300 600 l 400 400 l f"
        else:
            parts = [b"BT /F1 36 Tf 60 700 Td 44 TL"]
            for l in lines:
                parts.append(b"(" + l.encode() + b") Tj T*")
            parts.append(b"ET")
            content = b" ".join(parts)
        c = add(b"<< /Length %d >>\nstream\n" % len(content) + content + b"\nendstream")
        p = add(b"<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>" % (pages_id, font, c))
        kids.append(p)
    assert add(b"<< /Type /Pages /Kids [" + b" ".join(b"%d 0 R" % k for k in kids) + b"] /Count %d >>" % len(kids)) == pages_id
    cat = add(b"<< /Type /Catalog /Pages %d 0 R >>" % pages_id)
    out = bytearray(b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
    offs = []
    for i, o in enumerate(objs):
        offs.append(len(out))
        out += b"%d 0 obj\n" % (i+1) + o + b"\nendobj\n"
    x = len(out)
    out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs)+1)
    for o in offs: out += b"%010d 00000 n \n" % o
    out += b"trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objs)+1, cat, x)
    open(path, "wb").write(out)

d = sys.argv[1]
pdf([["A sea turtle swims", "slowly through the", "blue ocean water."]], d + "/turtle.pdf")
pdf([["Page %d of seven." % (i+1)] for i in range(7)], d + "/seven-pages.pdf")
pdf([None], d + "/no-text.pdf")
pdf([["Cats sleeping on a couch."], ["A sea turtle swims", "in the ocean."]], d + "/two-pages.pdf")
