#!/usr/bin/python3
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2023-2024 Univention GmbH

import glob
import json
import logging
import os
import re
import shutil
import subprocess
from datetime import datetime
from pathlib import Path
from pprint import pformat
from typing import NamedTuple

from univention.config_registry import ucr

logger = logging.getLogger(__name__)


class Config(NamedTuple):
    log_level: int | str
    ldap_base_dn: str
    schema_dirs: list[str] = ["/usr/share/univention-ldap/schema", "/etc/ldap/schema"]
    state_file_path: Path = Path("/var/lib/univention-ldap/ldap-index-statefile.json")
    state_file_template_path: Path = Path("/opt/univention/ldap-tools/ldap-index-statefile.json")
    mdb_file_path: Path = Path("/var/lib/univention-ldap/ldap/data.mdb")


def get_config() -> Config:
    ldap_base_dn = os.environ.get("LDAP_BASEDN")
    if not ldap_base_dn:
        raise ValueError("Missing environment variable: LDAP_BASEDN")
    log_level = os.environ.get("PYTHON_LOG_LEVEL")
    if not log_level:
        raise ValueError("Missing environment variable: PYTHON_LOG_LEVEL")
    return Config(
        log_level=log_level,
        ldap_base_dn=ldap_base_dn,
    )


class NoIndexException(Exception): ...


class SlapindexException(Exception): ...


def setup_logging(level: str | int):
    log_format = "%(asctime)s %(levelname)-5s [%(module)s.%(funcName)s:%(lineno)d] %(message)s"
    logging.basicConfig(format=log_format, level=level)
    global logger
    logger = logging.getLogger(__name__)


def parse_attributes(file_content: str) -> dict:
    """
    Parses the given string for the LDAP attributetype settings and returns
    the name and equality parameters as dictionary.

    attributetype example:
    ----------------------
    attributetype ( 1.3.6.1.4.1.10176.1003.3 NAME 'univentionObjectIdentifier'
        DESC 'ASCII string representation of a permanent IAM object identifier,
              like entryUUID or Active Directory objectGUID'
        EQUALITY uuidMatch
        ORDERING UUIDOrderingMatch
        SYNTAX 1.3.6.1.1.16.1 SINGLE-VALUE )
    """
    attributes = {}

    pattern = r"attributetype\s+\((.+)((?:\n.+)+)\)"
    matches = re.findall(pattern, file_content, re.MULTILINE)

    for mg in matches:
        attribute_name = None
        equality = None

        for m in mg:
            pattern = r".+NAME\s+\'(.*)\'"
            match = re.search(pattern, m)
            if match:
                attribute_name = match[1]

            pattern = r".+EQUALITY\s+([\w]+)"
            match = re.search(pattern, m)
            if match:
                equality = match[1]

        if attribute_name and equality:
            attributes[attribute_name] = {
                "equality": equality,
            }

    return attributes


def get_current_indexes() -> dict:
    """
    Returns all configured indexes from UCR.
    """
    index_types = ["eq", "pres", "approx", "sub"]
    current_indexes = {}

    for index_type in index_types:
        indexes = ucr.get(f"ldap/index/{index_type}")
        current_indexes[index_type] = set(indexes.split(","))

    return current_indexes


def get_attributes_from_schemas(schema_dirs: list) -> dict:
    """
    Returns attributes with equality from all attributetype definitionsschema files.

    attributetype example:
    ----------------------
    attributetype ( 1.3.6.1.4.1.10176.1003.3 NAME 'univentionObjectIdentifier'
        DESC 'ASCII string representation of a permanent IAM object identifier,
              like entryUUID or Active Directory objectGUID'
        EQUALITY uuidMatch
        ORDERING UUIDOrderingMatch
        SYNTAX 1.3.6.1.1.16.1 SINGLE-VALUE )
    """
    all_attributes = {}

    for schema_dir in schema_dirs:
        for schema_file in glob.glob(f"{schema_dir}/*.schema"):
            with open(os.path.join(schema_dir, schema_file)) as f:
                file_content = f.read()

                attributes = parse_attributes(file_content=file_content)
                for attr_value in attributes.values():
                    attr_value["schema_file"] = schema_file

                all_attributes |= attributes

    return all_attributes


def get_state(current_indexes: dict, attributes: dict) -> dict:
    """
    Returns state from the current config.
    """
    state = {"attributes": {}}

    # Populate state with index information.
    for index_type, index_attributes in current_indexes.items():
        for index_attribute in index_attributes:
            if not state["attributes"].get(index_attribute):
                state["attributes"][index_attribute] = {"indexes": []}
            state["attributes"][index_attribute]["indexes"].append(
                {
                    "type": index_type,
                    "last_index_date": datetime.now().isoformat(),
                }
            )

    # Populate state with attribute information.
    for attr_name, attr_val in attributes.items():
        if state.get("attributes", {}).get(attr_name) and attr_val:
            state["attributes"][attr_name]["equality"] = attr_val.get("equality")
            state["attributes"][attr_name]["schema_file"] = attr_val.get("schema_file")

    logger.debug("current state:\n%s", pformat(state))
    return state


