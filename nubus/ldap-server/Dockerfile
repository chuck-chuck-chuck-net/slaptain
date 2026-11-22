# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2023-2025 Univention GmbH


###############################################################################
# Global variables
###############################################################################

ARG UCS_BASE_IMAGE_TAG=5.3.0-build.20261112@sha256:519c50da25332623a7a30d911965f13cd333e4b9b1402036e5bb4dd85d98f6c6
ARG UCS_BASE_IMAGE=gitregistry.knut.univention.de/univention/dev/projects/ucs-base-image/ucs-base
# Sources for the slaptain targets at the end. The slapd-init image must carry the bootstrap.d hook.
ARG SLAPTAIN_INIT_IMAGE=ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-init:0.5.0-ol26
ARG OPENLDAP_POOL=https://updates-test.software-univention.de/pool/main/o/openldap
ARG OPENLDAP_ORIG=openldap_2.6.10+dfsg.orig.tar.xz
ARG OPENLDAP_ORIG_SHA256=e871102bda1e42155fd4d6be4825a297e1c593cb0907b84fc7dde888dc847877
ARG OPENLDAP_DEBIAN=openldap_2.6.10+dfsg-1A~5.3.0.202610131836.debian.tar.xz
ARG OPENLDAP_DEBIAN_SHA256=4fdf58720bb206b1c2029d9750a78799fc14b6dfcd709972bc064f0526683897
ARG PORTAL_EXTENSION_IMAGE=artifacts.software-univention.de/nubus/images/portal-extension:0.95.7@sha256:2c42d4240e2876ec52e1a722f704f3dad10401040920206f5cec43b0c673e6f4

###############################################################################
# Builder image for collecting Univention LDAP schema templates, base LDIFs
# and certain libraries that are maintained by Univention for UCS
# like cy2-saml
###############################################################################

FROM ${UCS_BASE_IMAGE}:${UCS_BASE_IMAGE_TAG} AS ucs_builder

SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

###############################################################################
# Install univention-ldap-server
###############################################################################

WORKDIR /home/deb_packages


