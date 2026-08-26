# Microservices in Go — book source

These are the individual source files that build the EPUB
*Microservices in Go: Building Cloud-Native Systems with NATS and Google Cloud*.

## Layout

```
metadata.yaml            # EPUB metadata (title, author, rights, description)
epub.css                 # stylesheet, including the note/tip/warning asides
build.sh                 # the pandoc command that assembles the EPUB
00-about.md              # front matter
01..14-*.md              # Chapters 1–14
15-appendix.md           # command reference appendix
images/                  # figures referenced by the chapters + cover.png
```

Chapters are plain Pandoc Markdown. Two conventions to know:

- Asides use fenced divs: `::: note` … `:::` (also `::: tip`, `::: warning`),
  styled by `epub.css`.
- An em dash is written `---` inline; Pandoc renders it as “—”. A lone `---`
  line inside a fenced code block is literal (e.g. a YAML document separator).

## Rebuild

Install Pandoc 3.x, then:

```sh
./build.sh
```

This produces `Microservices-in-Go.epub`. Convert onward to PDF or HTML with
Pandoc as well, e.g. `pandoc … -o book.html`.

## Provenance

These source files were regenerated from the compiled EPUB (via Pandoc) because
the original authoring workspace was a temporary sandbox that has since been
cleared. They are faithful in content and rebuild to an equivalent EPUB; minor
cosmetic differences from the very first drafts are possible (for example, an em
dash written as `---` rather than the literal “—”). One figure that was present
in the working tree but unused by any chapter, `ch05_compose.png`, is not
included because it was never part of the built book.
