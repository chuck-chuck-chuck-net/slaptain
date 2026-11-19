# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2025 Univention GmbH

import importlib.util
import os

import pytest


@pytest.fixture
def ucr_light_filter():
    """Provide "evaluate_database_init.py" as a module."""
    module_name = "ucr_light_filter"
    module_path = "./ucr_light_filter.py"
    spec = importlib.util.spec_from_file_location(
        module_name,
        os.path.join(module_path),
    )
    if not (spec and spec.loader):
        raise RuntimeError("Loading the script as module did fail")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@pytest.mark.parametrize("input", ["", "foobar", "foo\nbar"])
def test_noop_template(ucr_light_filter, input):
    result = ucr_light_filter.run_filter(input, {}).decode("UTF-8")
    assert result.rstrip() == input


def test_variable_template(ucr_light_filter):
    ucr_light_filter.config_registry = {"sambadomain": "stubdomain"}
    result = ucr_light_filter.run_filter("dn: @%@sambadomain@%@;dc: foobar", ucr_light_filter.config_registry).decode(
        "UTF-8"
    )
    assert result == "dn: stubdomain;dc: foobar\n"


@pytest.mark.parametrize(
    "input, output",
    [
        ("@!@\n1 == 1\n@!@", "\n"),
        ("@!@\nprint('dn: with_newlines')\n@!@", "dn: with_newlines\n\n"),
        ("@!@print('dn: no_newline_before')\n@!@", "dn: no_newline_before\n\n"),
        ("@!@\nprint('dn: no_newline_after')@!@", "dn: no_newline_after\n\n"),
        # ("@!@print('dn: no_newlines')@!@", "dn: no_newlines\n\n"),
        ("something before @!@print('uid: data_before')\n@!@", "something before uid: data_before\n\n"),
        # ("@!@\nprint('uid: data_after')@!@ something after", "uid: data_after something after\n\n"),
        (
            "@!@\nprint('first line')\nprint('second line')\nprint('third line')\n@!@",
            "first line\nsecond line\nthird line\n\n",
        ),
    ],
)
def test_valid_embedded_python_template(ucr_light_filter, input, output):
    ucr_light_filter.config_registry = {"sambadomain": "stubdomain"}
    result = ucr_light_filter.run_filter(input, ucr_light_filter.config_registry).decode("UTF-8")
    assert result == output


@pytest.mark.parametrize(
    "input, output",
    [
        # TODO: Raise an error if the embedded python is broken.
        ("@!@\n1 == 2\n@!@", "\n")
    ],
)
def test_invalid_embedded_python_template(ucr_light_filter, input, output):
    ucr_light_filter.config_registry = {"sambadomain": "stubdomain"}
    result = ucr_light_filter.run_filter(input, ucr_light_filter.config_registry).decode("UTF-8")
    assert result == output
