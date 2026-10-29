#!/usr/bin/python3
"""Converge the startup-only `cn=config` attributes offline, at pod start.

ADR-034. A minority of slapd settings cannot be written to a running server —
some segfault it, some hang it, and `olcSpSessionlogSource` is a double free
that no release fixes. The one moment a pod's configuration is on disk and
nothing holds it open is here, in the init container, before slapd runs. So the
declared table below is converged with `slapcat -n 0` to read and
`slapmodify -n 0` to write; the latter reaches back-ldif's tool modify and never
runs the configuration parser, which is what makes the write safe and also what
keeps the `# CRC32` header each entry file carries correct.

The desired values arrive as a mounted ConfigMap. `STARTUP_CONFIG_PATH` names
the mount DIRECTORY and the ConfigMap key is the file inside it — the
`DATABASE_DIRS_PATH` convention — so resolving the two is this side's job. One
TAB-separated record per line:

    <attribute>\t<database-suffix>\t<value>

`#` starts a comment, blank lines are skipped. The attribute must be one of the
compiled-in table below — which `cn=config` entry carries it is a property of
the attribute, not of the file. The suffix selects an mdb database by its
`olcSuffix`, case-insensitively.

Absence of the mount means UNMANAGED: nothing is read, nothing is written,
nothing is removed. It is what an old operator, a pod without the volume, and
the window before the ConfigMap exists all look like, and reading it as "delete
everything" would strip a working configuration on an operator upgrade. A mount
that is there but yields no readable table is reported and skipped for the same
reason, one step louder — nothing about this feature may keep a pod from
starting, used or not.

A present file is the whole truth: for every managed attribute and every mdb
database, the desired state is the value stated there, or absent when no line
names that pair.

R4 — refuse on doubt. A modification is emitted only when the database resolves
to exactly one entry, the entry that carries the attribute exists, and (for an
attribute whose value names a backend) the value resolves to a configured suffix
in the SAME dump. `syncprov_db_open` returns -1 for a backend that is not there,
which is a pod that will not boot — the failure shape ADR-019 R8 ended in. A
refusal is loud and NOT fatal: the pod still starts, carrying the configuration
it already had.
"""

import base64
import os
import subprocess
import sys
import tempfile


# ── The declared table (ADR-034 R1, R5) ───────────────────────────────────────

class ManagedAttribute:
    """One startup-only attribute: where it lives and what its value may name.

    `locator` finds the entry that carries the attribute, given the selected
    database entry and the whole dump. `value_names_database` marks a value that
    is a DN resolved against the configured backends at startup — the class of
    value that can make a pod unbootable, and therefore the class that must be
    proven resolvable before it is written.

    `exclusive_with` names the attributes that may not stand beside this one on
    the same entry. It is DATA for the same reason the other two are: where an
    attribute lives and what its value means are properties of the attribute,
    and so is what it displaces. The converger removes them in the SAME
    `slapmodify` run that writes this one, because the only two readings slapd
    may ever get of that file are "before" and "after" — never a configuration
    carrying both, which it rejects as a wasteful operation.
    """

    def __init__(self, name, locator, value_names_database, exclusive_with=()):
        self.name = name
        self.locator = locator
        self.value_names_database = value_names_database
        self.exclusive_with = tuple(exclusive_with)


def _syncprov_overlay_of(db_entry, entries):
    """The syncprov overlay child of a database entry.

    Its real DN carries the `{N}` ordering prefix slapd assigns, so it is READ
    from the dump, never reconstructed: `{0}` holds only while syncprov is the
    first overlay on that database.
    """
    suffix = "," + db_entry.dn
    found = []
    for e in entries:
        if not e.dn.endswith(suffix):
            continue
        if e.dn[: -len(suffix)].count(",") != 0:
            continue  # a grandchild, not a child
        overlay = e.first("olcOverlay")
        if overlay is not None and _strip_index(overlay) == "syncprov":
            found.append(e)
    return found


MANAGED_ATTRIBUTES = {
    "olcspsessionlogsource": ManagedAttribute(
        name="olcSpSessionlogSource",
        locator=_syncprov_overlay_of,
        value_names_database=True,
        # The in-memory session log this one replaces (ADR-022, ADR-034). The
        # operator puts it there and keeps it there until the source is live,
        # so the database is never without a session log; this is the other end
        # of that handover, and it is the only moment at which the two can be
        # exchanged without the database having none or two.
        exclusive_with=("olcSpSessionlog",),
    ),
}


# ── LDIF ──────────────────────────────────────────────────────────────────────

