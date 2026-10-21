# Microservices in Go — book source

These are the individual source files that build the EPUB
*Microservices in Go: Building Cloud-Native Systems with NATS and Google Cloud*.

## Layout

```
metadata.yaml            # EPUB metadata (title, author, rights, description)
epub.css                 # stylesheet, including the note/tip/warning asides
code-dark.theme          # syntax-highlighting colors for the dark code blocks
build.sh                 # builds the EPUB and book-src.zip (pins the Pandoc version)
book-src.zip             # generated: these sources as one download
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

Install the Pandoc version pinned at the top of `build.sh` (currently 3.12;
other 3.x releases work but print a warning), plus Python 3, then:

```sh
./build.sh
```

This produces `Microservices-in-Go.epub` and regenerates `book-src.zip`. The
build is reproducible: with the pinned Pandoc, the same sources always give
byte-identical files, here and in CI. Convert onward to PDF or HTML with Pandoc
as well, e.g. `pandoc … -o book.html`.

Pushing a change under `files/` to `main` also triggers `.forgejo/workflows/epub.yaml`,
which runs this same script and commits the rebuilt `Microservices-in-Go.epub`
(at the repository root) and `book-src.zip` if they changed. It also attaches
the EPUB to the rolling `latest` release. If you commit the outputs of your own
build together with the sources, CI finds nothing to change.

## Provenance

These source files were regenerated from the compiled EPUB (via Pandoc) because
the original authoring workspace was a temporary sandbox that has since been
cleared. They are faithful in content and rebuild to an equivalent EPUB; minor
cosmetic differences from the very first drafts are possible (for example, an em
dash written as `---` rather than the literal “—”). One figure that was present
in the working tree but unused by any chapter, `ch05_compose.png`, is not
included because it was never part of the built book.
