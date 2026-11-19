# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2024-2025 Univention GmbH

from unittest import mock

import pytest


def test_raises_value_error_when_key_is_missing(evaluate_database_init, stub_settings, client_mock):
    stub_configmap = mock.Mock()
    stub_configmap.data = {}
    read_namespaced_config_map_mock = client_mock.CoreV1Api().read_namespaced_config_map
    read_namespaced_config_map_mock.return_value = stub_configmap

    with pytest.raises(ValueError):
        evaluate_database_init.get_validated_configmap()


def test_handles_empty_configmap_correct(evaluate_database_init, stub_settings, client_mock):
    stub_configmap = mock.Mock()
    stub_configmap.data = None
    read_namespaced_config_map_mock = client_mock.CoreV1Api().read_namespaced_config_map
    read_namespaced_config_map_mock.return_value = stub_configmap

    with pytest.raises(ValueError):
        evaluate_database_init.get_validated_configmap()
