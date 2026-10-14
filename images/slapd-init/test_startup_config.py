"""Tests for the offline startup-only configuration converger (ADR-034).

The fixtures below are real `slapcat -F <dir> -n 0` output from OpenLDAP 2.7.1,
trimmed to the attributes the converger looks at. Two of them are kept exactly
as slapcat wrote them because they are the shapes a naive parser gets wrong: the
long suffix of `olcDatabase={3}mdb` is FOLDED across two lines, and the suffix of
`olcDatabase={4}mdb` is BASE64-encoded because it is not ASCII.

Run with: make test-init
"""

import base64
import os
import tempfile
import unittest

import startup_config as sc


# ── Fixtures ──────────────────────────────────────────────────────────────────

# Five databases: frontend and config (no suffix, never a target), the data DB
# `dc=example,dc=com`, its accesslog DB `cn=accesslog-default`, and two more
# whose only job is to exercise LDIF folding and base64. Every mdb database
# carries a syncprov overlay child, with the `{N}` ordering prefix slapd assigns.
CONFIG = """\
dn: cn=config
objectClass: olcGlobal

dn: olcDatabase={-1}frontend,cn=config
objectClass: olcDatabaseConfig
objectClass: olcFrontendConfig
olcDatabase: {-1}frontend
structuralObjectClass: olcDatabaseConfig

dn: olcDatabase={0}config,cn=config
objectClass: olcDatabaseConfig
olcDatabase: {0}config
structuralObjectClass: olcDatabaseConfig

dn: olcDatabase={1}mdb,cn=config
objectClass: olcDatabaseConfig
objectClass: olcMdbConfig
olcDatabase: {1}mdb
olcSuffix: dc=example,dc=com
olcDbDirectory: /data/default
structuralObjectClass: olcMdbConfig

dn: olcOverlay={0}syncprov,olcDatabase={1}mdb,cn=config
objectClass: olcOverlayConfig
objectClass: olcSyncProvConfig
olcOverlay: {0}syncprov
structuralObjectClass: olcSyncProvConfig

dn: olcDatabase={2}mdb,cn=config
objectClass: olcDatabaseConfig
objectClass: olcMdbConfig
olcDatabase: {2}mdb
olcSuffix: cn=accesslog-default
olcDbDirectory: /accesslog/default
structuralObjectClass: olcMdbConfig

dn: olcOverlay={0}syncprov,olcDatabase={2}mdb,cn=config
objectClass: olcOverlayConfig
objectClass: olcSyncProvConfig
olcOverlay: {0}syncprov
structuralObjectClass: olcSyncProvConfig

dn: olcDatabase={3}mdb,cn=config
objectClass: olcDatabaseConfig
objectClass: olcMdbConfig
olcDatabase: {3}mdb
olcSuffix: ou=a-very-long-organizational-unit-name-indeed,dc=exceedingly-long,
 dc=example,dc=net
olcDbDirectory: /data/long
structuralObjectClass: olcMdbConfig

dn: olcOverlay={0}syncprov,olcDatabase={3}mdb,cn=config
objectClass: olcOverlayConfig
objectClass: olcSyncProvConfig
olcOverlay: {0}syncprov
structuralObjectClass: olcSyncProvConfig

dn: olcDatabase={4}mdb,cn=config
objectClass: olcDatabaseConfig
objectClass: olcMdbConfig
olcDatabase: {4}mdb
olcSuffix:: bz3DnG7Dr2NvZGUsZGM9dW5pLGRjPW5ldA==
olcDbDirectory: /data/uni
structuralObjectClass: olcMdbConfig

dn: olcOverlay={0}syncprov,olcDatabase={4}mdb,cn=config
objectClass: olcOverlayConfig
objectClass: olcSyncProvConfig
olcOverlay: {0}syncprov
structuralObjectClass: olcSyncProvConfig
"""

LONG_SUFFIX = ("ou=a-very-long-organizational-unit-name-indeed,"
               "dc=exceedingly-long,dc=example,dc=net")