class Entry:
    """One LDIF entry: a DN and its attributes, keyed case-insensitively."""

    def __init__(self, dn, attrs):
        self.dn = dn
        self.attrs = attrs  # lowercased name -> list of values

    def values(self, name):
        return self.attrs.get(name.lower(), [])

    def first(self, name):
        vals = self.values(name)
        return vals[0] if vals else None

    def has_class(self, name):
        return name.lower() in [v.lower() for v in self.values("objectClass")]


def _decode(raw, is_base64):
    if not is_base64:
        return raw
    return base64.b64decode(raw).decode("utf-8", "replace")


def parse_ldif(text):
    """Parse LDIF into entries, rejoining folded lines and decoding base64.

    Both shapes come out of a real `slapcat -n 0`: it folds at 78 columns and
    base64-encodes any value that is not plain ASCII.
    """
    entries = []
    attrs = None
    pending = None  # (name, raw, is_base64) still open for continuation lines

    def close_attribute():
        if pending is None:
            return
        name, raw, is_b64 = pending
        value = _decode(raw, is_b64)
        if name == "dn":
            # set by the caller below; kept here so folding works for DNs too
            attrs["__dn__"] = [value]
        else:
            attrs.setdefault(name, []).append(value)

    def close_entry():
        nonlocal attrs, pending
        close_attribute()
        pending = None
        if attrs is not None and "__dn__" in attrs:
            entries.append(Entry(attrs.pop("__dn__")[0], attrs))
        attrs = None

    for line in text.split("\n"):
        if line.startswith(" ") and pending is not None:
            name, raw, is_b64 = pending
            pending = (name, raw + line[1:], is_b64)
            continue
        close_attribute()
        pending = None
        if not line.strip():
            close_entry()
            continue
        if line.startswith("#"):
            continue
        if ":" not in line:
            continue
        name, rest = line.split(":", 1)
        name = name.strip().lower()
        is_b64 = rest.startswith(":")
        if is_b64:
            rest = rest[1:]
        if rest.startswith("<"):
            continue  # a URL-valued attribute; slapcat never emits one here
        raw = rest[1:] if rest.startswith(" ") else rest
        if name == "dn":
            close_entry()
            attrs = {}
        if attrs is None:
            attrs = {}
        pending = (name, raw, is_b64)
    close_entry()
    return entries


def _needs_base64(value):
    try:
        value.encode("ascii")
    except UnicodeEncodeError:
        return True
    return (value.startswith((" ", ":", "<")) or value.endswith(" ")
            or any(ord(c) < 32 for c in value))


def _ldif_line(name, value):
    if _needs_base64(value):
        return "%s:: %s" % (name, base64.b64encode(value.encode("utf-8")).decode("ascii"))
    return "%s: %s" % (name, value)


def render_modify_ldif(mods):
    """The modify records handed to `slapmodify -l`."""
    out = []
    for mod in mods:
        out.append(_ldif_line("dn", mod.dn))
        out.append("changetype: modify")
        if mod.value is None:
            out.append("delete: %s" % mod.attribute)
        else:
            out.append("replace: %s" % mod.attribute)
            out.append(_ldif_line(mod.attribute, mod.value))
        out.append("-")
        # Inside the SAME record. back-ldif rewrites the whole entry file per
        # record, so a second record would put a file carrying both attributes
        # on disk between the two — briefly, but durably if the process dies
        # there, and slapd calls that pair a wasteful operation.
        for attribute in mod.delete_also:
            out.append("delete: %s" % attribute)
            out.append("-")
        out.append("")
    return "\n".join(out) + "\n" if out else ""


# ── The desired table ─────────────────────────────────────────────────────────

class Record:
    def __init__(self, attribute, suffix, value):
        self.attribute = attribute
        self.suffix = suffix
        self.value = value


# The ConfigMap key, and therefore the file name under the mount directory.
# Mirrors startupConfigKey in operator/internal/controller/startup_config.go.
TABLE_FILE_NAME = "startup-config"


class TableUnreadable(Exception):
    """The table is there in some form and cannot be read as one.

    Distinct from absence, which is UNMANAGED. This one is reported loudly and
    changes nothing — see read_table.
    """


def resolve_table_path(path):
    """STARTUP_CONFIG_PATH names the mount DIRECTORY; resolve the key inside it.

    Mirrors DATABASE_DIRS_PATH, which is the convention the operator follows:
    the env var is the mount point and the ConfigMap key is the file name under
    it. A path that is not a directory is used as given, so a hand-run against a
    single file keeps working.
    """
    if os.path.isdir(path):
        return os.path.join(path, TABLE_FILE_NAME)
    return path


