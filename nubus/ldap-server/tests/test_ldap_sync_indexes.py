# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2024 Univention GmbH
import logging
import os
from json import JSONDecodeError
from pathlib import Path

import pytest


def get_test_config(sync_ldap_indexes, state_filename: str, mdb_filename: str):
    base_dir = os.path.abspath(os.path.dirname(__file__))
    return sync_ldap_indexes.Config(
        log_level="INFO",
        ldap_base_dn="cn=test,cn=internal",
        schema_dirs=[Path(f"{base_dir}/ldap-testfiles/")],
        state_file_path=Path(f"{base_dir}/ldap-testfiles/{state_filename}"),
        state_file_template_path=Path(f"{base_dir}/ldap-testfiles/test-ldap-statefile.json"),
        mdb_file_path=Path(f"{base_dir}/ldap-testfiles/{mdb_filename}"),
    )


def test_parse_attributes(sync_ldap_indexes, schema_file):
    expected_attributes = {"univentionObjectIdentifier": {"equality": "uuidMatch"}}
    parsed_attributes = sync_ldap_indexes.parse_attributes(schema_file)

    assert expected_attributes == parsed_attributes


def test_get_attributes_from_schemas(sync_ldap_indexes):
    file_path = f"{os.path.abspath(os.path.dirname(__file__))}/ldap-testfiles"
    expected_attributes = {
        "univentionObjectIdentifier": {
            "equality": "uuidMatch",
            "schema_file": f"{file_path}/test-ldap-schema.schema",
        },
        "univentionObjectIdentifier2": {
            "equality": "uuidMatch",
            "schema_file": f"{file_path}/test-ldap-schema-2.schema",
        },
    }
    parsed_attributes = sync_ldap_indexes.get_attributes_from_schemas([file_path])

    assert expected_attributes == parsed_attributes


def test_get_current_indexes(sync_ldap_indexes):
    expected_indexes = {
        "eq": {"univentionObjectIdentifier", "newAttrToIndex"},
        "pres": {"univentionObjectIdentifier", "newAttrToIndex"},
        "approx": {"univentionObjectIdentifier", "newAttrToIndex"},
        "sub": {"univentionObjectIdentifier", "newAttrToIndex"},
    }
    current_indexes = sync_ldap_indexes.get_current_indexes()

    assert expected_indexes == current_indexes


def test_get_changed_attributes(sync_ldap_indexes, schema_file):
    expected_attributes = ["univentionObjectIdentifier", "newAttrToIndex"]

    attributes = sync_ldap_indexes.parse_attributes(schema_file)
    indexes = sync_ldap_indexes.get_current_indexes()
    state = sync_ldap_indexes.get_state(indexes, attributes)

    base_dir = os.path.abspath(os.path.dirname(__file__))
    statefile_state = sync_ldap_indexes.read_state_file(Path(f"{base_dir}/ldap-testfiles/test-ldap-statefile.json"))

    changed_attributes = sync_ldap_indexes.get_changed_attributes(statefile_state, state)

    assert set(expected_attributes) == set(changed_attributes)


def test_broken_state_file(sync_ldap_indexes):
    base_dir = os.path.abspath(os.path.dirname(__file__))
    with pytest.raises(JSONDecodeError):
        sync_ldap_indexes.read_state_file(Path(f"{base_dir}/ldap-testfiles/test-ldap-statefile-broken.json"))


def test_main_function_broken_state_file(sync_ldap_indexes, caplog):
    test_config = get_test_config(
        sync_ldap_indexes,
        "test-ldap-statefile-broken.json",
        "data.mdb",
    )

    with caplog.at_level(logging.ERROR):
        sync_ldap_indexes.main(test_config)

    assert len(caplog.records) == 1

    assert caplog.records[0].levelno == logging.ERROR
    assert "Error reading state file" in caplog.records[0].message


def test_main_function_missing_state_file(sync_ldap_indexes, caplog):
    test_config = get_test_config(
        sync_ldap_indexes,
        "test-ldap-statefile-tmp.json",
        "data.mdb",
    )

    # Cleanup tmp state file
    if os.path.isfile(test_config.state_file_path):
        os.remove(test_config.state_file_path)

    with caplog.at_level(logging.INFO):
        sync_ldap_indexes.main(test_config)

    assert len(caplog.records) == 6

    assert caplog.records[1].levelno == logging.INFO
    assert "Missing state file! New state file from template created." == caplog.records[1].message

    assert caplog.records[2].levelno == logging.INFO
    assert caplog.records[2].message in (
        "Processing attribute newAttrToIndex.",
        "Processing attribute univentionObjectIdentifier.",
    )

    assert caplog.records[4].levelno == logging.INFO
    assert caplog.records[4].message in (
        "Processing attribute newAttrToIndex.",
        "Processing attribute univentionObjectIdentifier.",
    )


def test_main_function_empty_pv(sync_ldap_indexes, caplog):
    test_config = get_test_config(
        sync_ldap_indexes,
        "test-ldap-statefile-tmp.json",
        "data_tmp.mdb",
    )

    # Cleanup tmp state file
    if os.path.isfile(test_config.state_file_path):
        os.remove(test_config.state_file_path)

    with caplog.at_level(logging.INFO):
        sync_ldap_indexes.main(test_config)

    assert len(caplog.records) == 2

    assert caplog.records[1].levelno == logging.INFO
    assert "Empty persistent volume. New state file with current state created." == caplog.records[1].message
