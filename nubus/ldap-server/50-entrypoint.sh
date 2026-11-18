#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2023-2025 Univention GmbH


set -euo pipefail

setup_paths() {
  mkdir --parents /var/lib/univention-ldap/internal
  mkdir --parents /var/lib/univention-ldap/ldap
  mkdir --parents /var/lib/univention-ldap/translog
  mkdir --parents /var/lib/univention-ldap/notify
  touch /var/lib/univention-ldap/notify/transaction
  if [ ! -e /var/run/slapd ]
  then
    mkdir /var/run/slapd
    chown openldap:openldap /var/run/slapd
  fi
}

setup_symlinks() {
  # Some files from the local image are expected under
  # /var/lib/univention-ldap, using symlinks to make them available.
  if [ ! -L /var/lib/univention-ldap/local-schema ]
  then
    ln --symbolic /var/lib/univention-ldap-local/local-schema /var/lib/univention-ldap/
  fi
  if [ ! -L /var/lib/univention-ldap/schema ]
  then
    ln --symbolic /var/lib/univention-ldap-local/schema /var/lib/univention-ldap/
  fi
}

setup_listener_path() {
  # TODO: Shouldn't the translog overlay be able to do this?
  mkdir --parents /var/lib/univention-ldap/listener/
  touch /var/lib/univention-ldap/listener/listener
}

setup_last_id_path() {
  # If the last_id file exists, then it should never be empty
  # otherwise the translog overlay gets stuck at ID -1
  touch /var/lib/univention-ldap/last_id
  if [[ ! -s /var/lib/univention-ldap/last_id ]]; then
    echo "Setting 'last_id' to '0' because it is empty"
    echo -n '0' > /var/lib/univention-ldap/last_id
  fi
}

