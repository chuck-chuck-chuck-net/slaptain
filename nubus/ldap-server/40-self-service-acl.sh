#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2023-2025 Univention GmbH


set -euo pipefail

##############################################################################
# Generate the LDAP ACLs for the self-service from UCR variable.
#
# In UCS, this runs as a UCR hook observing `self-service/ldap_attributes`.
# This hook then writes the ACL to LDAP using UDM (`settings/ldapacl`).
# The Listener plugin will observe this change, generate the ACL snippet,
# regenerate slapd.conf and restart slapd.
#
# This flow is incompatible with container operation.
#
# Here we just generate the ACL on container startup and perform the little
# sanitization that `self-service-acl.py` would do: remove all whitespace
# around the LDAP attribute names
#
##############################################################################

LDAP_ATTRIBUTES=$(
    echo "@%@self-service/ldap_attributes@%@" \
      | ucr-light-filter \
      | sed --expression 's/[[:blank:]]*,[[:blank:]]*/,/g' \
      || echo ""
)

if [ -n "${LDAP_ATTRIBUTES:-}" ]; then
    {
        echo ""
        echo "access to filter=\"univentionObjectType=users/user\" attrs=${LDAP_ATTRIBUTES}"
        echo "    by self write"
        echo "    by * +0 break"
        echo ""
    } > /etc/univention/templates/files/etc/ldap/slapd.conf.d/64selfservice_userattributes.acl
fi
