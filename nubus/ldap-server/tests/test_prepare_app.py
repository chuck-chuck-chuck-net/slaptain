# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2024-2025 Univention GmbH

import pytest
from typer.testing import CliRunner

runner = CliRunner()


@pytest.fixture(autouse=True)
def mock_configure_kubernetes_client(evaluate_database_init, mocker):
    """Replace configure_kubernetes_client with a mock object."""
    mocker.patch.object(evaluate_database_init, "configure_kubernetes_client")


def test_configmap_provided_via_cli(evaluate_database_init):
    evaluate_database_init.prepare_app(configmap="stub_name", namespace="stub_namespace")
    assert evaluate_database_init.settings["configmap"] == "stub_name"


def test_configmap_is_provided_via_environment(evaluate_database_init, mocker):
    evaluate_database_init.app.command()(stub_command)
    mocker.patch.dict("os.environ", {"STATUS_CONFIGMAP": "stub_configmap"})

    result = runner.invoke(evaluate_database_init.app, ["--namespace", "stub_namespace", "stub-command"])

    assert result.exit_code == 0
    assert evaluate_database_init.settings["configmap"] == "stub_configmap"


def test_configures_logging(evaluate_database_init, mocker):
    setup_logging_mock = mocker.patch.object(evaluate_database_init, "setup_logging")

    evaluate_database_init.prepare_app(configmap="stub_name", namespace="stub_namespace", log_level="stub_log_level")

    setup_logging_mock.assert_called_once_with("stub_log_level")


def test_discovers_namespace_as_fallback(evaluate_database_init, mocker):
    dn_mock = mocker.patch.object(evaluate_database_init, "discover_namespace", return_value="stub_namespace")

    evaluate_database_init.prepare_app(configmap="stub_configmap")
    dn_mock.assert_called_once()
    assert evaluate_database_init.settings["namespace"] == "stub_namespace"


def test_uses_provided_namespace(evaluate_database_init, mocker):
    dn_mock = mocker.patch.object(evaluate_database_init, "discover_namespace")

    evaluate_database_init.prepare_app(configmap="stub_configmap", namespace="stub_namespace")
    dn_mock.assert_not_called()
    assert evaluate_database_init.settings["namespace"] == "stub_namespace"


def test_namespace_is_provided_via_environment(evaluate_database_init, mocker):
    evaluate_database_init.app.command()(stub_command)
    mocker.patch.dict("os.environ", {"STATUS_NAMESPACE": "stub_namespace"})

    result = runner.invoke(evaluate_database_init.app, ["--configmap", "stub_configmap", "stub-command"])

    assert result.exit_code == 0
    assert evaluate_database_init.settings["namespace"] == "stub_namespace"


def stub_command():
    pass