def read_table(path):
    """The desired table as text, or None when nothing is mounted at all.

    None is UNMANAGED and is not an error — see the module docstring. Anything
    else that stops the table being read (a mount with no key file in it, a
    permission error, a path that is neither) raises TableUnreadable, which the
    caller reports and survives: a pod that starts with the configuration it
    already had beats a fleet that cannot start because a projection surprised
    us.
    """
    target = resolve_table_path(path)
    try:
        with open(target, "r", encoding="utf-8") as fh:
            return fh.read()
    except FileNotFoundError:
        if target != path:
            raise TableUnreadable(
                "%s is a directory with no %s in it" % (path, TABLE_FILE_NAME))
        return None
    except OSError as exc:
        raise TableUnreadable("%s: %s" % (target, exc))


def parse_table(text):
    """(records, refusals) for a present table. Unknown attributes are refused."""
    records = []
    refusals = []
    for lineno, line in enumerate(text.split("\n"), start=1):
        line = line.rstrip("\r")
        if not line.strip() or line.startswith("#"):
            continue
        fields = line.split("\t")
        if len(fields) != 3:
            refusals.append(
                "line %d: expected 3 tab-separated fields, got %d: %r"
                % (lineno, len(fields), line))
            continue
        attribute, suffix, value = (f.strip() for f in fields)
        known = MANAGED_ATTRIBUTES.get(attribute.lower())
        if known is None:
            refusals.append(
                "line %d: %s is not a managed startup-only attribute"
                % (lineno, attribute))
            continue
        if not suffix or not value:
            refusals.append("line %d: empty suffix or value: %r" % (lineno, line))
            continue
        records.append(Record(known.name, suffix, value))
    return records, refusals


# ── The decision (pure) ───────────────────────────────────────────────────────

class Modification:
    """Set `attribute` on `dn` to `value`, or delete it when value is None.

    `delete_also` names further attributes removed from the same entry in the
    same record — the exclusive-with set, narrowed to the ones actually present
    in the dump. Narrowed because `slapmodify` fails outright on a delete of an
    attribute that is not set, and a converger that refuses to run is worse than
    the state it was asked to fix.

    One record, not two, so there is no moment at which the file on disk carries
    the new attribute and the one it replaces.
    """

    def __init__(self, dn, attribute, value, delete_also=()):
        self.dn = dn
        self.attribute = attribute
        self.value = value
        self.delete_also = tuple(delete_also)

    def __repr__(self):
        verb = "delete" if self.value is None else "replace with %r" % self.value
        if self.delete_also:
            verb += " (and delete %s)" % ", ".join(self.delete_also)
        return "<%s %s: %s>" % (self.dn, self.attribute, verb)


class Plan:
    def __init__(self, modifications, refusals, managed=True):
        self.modifications = modifications
        self.refusals = refusals
        self.managed = managed


def _strip_index(value):
    """`{0}syncprov` → `syncprov`. slapd orders siblings with a `{N}` prefix."""
    if value.startswith("{"):
        end = value.find("}")
        if end != -1:
            return value[end + 1:]
    return value


def normalize_dn(dn):
    """Fold a DN for comparison: lowercase, no space around the separators.

    Not a full RFC 4514 normalisation — it is the comparison slapd's own
    `olcSuffix` values and an operator-rendered table need, and nothing here
    writes a value that was not matched this way.
    """
    return ",".join(part.strip() for part in dn.split(",")).lower()


def _databases(entries):
    """Every mdb database entry that has a suffix, by normalized suffix."""
    by_suffix = {}
    for e in entries:
        if not e.has_class("olcMdbConfig"):
            continue
        suffix = e.first("olcSuffix")
        if suffix is None:
            continue
        by_suffix.setdefault(normalize_dn(suffix), []).append(e)
    return by_suffix