RUN apt-get download \
    # The Self-service does not have an extension package. It's part of the UMC.
    # We bake support for it directly into the LDAP server image.
    univention-self-service-passwordreset-umc \
    univention-ldap-config \
    univention-ldap-server \
    univention-ldap-acl-master \
    univention-management-console-server \
    liblasso3t64 \
    libsasl2-modules-oauthbearer \
    cy2-saml && \
    (dpkg --force-all -i ./*.deb || true)

###############################################################################
# Base image
###############################################################################
FROM ${UCS_BASE_IMAGE}:${UCS_BASE_IMAGE_TAG} AS base

ARG LABEL_CREATED=undefined
ARG LABEL_REVISION=undefined
ARG LABEL_SOURCE=undefined
ARG LABEL_VERSION=undefined

SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

# hadolint ignore=DL3008
RUN \
    # Do the non-interactive package configuration
    # of slapd otherwise the build process would
    # stall when debconf asks for input interactively
    { \
    echo debconf debconf/frontend select Noninteractive; \
    # Administrator password
    echo slapd slapd/password1 password 'temppwd'; \
    # Confirm password:
    echo slapd slapd/password2 password 'temppwd'; \
    } | debconf-set-selections && \
    DEBIAN_FRONTEND=noninteractive \
    apt-get --assume-yes --verbose-versions --no-install-recommends install \
    # Slapd Debian dependencies, some of them could be removed later
    libltdl7 \
    libodbc2 \
    libperl5.40 \
    libwrap0 \
    psmisc \
    perl \
    lsb-base \
    # Some definitely needed slapd dependencies
    libsasl2-2 \
    libsasl2-modules \
    libsasl2-modules-db \
    libsasl2-modules-gssapi-heimdal \
    libgssapi-krb5-2 \
    libkrb5-26t64-heimdal \
    libkadm5srv8t64-heimdal \
    openssl \
    # cy2-saml dependencies
    libglib2.0-0t64 \
    libxmlsec1t64-openssl \
    # libsasl2-modules-oauthbearer dependencies
    libjansson4 \
    librhonabwy1.1t64 \
    # for downloading SAML metadata on startup
    wget \
    ca-certificates \
    python3 \
    python3-pip \
    pipenv && \
    # This file is a dependency of the slapd.conf templates
    mkdir -p /var/lib/univention-ldap-local/schema/id/ && \
    echo "1" >/var/lib/univention-ldap-local/schema/id/id

###############################################################################
# Copy LDAP config and libs from UCS builder image
###############################################################################
# slapd.conf templates:
COPY --from=ucs_builder /usr/share/univention-ldap/schema \
    /usr/share/univention-ldap/schema
COPY --from=ucs_builder /etc/univention/templates/files/etc/ldap/slapd.conf.d \
    /etc/univention/templates/files/etc/ldap/slapd.conf.d
# This "info" file is also a dependency of the templates
COPY --from=ucs_builder /etc/univention/templates/info/univention-ldap-server.info \
    /etc/univention/templates/info/univention-ldap-server.info

# LDIF Files:
COPY --from=ucs_builder /usr/share/univention-ldap/base.ldif \
    /usr/share/univention-ldap/base.ldif
COPY --from=ucs_builder /usr/share/univention-ldap/core-edition.ldif \
    /usr/share/univention-ldap/core-edition.ldif
COPY --from=ucs_builder /usr/share/univention-ldap/translog.ldif \
    /usr/share/univention-ldap/translog.ldif

# local-schema files:
COPY --from=ucs_builder \
    /usr/share/univention-self-service/self-service-passwordreset.schema \
    /var/lib/univention-ldap-local/local-schema/self-service-passwordreset.schema

# Configuration of the Crude OIDC plugin for OAUTHBEARER and Crude SAML plugin for Cyrus SASL2:
# This template is contained by univention-management-console-server
COPY --from=ucs_builder \
    /etc/univention/templates/files/etc/ldap/sasl2/slapd.conf \
    /etc/univention/templates/files/etc/ldap/sasl2/slapd.conf

# UCS only libraries:
COPY --from=ucs_builder \
    /usr/lib/x86_64-linux-gnu/sasl2/libsaml.so.0.2.0 \
    /usr/lib/x86_64-linux-gnu/sasl2/
COPY --from=ucs_builder \
    /usr/lib/x86_64-linux-gnu/sasl2/liboauthbearer.so \
    /usr/lib/x86_64-linux-gnu/sasl2/

# Only the UCS version of this library fits as libsaml linked against this
# and otherwise dlopen(,) complains about 'undefined symbol'
COPY --from=ucs_builder /usr/lib/liblasso.so.3.15.2  /usr/lib/

# ldconfig doesn't create the following links as this directory
# is not in any config under /etc/ld.so.conf.d/
RUN ln -s /usr/lib/x86_64-linux-gnu/sasl2/libsaml.so.0.2.0 \
    /usr/lib/x86_64-linux-gnu/sasl2/libsaml.so.0 \
    \
    && ln -s /usr/lib/x86_64-linux-gnu/sasl2/libsaml.so.0.2.0 \
    /usr/lib/x86_64-linux-gnu/sasl2/libsaml.so \
    \
    # The slapd postinst script starts the daemon directly via
    # /etc/init.d/slapd (not through invoke-rc.d), so it ignores
    # policy-rc.d. It needs to bind the privileged LDAP port, which
    # rootless buildkit's sandbox does not allow. Divert the init
    # script to a no-op for the duration of the install to prevent
    # the postinst from actually starting the daemon during the build.
    && mkdir -p /etc/init.d \
    && dpkg-divert --local --rename --add /etc/init.d/slapd \
    && ln -s /bin/true /etc/init.d/slapd \
    && apt-get --assume-yes --verbose-versions --no-install-recommends install \
    slapd \
    slapd-contrib \
    ldap-utils \
    && rm /etc/init.d/slapd \
    && dpkg-divert --local --rename --remove /etc/init.d/slapd \
    # Explicitly disable cn=config, so
    # slapcat defaults to reading the config from
    # /etc/ldap/slapd.conf
    # this is the current behavior in UCS 5
    && mv "/etc/ldap/slapd.d/cn=config.ldif" \
    "/etc/ldap/slapd.d/cn=config.ldif.DISABLED" \
    && mkdir /var/lib/univention-ldap \
    # Some file ownerships are explicitly changed to the user openldap with uid 100
    # in the debian installation scripts.
    # This leads to errors when running the container as any other user and needs to be reversed.
    && chown -R 100:102 /etc/ldap/

###############################################################################
# Copy LDAP index tools
###############################################################################
RUN mkdir -p /opt/univention/ldap-tools
COPY sync_ldap_indexes.py /opt/univention/ldap-tools
COPY ldap-index-statefile.json /opt/univention/ldap-tools
RUN ln -s /opt/univention/ldap-tools/sync_ldap_indexes.py /usr/bin/sync-ldap-indexes

###############################################################################
# Copy from Docker build context
###############################################################################
WORKDIR /src
COPY Pipfile Pipfile.lock ./

###############################################################################
# Test image
###############################################################################
FROM base AS pytest

# trixie's pipenv stops at PEP 668 and at Debian-installed packages it cannot uninstall.
RUN PIP_BREAK_SYSTEM_PACKAGES=1 PIP_IGNORE_INSTALLED=1 pipenv sync --dev --system \
    && rm -r ~/.cache

WORKDIR /src/docker/ldap-server

CMD ["pytest"]


###############################################################################
# Final image
###############################################################################
FROM base AS final

# trixie's pipenv stops at PEP 668 and at Debian-installed packages it cannot uninstall.
RUN PIP_BREAK_SYSTEM_PACKAGES=1 PIP_IGNORE_INSTALLED=1 pipenv sync --system \
    && rm -r ~/.cache

# bind9 arrives as a hard dependency of python3-univention-lib, which only calls
# dig for AD member mode. This has to stay the last dpkg operation in the stage.
RUN dpkg --purge --force-depends bind9-dnsutils bind9-host bind9-libs

WORKDIR /
COPY ucr /usr/sbin/
COPY ucr_light_filter.py /usr/sbin/ucr-light-filter

# UCR defaults the image owns, overridable by a mounted base.conf
COPY base-defaults.conf /etc/univention/base-defaults.conf

COPY 40-self-service-acl.sh \
    50-entrypoint.sh \
    /entrypoint.d/

COPY evaluate_database_init.py /usr/local/bin/evaluate_database_init

CMD ["/etc/univention/run-slapd.sh"]

# Since in the classic UCS the ports 7636 and 7389 were only needed
# for avoiding conflict with Samba domain controller service
# they are not needed in this image
EXPOSE 389 636

LABEL org.opencontainers.image.created="${LABEL_CREATED}"
LABEL org.opencontainers.image.description="Univention OpenLDAP Server"
LABEL org.opencontainers.image.licenses="AGPL-3.0-or-later"
LABEL org.opencontainers.image.revision="${LABEL_REVISION}"
LABEL org.opencontainers.image.source="${LABEL_SOURCE}"
LABEL org.opencontainers.image.title="upx-ldap-server"
LABEL org.opencontainers.image.url="https://docs.software-univention.de/manual-5.0.html#introduction:LDAP_directory_service"
LABEL org.opencontainers.image.vendor="Univention GmbH"
LABEL org.opencontainers.image.version="${LABEL_VERSION}"


###############################################################################
# slaptain: the same image as the slapd and init containers of a SlapdCluster
###############################################################################

# translog reads its file name only from slapd.conf, which slaptain does not use.
# Rebuild it from the UCS source of the installed slapd, with the UCS default file name built in.
FROM ${UCS_BASE_IMAGE}:${UCS_BASE_IMAGE_TAG} AS translog_builder
ARG OPENLDAP_POOL
ARG OPENLDAP_ORIG
ARG OPENLDAP_ORIG_SHA256
ARG OPENLDAP_DEBIAN
ARG OPENLDAP_DEBIAN_SHA256
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
# hadolint ignore=DL3008
RUN apt-get --assume-yes --no-install-recommends install \
    build-essential \
    ca-certificates \
    curl \
    quilt \
    xz-utils \
    pkgconf \
    # configure checks for these with the Debian options
    libargon2-dev \
    libltdl-dev \
    libperl-dev \
    libsasl2-dev \
    libssl-dev \
    libsystemd-dev \
    libwrap0-dev \
    unixodbc-dev
WORKDIR /build
COPY translog-default-file.patch ./
RUN curl -fsSLO "${OPENLDAP_POOL}/${OPENLDAP_ORIG}" \
    && curl -fsSLO "${OPENLDAP_POOL}/${OPENLDAP_DEBIAN}" \
    && printf '%s  %s\n' \
        "${OPENLDAP_ORIG_SHA256}" "${OPENLDAP_ORIG}" \
        "${OPENLDAP_DEBIAN_SHA256}" "${OPENLDAP_DEBIAN}" | sha256sum -c - \
    && mkdir src \
    && tar -C src --strip-components=1 -xf "${OPENLDAP_ORIG}" \
    && tar -C src -xf "${OPENLDAP_DEBIAN}"
WORKDIR /build/src
# configure.options quotes '${prefix}/lib' for make, so the shell has to expand it.
# gcc 14 rejects translog's memrchr call without _GNU_SOURCE.
RUN QUILT_PATCHES=debian/patches quilt push -a -q \
    && patch -p1 <../translog-default-file.patch \
    && export CPPFLAGS=-D_GNU_SOURCE \
    && eval ./configure --prefix=/usr --sysconfdir=/etc --localstatedir=/var \
        "$(grep -v '^#' debian/configure.options | tr '\n' ' ')" >/dev/null \
    && make -j"$(nproc)" depend >/dev/null \
    && make -j"$(nproc)" -C include >/dev/null \
    && make -j"$(nproc)" -C libraries >/dev/null \
    && make -C servers/slapd/overlays version.lo translog.la

FROM ${SLAPTAIN_INIT_IMAGE} AS slaptain_init_source
FROM ${PORTAL_EXTENSION_IMAGE} AS portal_extension

# slaptain passes only -h, -d and -F, as UID 1024 on a read-only root filesystem.
FROM final AS slaptain-slapd
COPY --from=translog_builder /build/src/servers/slapd/overlays/.libs/translog.so.2.0.200 /usr/lib/ldap/
USER 1024:1024
ENTRYPOINT ["/usr/sbin/slapd"]
CMD []

FROM final AS slaptain-init
COPY --from=translog_builder /build/src/servers/slapd/overlays/.libs/translog.so.2.0.200 /usr/lib/ldap/
COPY --from=slaptain_init_source \
    /usr/local/bin/bootstrap.sh \
    /usr/local/bin/startup_config.py \
    /usr/local/bin/
# The Nubus chart copies the portal schema in at runtime; slaptain needs it at bootstrap.
COPY --from=portal_extension /plugins/ldap-schema/univention-portal.schema \
    /var/lib/univention-ldap-local/local-schema/
COPY slaptain-bootstrap.conf /etc/ldap/bootstrap.d/50-nubus.conf
USER 1024:1024
ENTRYPOINT ["/usr/local/bin/bootstrap.sh"]
CMD []

# [EOF]