UNI_SUFFIX = base64.b64decode("bz3DnG7Dr2NvZGUsZGM9dW5pLGRjPW5ldA==").decode("utf-8")

SYNCPROV_1 = "olcOverlay={0}syncprov,olcDatabase={1}mdb,cn=config"
SYNCPROV_3 = "olcOverlay={0}syncprov,olcDatabase={3}mdb,cn=config"

# The same configuration with the attribute already in place on the data DB's
# syncprov overlay — what the second pod start reads back.
CONFIG_WITH_SOURCE = CONFIG.replace(
    "dn: olcOverlay={0}syncprov,olcDatabase={1}mdb,cn=config\n"
    "objectClass: olcOverlayConfig\n"
    "objectClass: olcSyncProvConfig\n"
    "olcOverlay: {0}syncprov\n",
    "dn: olcOverlay={0}syncprov,olcDatabase={1}mdb,cn=config\n"
    "objectClass: olcOverlayConfig\n"
    "objectClass: olcSyncProvConfig\n"
    "olcOverlay: {0}syncprov\n"
    "olcSpSessionlogSource: cn=accesslog-default\n",
)

TABLE = "olcSpSessionlogSource\tdc=example,dc=com\tcn=accesslog-default\n"


def refusal_text(plan):
    return "\n".join(plan.refusals)


class FixtureSanityTest(unittest.TestCase):
    """A fixture built by str.replace is a fixture that can silently not change.

    Half the cases below distinguish "already correct" from "not set yet", so a
    replacement that quietly matched nothing would turn them into mirrors.
    """

    def test_the_with_source_fixture_really_differs(self):
        self.assertNotEqual(CONFIG, CONFIG_WITH_SOURCE)
        self.assertNotIn("olcSpSessionlogSource", CONFIG)
        self.assertIn("olcSpSessionlogSource: cn=accesslog-default",
                      CONFIG_WITH_SOURCE)


# ── The parser ────────────────────────────────────────────────────────────────

class ParseLDIFTest(unittest.TestCase):

    def test_folded_value_is_rejoined(self):
        entries = {e.dn: e for e in sc.parse_ldif(CONFIG)}
        self.assertEqual(
            entries["olcDatabase={3}mdb,cn=config"].first("olcSuffix"),
            LONG_SUFFIX,
        )

    def test_base64_value_is_decoded(self):
        entries = {e.dn: e for e in sc.parse_ldif(CONFIG)}
        self.assertEqual(
            entries["olcDatabase={4}mdb,cn=config"].first("olcSuffix"),
            UNI_SUFFIX,
        )

    def test_comments_and_blank_lines_are_skipped(self):
        entries = sc.parse_ldif("# a comment\n\ndn: cn=config\nobjectClass: olcGlobal\n")
        self.assertEqual([e.dn for e in entries], ["cn=config"])


# ── The desired table ─────────────────────────────────────────────────────────

class ParseTableTest(unittest.TestCase):

    def test_comments_and_blank_lines_are_ignored(self):
        records, refusals = sc.parse_table(
            "# rendered by the operator\n"
            "\n"
            + TABLE
        )
        self.assertEqual(refusals, [])
        self.assertEqual(len(records), 1)
        self.assertEqual(records[0].attribute, "olcSpSessionlogSource")
        self.assertEqual(records[0].suffix, "dc=example,dc=com")
        self.assertEqual(records[0].value, "cn=accesslog-default")

    def test_unknown_attribute_is_refused(self):
        records, refusals = sc.parse_table(
            "olcDbMaxSize\tdc=example,dc=com\t42\n" + TABLE)
        self.assertEqual(len(records), 1, "the known record must survive")
        self.assertIn("olcDbMaxSize", "\n".join(refusals))

    def test_malformed_line_is_refused(self):
        records, refusals = sc.parse_table("olcSpSessionlogSource only-two fields\n")
        self.assertEqual(records, [])
        self.assertTrue(refusals)


