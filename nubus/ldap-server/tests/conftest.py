# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2024-2025 Univention GmbH

import importlib.util
import os
import sys

import pytest


@pytest.fixture
def evaluate_database_init():
    """Provide "evaluate_database_init.py" as a module."""
    module_name = "evaluate_database_init"
    module_path = "./evaluate_database_init.py"
    spec = importlib.util.spec_from_file_location(
        module_name,
        os.path.join(module_path),
    )
    if not (spec and spec.loader):
        raise RuntimeError("Loading the script as module did fail")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@pytest.fixture
def stub_settings(evaluate_database_init, mocker):
    """
    Mock settings with a stub configuration.

    It does provide access to the injected settings as a `dict`.
    """
    settings = {
        "configmap": "stub_configmap",
        "namespace": "stub_namespace",
    }
    mocker.patch.dict(evaluate_database_init.settings, settings)
    return settings


@pytest.fixture
def client_mock(evaluate_database_init, mocker):
    """Mock of the Kubernetes client module."""
    return mocker.patch.object(evaluate_database_init, "client")


###############################


@pytest.fixture(autouse=True)
def mock_univention_config_registry_installed():
    class mock_ucr:
        def get(ldap_path: str):
            return "univentionObjectIdentifier,newAttrToIndex"

    module = type(sys)("univention")
    module.submodule = type(sys)("config_registry")
    module.submodule.ucr = mock_ucr
    sys.modules["univention"] = module
    sys.modules["univention.config_registry"] = module.submodule

    yield

    del sys.modules["univention"]
    del sys.modules["univention.config_registry"]

    # Create a dummy module using MagicMock.
    # mock_ucr = MagicMock()

    # Configure the dummy module's behavior.
    # mock_ucr.get.return_value = "univentionObjectIdentifier,newAttrToIndex"

    # Patch sys.modules to insert the dummy in place of 'univention.config_registry'
    # with patch.dict(sys.modules, {"univention.config_registry.ucr": mock_ucr}):
    #     yield mock_ucr  # The dummy remains active during the test.


@pytest.fixture
def schema_file():
    return """
    attributetype ( 1.3.6.1.4.1.10176.1003.3 NAME 'univentionObjectIdentifier'
        DESC 'ASCII string representation of a permanent IAM object identifier,
              like entryUUID or Active Directroy objectGUID'
        EQUALITY uuidMatch
        ORDERING UUIDOrderingMatch
        SYNTAX 1.3.6.1.1.16.1 SINGLE-VALUE )
    """


@pytest.fixture
def sync_ldap_indexes(mock_univention_config_registry_installed):
    """Provide "sync_ldap_indexes.py" as a module."""
    module_name = "sync_ldap_indexes"
    module_path = "./sync_ldap_indexes.py"
    spec = importlib.util.spec_from_file_location(
        module_name,
        os.path.join(module_path),
    )
    if not (spec and spec.loader):
        raise RuntimeError("Loading the script as module did fail")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)

    # Mock execute_slapindex method
    def mock_execute_slapindex(ldap_base_dn, attribute_name):
        return

    module.execute_slapindex = mock_execute_slapindex

    return module
