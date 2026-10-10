# gh-pages — the published site for slaptain

Serves <https://slaptain.chuck-chuck-chuck.net>. Orphan branch: no shared history
with `main`, so nothing here appears in a normal checkout of the project.

    index.html   the project page
    CNAME        the custom domain (also set in Settings -> Pages)
    .nojekyll    serve files verbatim
    ldapcon/     the LDAPCon deck: single-file HTML and PDF

`ldapcon/index.html` is the deck as one self-contained file (`npm run portable`
in `2026-ldapcon-slaptain/` of the `talks` repo), next to the published PDF.
Build it only from that published `slides.md`, never from a working copy with
speaker notes: Slidev compiles notes into the JavaScript bundle.