setup_slapd_conf() {
  cat /etc/univention/templates/files/etc/ldap/slapd.conf.d/* \
    | ucr-light-filter > /etc/ldap/slapd.conf
}

fetch_saml_metadata() {
  mkdir -p /usr/share/saml/idp

  SAML_METADATA_URL=$(echo -n -e '@%@umc/saml/idp-server@%@' | ucr-light-filter || true)
  SAML_METADATA_URL_INTERNAL=$(echo -n -e '@%@umc/saml/idp-server-internal@%@' | ucr-light-filter || true)

  if [[ -n "${SAML_METADATA_URL:-}" ]]; then
    DOWNLOAD_URL=${SAML_METADATA_URL_INTERNAL:-${SAML_METADATA_URL}}
    SAML_HOST=$(echo "${SAML_METADATA_URL}" | awk -F/ '{print $3}')

    SAML_METADATA_BASE=/usr/share/saml/idp
    SAML_METADATA_PATH="${SAML_METADATA_BASE}/${SAML_HOST}.xml"

    echo "Trying to fetch SAML metadata from ${DOWNLOAD_URL}"
    result=1
    counter=3
    # 'Connection refused' is not retried by `wget --tries=X` hence the loop
    while [[ ${result} -gt 0 && ${counter} -gt 0 ]]; do
      {
          wget \
            --quiet \
            --timeout=3 \
            --tries=2 \
            --header="Host: ${SAML_HOST}" \
            --output-document="${SAML_METADATA_PATH}" \
            "${DOWNLOAD_URL}" \
          && result=0
      } || true

      counter=$((counter-1))
      sleep 3
    done

    if [[ ${result} -gt 0 ]]; then
      echo "Error: Failed to fetch SAML metadata from ${DOWNLOAD_URL}" >&2
      exit 255
    fi

    echo "Successfully set SAML metadata in ${SAML_METADATA_PATH}"
  fi
}

setup_oidc() {
  OIDC_JWKS_URL=$(echo -n -e '@%@ldap/server/sasl/oauthbearer/issuer@%@' | ucr-light-filter || true)
  OIDC_JWKS_URL_INTERNAL=$(echo -n -e '@%@ldap/server/sasl/oauthbearer/issuer-internal@%@' | ucr-light-filter || true)

  if [[ -n "${OIDC_JWKS_URL:-}" ]]; then
    DOWNLOAD_URL="${OIDC_JWKS_URL_INTERNAL:-${OIDC_JWKS_URL}}/protocol/openid-connect/certs"
    OIDC_HOST=$(echo "${OIDC_JWKS_URL}" | awk -F/ '{print $3}')

    OIDC_JWKS_BASE="/usr/share/oidc/"
    OIDC_JWKS_PATH="${OIDC_JWKS_BASE}/trusted.jwks"

    echo "Trying to fetch OIDC JWKS from ${DOWNLOAD_URL}"
    result=1
    counter=3
    # 'Connection refused' is not retried by `wget --tries=X` hence the loop
    while [[ ${result} -gt 0 && ${counter} -gt 0 ]]; do
      {
          wget \
            --quiet \
            --timeout=3 \
            --tries=2 \
            --header="Host: ${OIDC_HOST}" \
            --output-document="${OIDC_JWKS_PATH}" \
            "${DOWNLOAD_URL}" \
          && result=0
      } || true

      counter=$((counter-1))
      sleep 3
    done

    if [[ ${result} -gt 0 ]]; then
      echo "Error: Failed to fetch OIDC JWKS from ${DOWNLOAD_URL}" >&2
      exit 255
    fi

    echo "Successfully set OIDC JWKS in ${OIDC_JWKS_PATH}"
  fi
}

setup_sasl_config() {
  SERVICE_PROVIDERS=$(echo -n -e '@%@ldap/saml/service-providers@%@' | ucr-light-filter || true)

  if [[ -n "${SERVICE_PROVIDERS:-}" ]]; then  # FIXME: if it's -z, we need to configure OIDC nevertheless
    # We have to modify the template since the hardcoded univention/saml/metadata
    # URL endpoint is not necessarily valid for future service providers.
    # Therefore we expect a comma-separated list of URLs in SERVICE_PROVIDERS.
    # And since our Identitiy Providers are not UMC anymore but Keycloak or Gluu,
    # we put the metadata XMLs into a vendor neutral location.
    # The sp library is not quite usable nor desired in this context so that
    # and its dependency sys are removed.

    printf -v filter_string '%s' \
     's/import sys/import os/;' \
     '/sys.path.insert/,+1d;' \
     's#univention-management-console/##;' \
     '/service_providers =/,+3d;' \
     '/if identity_provider/i ' \
     'service_providers = configRegistry.get("ldap/saml/service-providers", "").split(",")'

    sed -e "${filter_string}" \
      /etc/univention/templates/files/etc/ldap/sasl2/slapd.conf \
      | ucr-light-filter > /etc/ldap/sasl2/slapd.conf
  fi
}

setup_initial_ldif() {
  # Inspired by 01univention-ldap-server-init.inst

  files="$(find /var/lib/univention-ldap/ldap/ -name "data.*" -type f)"

  if [[ "${LDAP_SERVER_ENABLE_STATUS_CONFIGMAP:-false}" = "true" ]]; then
    evaluate_database_init database-needs-initialization && true
    db_exit_code=$?
  else
    echo "INFO: Feature \"Status ConfigMap\" not enabled."
    db_exit_code=0
  fi

  if [[ ${db_exit_code} -eq 2 ]]; then
    echo "ERROR: Failed to check the database initialization status."
    exit 1
  fi

  if [[ -n "${files}" || ${db_exit_code} -eq 1 ]]; then
    echo "INFO: Skipping loading of initial content, found existing database files."
    return 0
  else
    echo "INFO: Loading initial directory content."
  fi

  pw_crypt="$(slappasswd -h "{CRYPT}" -s "${LDAP_CN_ADMIN_PW}")"
  pw_crypt="${pw_crypt#'{CRYPT}'}"
  ldap_base="$(echo -n -e '@%@ldap/base@%@' | ucr-light-filter)"
  domainname="$(echo -n -e '@%@domainname@%@' | ucr-light-filter)"
  sambadomain="${domainname%%.*}"
  realm="$(echo "$domainname" | sed -e 's/dc=//g;s/,/./g;s/[a-z]/\u&/g')"
  firstdc="$(echo "$ldap_base" | sed -e 's|,.*||g;s|.*=||')"

  # TODO: check /usr/sbin/univention-newsid
  sid="S-1-5-21-UNSET"

  printf -v filter_string '%s' \
    "s|@@%%@@ldap\\.pw@@%%@@|$pw_crypt|;"\
    "s|@@%%@@sambadomain@@%%@@|$sambadomain|;"\
    "s|@@%@@sambadomain@@%@@|$sambadomain|;"\
    "s|@@%%@@firstdc@@%%@@|$firstdc|;"\
    "s|@@%%@@realm@@%%@@|$realm|;"\
    "s|@@%%@@sid@@%%@@|$sid|;"\
    "s|@@%@@domain@@%@@|$domainname|"

  # Remove cn=backup user as we don't need it
  sed -i '/cn=backup/,+6d' /usr/share/univention-ldap/base.ldif

  cat /usr/share/univention-ldap/{base.ldif,core-edition.ldif} \
    | ucr-light-filter | sed -e "${filter_string}" \
    | slapadd -f /etc/ldap/slapd.conf

  if [[ "${LDAP_SERVER_ENABLE_STATUS_CONFIGMAP:-false}" = "true" ]]; then
    evaluate_database_init database-initialized
  fi

}

setup_translog_ldif() {
  # Inspired by /usr/share/univention-ldap/setup-translog
  translog_exists="$(slapcat -f /etc/ldap/slapd.conf \
                             -b cn=translog \
                             -H 'ldap:///cn=translog??base' || true)"

  if [[ -n "${translog_exists}" ]]; then
    return 0
  fi

  slapadd -f /etc/ldap/slapd.conf \
          -b cn=translog \
          -l /usr/share/univention-ldap/translog.ldif
}

setup_tls() {
  # TODO: Fix this in Config Adapter
  # Check univention-ssl/debian/univention-ssl.postinst
  # and make-certificates.sh
  HOSTNAME=$(echo -n -e '@%@hostname@%@' | ucr-light-filter)
  DOMAIN_NAME=$(echo -n -e '@%@domainname@%@' | ucr-light-filter)
  target_dir="/etc/univention/ssl/${HOSTNAME}.${DOMAIN_NAME}"
  link_dir="/etc/univention/ssl/${HOSTNAME}"
  mkdir --parents "${target_dir}" /etc/univention/ssl/ucsCA/
  ln --symbolic --no-dereference --force "${target_dir}" "${link_dir}"

  TLS_MODE=$(echo -n -e '@%@directory/manager/starttls@%@' | ucr-light-filter)
  case "${TLS_MODE:-}" in
    "2" | "1")
      echo "Linking TLS certificates"
      if [ ! -f "${CA_CERT_FILE:-}" ] || [ ! -f "${CERT_PEM_FILE:-}" ] || [ ! -f "${PRIVATE_KEY_FILE:-}" ] \
        || [ ! -f "${DH_PARAM_FILE:-}" ]; then
        echo "All of \$CA_CERT_FILE, \$CERT_PEM_FILE, \$PRIVATE_KEY_FILE, \$DH_PARAM_FILE must be present!"
        exit 1
      fi
      DH_PARAM_TARGET=$(echo -n -e '@%@ldap/tls/dh/paramfile@%@' | ucr-light-filter)
      ln --symbolic --force "${CA_CERT_FILE}" "/etc/univention/ssl/ucsCA/CAcert.pem"
      ln --symbolic --force "${CERT_PEM_FILE}" "${target_dir}/cert.pem"
      ln --symbolic --force "${PRIVATE_KEY_FILE}" "${target_dir}/private.key"
      ln --symbolic --force "${DH_PARAM_FILE}" "${DH_PARAM_TARGET}"
      export LDAP_LISTEN="ldapi:/// ldap://:389/ ldaps://:636/"
      ;;
    "0")
      echo "No TLS certificates configured!"
      sed --in-place --expression '/^TLS/d' /etc/ldap/slapd.conf
      echo 'sasl-secprops none,minssf=0' >> /etc/ldap/slapd.conf
      export LDAP_LISTEN="ldapi:/// ldap://:389/"
      ;;
    *)
      echo "TLS_MODE must be set to '2', '1' or '0'."
      exit 1
      ;;
  esac
}

log_configuration() {
  echo "=== /etc/ldap/slapd.conf START ==="
  cat /etc/ldap/slapd.conf
  echo "=== /etc/ldap/slapd.conf  END  ==="

  echo "=== Files and directories START ==="
  find /etc/ldap /etc/univention/templates/files/etc/ldap /usr/lib/univention-ldap /usr/share/univention-ldap /var/lib/univention-ldap-local /var/lib/univention-ldap/ -ls || true
  echo "=== Files and directories  END  ==="
}

prepare_slapd_run() {
  # Adding `-d LOG_LEVEL` here overrides earlier settings in /etc/ldap/slapd.conf,
  # but without `-d` slapd would detach and the container would exit.
  export LOG_LEVEL=${LOG_LEVEL:-}

  if [ -z "${LOG_LEVEL}" ]; then
    echo "Setting OpenLDAP log level from UCS."
    LOG_LEVEL=$(echo -n -e '@%@ldap/debug/level@%@' | ucr-light-filter || true)
  fi

  {
    echo '#!/usr/bin/env bash'
    echo "ulimit -n \"$(ucr-light-filter <<< '@%@ldap/maxopenfiles@%@' || echo 65535)\""
    echo "/usr/sbin/slapd \\"
    echo "   -f /etc/ldap/slapd.conf \\"
    echo "   -d \"${LOG_LEVEL:-stats}\" \\"
    echo "   -h \"${LDAP_LISTEN}\" \\"
    echo '   "$@"'
  } > /etc/univention/run-slapd.sh

  chmod +x /etc/univention/run-slapd.sh
}

setup_symlinks
setup_paths
setup_listener_path
setup_last_id_path
setup_slapd_conf
fetch_saml_metadata
setup_sasl_config
setup_oidc
if [ "${UPDATE_INDEX_ON_STARTUP}" = "true" ]; then
  /usr/bin/sync-ldap-indexes
fi
if [ "${LDAP_SERVER_ROLE:-primary}" = "primary" ]; then
  setup_initial_ldif
fi
setup_translog_ldif
setup_tls
# log_configuration
prepare_slapd_run
