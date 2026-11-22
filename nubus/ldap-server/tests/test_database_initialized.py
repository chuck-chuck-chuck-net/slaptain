# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2024-2025 Univention GmbH

from unittest import mock


def test_uses_configuration_from_settings(evaluate_database_init, stub_settings, client_mock, mocker):
    client_mock = mocker.patch.object(evaluate_database_init, "client")

    stub_configmap = mock.Mock()
    stub_configmap.data = {"ldap_database_initialized": "uninitialized"}
    read_namespaced_config_map_mock = client_mock.CoreV1Api().read_namespaced_config_map
    read_namespaced_config_map_mock.return_value = stub_configmap

    replace_namespaced_config_map_mock = client_mock.CoreV1Api().replace_namespaced_config_map

    evaluate_database_init.database_initialized()

    assert stub_configmap.data["ldap_database_initialized"] == "initialized"
    replace_namespaced_config_map_mock.assert_called_once_with(
        name="stub_configmap", namespace="stub_namespace", body=stub_configmap
    )