# ── The decision (pure) ───────────────────────────────────────────────────────

class PlanTest(unittest.TestCase):

    def test_happy_path_sets_the_attribute_on_the_syncprov_overlay(self):
        plan = sc.plan(sc.parse_ldif(CONFIG), sc.parse_table(TABLE)[0])
        self.assertEqual(plan.refusals, [])
        self.assertEqual(len(plan.modifications), 1)
        mod = plan.modifications[0]
        self.assertEqual(mod.dn, SYNCPROV_1)
        self.assertEqual(mod.attribute, "olcSpSessionlogSource")
        self.assertEqual(mod.value, "cn=accesslog-default")

    def test_already_correct_is_a_no_op(self):
        plan = sc.plan(sc.parse_ldif(CONFIG_WITH_SOURCE), sc.parse_table(TABLE)[0])
        self.assertEqual(plan.modifications, [])
        self.assertEqual(plan.refusals, [])

    def test_removing_the_line_deletes_the_attribute(self):
        plan = sc.plan(sc.parse_ldif(CONFIG_WITH_SOURCE), [])
        self.assertEqual(len(plan.modifications), 1)
        mod = plan.modifications[0]
        self.assertEqual(mod.dn, SYNCPROV_1)
        self.assertEqual(mod.attribute, "olcSpSessionlogSource")
        self.assertIsNone(mod.value, "a None value means delete the attribute")

    def test_value_naming_an_absent_database_is_refused(self):
        table, _ = sc.parse_table(
            "olcSpSessionlogSource\tdc=example,dc=com\tcn=accesslog-gone\n")
        plan = sc.plan(sc.parse_ldif(CONFIG), table)
        self.assertEqual(plan.modifications, [],
                         "a dangling source makes the pod unbootable — never write it")
        self.assertIn("cn=accesslog-gone", refusal_text(plan))

    def test_suffix_matching_no_database_is_refused(self):
        table, _ = sc.parse_table(
            "olcSpSessionlogSource\tdc=nosuch,dc=com\tcn=accesslog-default\n")
        plan = sc.plan(sc.parse_ldif(CONFIG), table)
        self.assertEqual(plan.modifications, [])
        self.assertIn("dc=nosuch,dc=com", refusal_text(plan))

    def test_database_without_a_syncprov_overlay_is_refused(self):
        config = CONFIG.replace(
            "dn: olcOverlay={0}syncprov,olcDatabase={1}mdb,cn=config\n"
            "objectClass: olcOverlayConfig\n"
            "objectClass: olcSyncProvConfig\n"
            "olcOverlay: {0}syncprov\n"
            "structuralObjectClass: olcSyncProvConfig\n\n", "")
        plan = sc.plan(sc.parse_ldif(config), sc.parse_table(TABLE)[0])
        self.assertEqual(plan.modifications, [])
        self.assertIn("syncprov", refusal_text(plan))

    def test_the_overlay_ordering_prefix_is_read_not_assumed(self):
        """`{0}` holds only while syncprov is the first overlay on the database."""
        config = CONFIG.replace(
            "dn: olcOverlay={0}syncprov,olcDatabase={1}mdb,cn=config\n"
            "objectClass: olcOverlayConfig\n"
            "objectClass: olcSyncProvConfig\n"
            "olcOverlay: {0}syncprov\n",
            "dn: olcOverlay={1}syncprov,olcDatabase={1}mdb,cn=config\n"
            "objectClass: olcOverlayConfig\n"
            "objectClass: olcSyncProvConfig\n"
            "olcOverlay: {1}syncprov\n")
        self.assertIn("olcOverlay={1}syncprov", config, "the fixture must differ")
        plan = sc.plan(sc.parse_ldif(config), sc.parse_table(TABLE)[0])
        self.assertEqual(plan.refusals, [])
        self.assertEqual([m.dn for m in plan.modifications],
                         ["olcOverlay={1}syncprov,olcDatabase={1}mdb,cn=config"])

    def test_suffix_selection_is_case_insensitive_and_reads_folded_values(self):
        table, _ = sc.parse_table(
            "olcSpSessionlogSource\t" + LONG_SUFFIX.upper() + "\tdc=example,dc=com\n")
        plan = sc.plan(sc.parse_ldif(CONFIG), table)
        self.assertEqual(plan.refusals, [])
        self.assertEqual([m.dn for m in plan.modifications], [SYNCPROV_3])

    def test_a_refused_value_leaves_the_existing_one_alone(self):
        table, _ = sc.parse_table(
            "olcSpSessionlogSource\tdc=example,dc=com\tcn=accesslog-gone\n")
        plan = sc.plan(sc.parse_ldif(CONFIG_WITH_SOURCE), table)
        self.assertEqual(plan.modifications, [],
                         "doubt leaves the value standing; it never deletes it")
        self.assertTrue(plan.refusals)

    def test_a_suffix_declared_twice_is_refused_and_changes_nothing(self):
        # Read against the configuration that ALREADY carries the attribute: an
        # ambiguous declaration must not resolve into a deletion.
        table, _ = sc.parse_table(TABLE + TABLE)
        plan = sc.plan(sc.parse_ldif(CONFIG_WITH_SOURCE), table)
        self.assertEqual(plan.modifications, [])
        self.assertTrue(plan.refusals)

    def test_a_refusal_never_suppresses_an_unrelated_write(self):
        table, _ = sc.parse_table(
            "olcSpSessionlogSource\tdc=nosuch,dc=com\tcn=accesslog-default\n" + TABLE)
        plan = sc.plan(sc.parse_ldif(CONFIG), table)
        self.assertEqual([m.dn for m in plan.modifications], [SYNCPROV_1])
        self.assertTrue(plan.refusals)


