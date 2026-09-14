# gh-pages — the published site for slaptain

Serves <https://slaptain.chuck-chuck-chuck.net>. Orphan branch: no shared history
with `main`, so nothing here appears in a normal checkout of the project.

    index.html   the project page
    CNAME        the custom domain (also set in Settings -> Pages)
    .nojekyll    serve files verbatim
    ldapcon/     the LDAPCon deck (added after the talk)

The deck must be built from the **scrubbed** copy of the slide sources
(`npm run shareable` in the deck repo), never from the master copy: Slidev
compiles speaker notes into the JavaScript bundle.