def plan(entries, records):
    """Decide, from a parsed configuration and a parsed table. Pure."""
    modifications = []
    refusals = []
    by_suffix = _databases(entries)
    desired = {}
    for rec in records:
        key = (rec.attribute.lower(), normalize_dn(rec.suffix))
        if key in desired:
            refusals.append(
                "%s is declared twice for %s; neither value is written"
                % (rec.attribute, rec.suffix))
            desired[key] = None
            continue
        desired[key] = rec

    for managed in MANAGED_ATTRIBUTES.values():
        attr_key = managed.name.lower()

        # Deletions first: every entry that carries the attribute and is not
        # named by the table. Removing a line removes the attribute, and a
        # deletion needs none of the preconditions below — dropping an attribute
        # can never make slapd unbootable.
        for norm_suffix, dbs in sorted(by_suffix.items()):
            if (attr_key, norm_suffix) in desired:
                # Named by the table — including when the value was refused or
                # declared twice. Doubt leaves the existing value alone; it
                # never turns into a deletion.
                continue
            for db in dbs:
                for carrier in managed.locator(db, entries):
                    if carrier.first(managed.name) is not None:
                        modifications.append(
                            Modification(carrier.dn, managed.name, None))

        for (key_attr, norm_suffix), rec in sorted(
                desired.items(), key=lambda kv: kv[0]):
            if key_attr != attr_key or rec is None:
                continue
            dbs = by_suffix.get(norm_suffix, [])
            if len(dbs) != 1:
                refusals.append(
                    "%s: %s matches %d databases, not 1 — refusing to write"
                    % (managed.name, rec.suffix, len(dbs)))
                continue
            carriers = managed.locator(dbs[0], entries)
            if len(carriers) != 1:
                refusals.append(
                    "%s: database %s has %d syncprov overlays, not 1 — refusing "
                    "to write" % (managed.name, rec.suffix, len(carriers)))
                continue
            if managed.value_names_database:
                if normalize_dn(rec.value) not in by_suffix:
                    refusals.append(
                        "%s: value %s names no configured database — writing it "
                        "would leave a pod that does not start"
                        % (managed.name, rec.value))
                    continue
            # The attributes this one replaces, narrowed to those actually on
            # the entry: slapmodify fails outright on a delete of an attribute
            # that is not set.
            delete_also = tuple(
                name for name in managed.exclusive_with
                if carriers[0].first(name) is not None)

            current = carriers[0].first(managed.name)
            if (current is not None
                    and normalize_dn(current) == normalize_dn(rec.value)
                    and not delete_also):
                continue
            # "Already correct" is a property of the PAIR. A right value with a
            # leftover exclusive beside it is the wasteful combination, and
            # nothing else ever looks at this entry offline — so it is rewritten
            # here, identical value and all, rather than left for a pass that
            # does not exist.
            modifications.append(
                Modification(carriers[0].dn, managed.name, rec.value,
                             delete_also=delete_also))

    return Plan(modifications, refusals)


def plan_for_table(config_text, table_text):
    """The whole decision from two texts. `table_text` None means unmanaged."""
    if table_text is None:
        return Plan([], [], managed=False)
    records, refusals = parse_table(table_text)
    result = plan(parse_ldif(config_text), records)
    result.refusals = refusals + result.refusals
    return result


# ── The IO around it ──────────────────────────────────────────────────────────

DEFAULT_TABLE_PATH = "/etc/slaptain/startup-config"


def slapcat_config(config_dir):
    return subprocess.run(
        ["slapcat", "-F", config_dir, "-n", "0"],
        check=True, stdout=subprocess.PIPE, universal_newlines=True).stdout


def slapmodify(config_dir, ldif):
    with tempfile.NamedTemporaryFile(
            "w", suffix=".ldif", encoding="utf-8", delete=False) as fh:
        fh.write(ldif)
        path = fh.name
    try:
        subprocess.run(
            ["slapmodify", "-F", config_dir, "-n", "0", "-l", path], check=True)
    finally:
        os.unlink(path)


def main(argv):
    config_dir = argv[1] if len(argv) > 1 else os.environ.get(
        "CONFIG_DIR", "/config") + "/slapd.d"
    table_path = os.environ.get("STARTUP_CONFIG_PATH") or DEFAULT_TABLE_PATH

    try:
        table_text = read_table(table_path)
    except TableUnreadable as exc:
        # Never fatal. The table is the operator's to publish and the mount is
        # Kubernetes's to project; neither is a reason to refuse to boot.
        print("REFUSED: startup-only config table unreadable: %s — nothing is "
              "managed this pass" % exc)
        return 0
    if table_text is None:
        print("Startup-only config: no table at %s — nothing is managed" % table_path)
        return 0

    result = plan_for_table(slapcat_config(config_dir), table_text)
    for refusal in result.refusals:
        print("REFUSED: %s" % refusal)
    if not result.modifications:
        print("Startup-only config: already converged (%d refusal(s))"
              % len(result.refusals))
        return 0

    for mod in result.modifications:
        print("Converging %s" % mod)
    try:
        slapmodify(config_dir, render_modify_ldif(result.modifications))
    except subprocess.CalledProcessError as exc:
        print("ERROR: slapmodify exited %d — the configuration is not what was "
              "asked for" % exc.returncode)
        return 1

    # Read it back. A configuration slapd will refuse at startup must never
    # leave this script behind a zero exit — better a pod that does not start
    # now than one that does not start at the next restart, for a reason nobody
    # will connect to this.
    verify = plan_for_table(slapcat_config(config_dir), table_text)
    if verify.modifications:
        print("ERROR: startup-only config did not take: %s" % verify.modifications)
        return 1
    print("Startup-only config: converged %d attribute(s)"
          % len(result.modifications))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
