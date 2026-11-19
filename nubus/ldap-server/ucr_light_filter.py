#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# pylint: disable=invalid-name
#
# Univention Config Registry
#
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2024-2025 Univention GmbH

"""Replacement for "univention-config-registry" get and filter"""

# included
import collections
import io
import os
import re
import sys


class Registry(collections.UserDict):  # pylint: disable=too-many-ancestors
    """Container-class to hold UCR items"""

    def add_file(self, filename):
        """Read the given UCR configuration file and add the settings to the
        registry."""
        try:
            with open(filename, mode="r", encoding="utf-8") as fd:
                for line in fd.readlines():
                    if ":" in line:
                        key, value = line.split(":", 1)
                        self[key] = value.strip()
        except FileNotFoundError:
            sys.stderr.write("Config file does not exist: " + filename + "\n")

    def get_int(self, key, default=None, value=None):
        """Get item from value-param, data-dict or default and
        convert it to an integer"""
        if value is None:
            value = self.data.get(key)
        return int(value) if value is not None else default

    def is_true(self, key, default=None, value=None):
        """Get item from value-param, data-dict or default and
        compare it to strings which stand for True"""
        if value is None:
            value = self.data.get(key)  # type: ignore
            if value is None:
                return default
        return value.lower() in (
            "yes",
            "true",
            "1",
            "enable",
            "enabled",
            "on",
        )

    def is_false(self, key, default=None, value=None):
        """Get item from value-param, data-dict or default and
        compare it to strings which stand for False"""
        if value is None:
            value = self.data.get(key)  # type: ignore
            if value is None:
                return default
        return value.lower() in (
            "no",
            "false",
            "0",
            "disable",
            "disabled",
            "off",
        )


config_registry = Registry()
# load the three layers of UCR which can be filled via ConfigMaps in K8s
config_registry.add_file("/etc/univention/base-defaults.conf")
config_registry.add_file("/etc/univention/base.conf")
config_registry.add_file("/etc/univention/base-forced.conf")

# TODO: Eventually admit that it's not
#       univention-config-registry anymore
WARNING_TEXT = """\
{prefix}Warning: This file is auto-generated and might be overwritten by
{prefix}         univention-config-registry.
{prefix}         Please edit the following file(s) instead:
{prefix}Warnung: Diese Datei wurde automatisch generiert und kann durch
{prefix}         univention-config-registry ueberschrieben werden.
{prefix}         Bitte bearbeiten Sie an Stelle dessen die folgende(n) Datei(en):
{prefix}
"""  # noqa: E101


# TODO: Check if full porting of warning_string() is needed
def warning_string(prefix="# "):
    """Print out a warning message and all filenames from the template-dir"""
    string = WARNING_TEXT.format(prefix=prefix)
    path = "/etc/univention/templates/files/etc/ldap/slapd.conf.d/"
    for file_name in sorted(os.listdir(path)):
        file_path = os.path.join(path, file_name)
        string += f"{prefix}\t{file_path}\n"
    return string


def resolve_variable(line):
    """Replaces a UCR-Template-variable with a value from UCR"""
    VARIABLE_PATTERN = re.compile("@%@([^@]+)@%@")

    def _replace(x):
        key = x.group(1)
        if key.startswith("UCRWARNING="):
            return warning_string(key.replace("UCRWARNING=", ""))
        return config_registry[key]

    return VARIABLE_PATTERN.sub(_replace, line)


# TODO: Complete this
def custom_groupname(value):
    """Stub function for UCR-Templates"""
    return value


# TODO: Complete this
def custom_username(value):
    """Stub function for UCR-Templates"""
    return value


def run_filter(template, directory):
    """Parses UCR-Templates from stdin and calls exec on embedded code"""
    inside_section = False
    to_be_compiled = []

    buf = io.StringIO()
    for line in template.splitlines():
        line += "\n"
        if "@%@" in line:
            line = resolve_variable(line)

        if "@!@" in line:
            if line.count("@!@") > 1:
                raise NotImplementedError("only one `@!@` delimiter is allowed per line")
            before, after = line.split("@!@")
            if not inside_section:
                print(before, file=buf, end="")
                to_be_compiled.append(after)
                inside_section = True

            else:
                to_be_compiled.append(before)

                def print_wrapper(*_args, **kwargs):
                    kwargs.setdefault("file", buf)
                    print(*_args, **kwargs)

                inside_section = False
                exec(  # pylint: disable=exec-used
                    "".join(to_be_compiled),
                    {
                        "print": print_wrapper,
                        "custom_groupname": custom_groupname,
                        "custom_username": custom_username,
                        "configRegistry": directory,
                        "run_filter": run_filter,
                    },
                )
                # print("", file=buf)
                to_be_compiled = []
                print(after, file=buf, end="")

        elif inside_section:
            # Workaround for some import that doesn't do much anyway
            # TODO: Complete this, if needed
            if line in (
                "from univention.config_registry.handler import run_filter\n",
                "from univention.lib.misc import custom_groupname\n",
                "from univention.lib.misc import custom_username, custom_groupname\n",
            ):
                continue
            to_be_compiled += line

        else:
            print(line, file=buf, end="")

    # This function must return a decodable byte array
    # in order to maintain compatibilty with the
    # 25univention-ldap-server_local-schema fragment
    # which invokes this function and tries to decode the result
    return buf.getvalue().encode("UTF-8")


def main():
    """main - what else, pylint?"""
    print(run_filter(sys.stdin.read(), config_registry).decode("UTF-8"))


if __name__ == "__main__":
    main()