# ── Absence means unmanaged ───────────────────────────────────────────────────

class TableAbsentTest(unittest.TestCase):

    def test_absent_file_reads_as_unmanaged(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertIsNone(sc.read_table(os.path.join(d, "startup-config")))

    def test_unmanaged_changes_nothing_even_when_the_attribute_is_set(self):
        plan = sc.plan_for_table(CONFIG_WITH_SOURCE, None)
        self.assertEqual(plan.modifications, [],
                         "absence means unmanaged, never 'delete everything'")
        self.assertFalse(plan.managed)

    def test_an_empty_present_file_still_manages(self):
        plan = sc.plan_for_table(CONFIG_WITH_SOURCE, "")
        self.assertTrue(plan.managed)
        self.assertEqual(len(plan.modifications), 1)
        self.assertIsNone(plan.modifications[0].value)


# ── The LDIF the converger hands slapmodify ───────────────────────────────────

class RenderTest(unittest.TestCase):

    def test_replace_record(self):
        ldif = sc.render_modify_ldif([
            sc.Modification(SYNCPROV_1, "olcSpSessionlogSource", "cn=accesslog-default")])
        self.assertEqual(ldif,
                         "dn: " + SYNCPROV_1 + "\n"
                         "changetype: modify\n"
                         "replace: olcSpSessionlogSource\n"
                         "olcSpSessionlogSource: cn=accesslog-default\n"
                         "-\n\n")

    def test_delete_record(self):
        ldif = sc.render_modify_ldif([
            sc.Modification(SYNCPROV_1, "olcSpSessionlogSource", None)])
        self.assertEqual(ldif,
                         "dn: " + SYNCPROV_1 + "\n"
                         "changetype: modify\n"
                         "delete: olcSpSessionlogSource\n"
                         "-\n\n")

    def test_non_ascii_value_is_base64_encoded(self):
        ldif = sc.render_modify_ldif([
            sc.Modification(SYNCPROV_1, "olcSpSessionlogSource", UNI_SUFFIX)])
        self.assertIn("olcSpSessionlogSource:: " +
                      base64.b64encode(UNI_SUFFIX.encode("utf-8")).decode("ascii"), ldif)


if __name__ == "__main__":
    unittest.main()