def read_state_file(state_file_path: Path) -> dict:
    """
    Returns state from the state file.
    """
    with state_file_path.open("r") as f:
        file_content = f.read()

    state = json.loads(file_content)
    logger.debug("state from state file:\n%s", pformat(state))
    return state


def write_state_file(state_file_path: Path, state: dict):
    state_json = json.dumps(dict(sorted(state.items())), indent=4)
    with state_file_path.open("w") as f:
        f.write(state_json)


def attribute_needs_reindexing(new: dict, old: dict) -> bool:
    if not new or not old:
        return True
    if new.get("equality") != old.get("equality"):
        return True
    try:
        new_indexes = {i["type"] for i in new.get("indexes", [])}
        old_indexes = {i["type"] for i in old.get("indexes", [])}
    except KeyError:
        logger.warning("Invalid state file data for an index. Forcing a reindex for this attribute.")
        return False
    return new_indexes != old_indexes


def get_changed_attributes(new_state: dict, old_state: dict) -> list:
    differences = []
    new_attributes = new_state["attributes"]
    old_attributes = old_state["attributes"]

    for attribute in set(new_attributes) | set(old_attributes):
        if attribute_needs_reindexing(
            new_attributes.get(attribute, {}),
            old_attributes.get(attribute, {}),
        ):
            differences.append(attribute)
    return differences


def run_slapindex(ldap_base_dn: str, attribute_name: str):
    command = f'slapindex -b "{ldap_base_dn}" {attribute_name}'
    ans = subprocess.run(command, shell=True, executable="/bin/bash", capture_output=True, text=True)
    if ans.returncode == 0:
        logger.info("Successful index update: %s [%s]", command, ans.stdout or "-")
        return

    if ans.stderr.find(f"mdb_tool_entry_reindex: no index configured for {attribute_name}") > -1:
        raise NoIndexException(f"Index for {attribute_name} is currently not configured.")
    else:
        raise SlapindexException(
            f"Encountered an error while running slapindex with the following command: {command}: {ans.stderr}"
        )


def main(config: Config):
    # Check ldap installation state.
    setup_logging(config.log_level)
    logger.info("Checking if ldap indexes need to be updated")
    empty_persistent_volume = not any((config.mdb_file_path.is_file(), config.state_file_path.is_file()))
    missing_state_file = not any((empty_persistent_volume, config.state_file_path.is_file()))
    logger.debug("empty_persistent_volume: %s", empty_persistent_volume)
    logger.debug("missing_state_file: %s", missing_state_file)

    # Check and create state file
    if missing_state_file:
        shutil.copyfile(config.state_file_template_path, config.state_file_path)
        logger.info("Missing state file! New state file from template created.")

    # Get current state from ucr.
    current_state = get_state(
        current_indexes=get_current_indexes(),
        attributes=get_attributes_from_schemas(config.schema_dirs),
    )

    # On a clean installation, the state file is written with the current state.
    if empty_persistent_volume:
        write_state_file(config.state_file_path, current_state)
        logger.info("Empty persistent volume. New state file with current state created.")
        return

    # Read state file
    try:
        state_file = read_state_file(config.state_file_path)
    except json.decoder.JSONDecodeError as e:
        logger.error("Error reading state file: %s.", e)
        return

    # Check changed attributes.
    try:
        changed_attributes = get_changed_attributes(new_state=state_file, old_state=current_state)
    except KeyError:
        logger.error("Invalid state file schema! Until this is manually fixed, no ldap index changes can be evaluated.")
        return
    if not changed_attributes:
        logger.info("No index configuration changes or attribute syntax configuration changes detected. Nothing to do")
        return

    # Execute slapindex for each changed attribute.
    for attribute_name in changed_attributes:
        logger.info("Processing attribute %s.", attribute_name)
        try:
            run_slapindex(config.ldap_base_dn, attribute_name)
        except SlapindexException as e:
            logger.error(e)
        except NoIndexException as e:
            # If no index exists for an attribute,
            # the index is deleted from state.
            logger.info("%s Index is deleted from state file.", e)
            state_file["attributes"].pop(attribute_name)
        else:
            # Update state
            state_file["attributes"][attribute_name] = current_state["attributes"][attribute_name]

    # Write current state to state file.
    logger.debug("new state:\n%s", pformat(state_file))
    write_state_file(config.state_file_path, state_file)


if __name__ == "__main__":
    main(get_config())
