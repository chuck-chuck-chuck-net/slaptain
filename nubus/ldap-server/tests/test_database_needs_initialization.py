# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2024-2025 Univention GmbH

from unittest import mock

import pytest
from kubernetes.client.exceptions import ApiException


def test_uses_configuration_from_settings(evaluate_database_init, stub_settings, client_mock):
    stub_configmap = mock.Mock()
    stub_configmap.data = {"ldap_database_initialized": "uninitialized"}
    read_namespaced_config_map_mock = client_mock.CoreV1Api().read_namespaced_config_map
    read_namespaced_config_map_mock.return_value = stub_configmap

    evaluate_database_init.database_needs_initialization()

    read_namespaced_config_map_mock.assert_called_once_with(name="stub_configmap", namespace="stub_namespace")


def test_exit_code_1_if_already_initialized(evaluate_database_init, stub_settings, client_mock):
    stub_configmap = mock.Mock()
    stub_configmap.data = {"ldap_database_initialized": "initialized"}
    read_namespaced_config_map_mock = client_mock.CoreV1Api().read_namespaced_config_map
    read_namespaced_config_map_mock.return_value = stub_configmap

    with pytest.raises(SystemExit) as exit_info:
        evaluate_database_init.database_needs_initialization()

    assert exit_info.value.code == 1


def test_creates_status_config_map(evaluate_database_init, stub_settings, client_mock):
    read_namespaced_config_map_mock = client_mock.CoreV1Api().read_namespaced_config_map
    read_namespaced_config_map_mock.side_effect = ApiException(status=404, reason="Not Found")
    create_namespaced_config_map_mock = client_mock.CoreV1Api().create_namespaced_config_map

    evaluate_database_init.database_needs_initialization()

    expected_configmap = {
        "api_version": "v1",
        "metadata": {
            "name": stub_settings["configmap"],
            "labels": {
                "app.kubernetes.io/managed-by": "ldap-server-evaluate-database-init",
            },
        },
        "data": {
            evaluate_database_init.DATABASE_INITIALIZED_KEY: evaluate_database_init.InitializedEnum.UNINITIALIZED,
        },
    }
    create_namespaced_config_map_mock.assert_called_once_with(
        body=expected_configmap, namespace=stub_settings["namespace"]
    )
