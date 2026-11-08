# Every image CI publishes, as one BuildKit graph:
#
#   env $(scripts/bake-vars.sh) docker buildx bake --push
#
# One bake run builds each OpenLDAP package set once and every independent
# target in parallel. Names and tags follow docs/VERSIONING.md.

variable "REGISTRY" {
  default = "ghcr.io/chuck-chuck-chuck-net"
}
variable "PROJECT" {
  default = "slaptain"
}
variable "TAG" {}
variable "EXPORTER_TAG" {}
variable "OPENLDAP_27_VERSION" {}
variable "OL26SRC_PATCHES" {}
# Run OpenLDAP's own test suite during the package build (slow; off by default).
variable "RUN_UPSTREAM_TESTS" {
  default = "0"
}
# The Debian slapd version both 2.6 lines are pinned to (ADR-040 decision 1),
# one variable so the slapd / slapd-init pair cannot drift apart.
variable "OPENLDAP_OL26_VERSION" {
  default = "2.6.10+dfsg-1"
}

function "image" {
  params = [name, tag]
  result = "${REGISTRY}/${PROJECT}/${name}:${tag}"
}

group "default" {
  targets = ["openldap", "ol26", "toolkit", "operator", "exporter"]
}

# The default group split for CI, one runner each: every OpenLDAP package build
# runs make -j$(nproc), so three of them on one runner share its cores.
group "publish-pure" {
  targets = ["slapd-pure", "slapd-init-pure"]
}
group "publish-slaptain" {
  targets = ["slapd-slaptain", "slapd-init-slaptain"]
}
group "publish-ol26src" {
  targets = ["slapd-ol26src", "slapd-init-ol26src"]
}
group "publish-other" {
  targets = ["ol26", "toolkit", "operator", "exporter"]
}

target "_common" {
  platforms = ["linux/amd64"]
  attest    = ["type=provenance,disabled=true"]
}

# slapd and slapd-init from images/openldap/Containerfile. The two images of a
# flavour get identical arguments, so they share one package build.
target "openldap" {
  name       = "${img}-${flavour}"
  inherits   = ["_common"]
  context    = "images/openldap"
  dockerfile = "Containerfile"
  target     = img
  matrix = {
    img     = ["slapd", "slapd-init"]
    flavour = ["pure", "slaptain", "ol26src"]
  }
  args = {
    OPENLDAP_SOURCE       = flavour == "ol26src" ? "debian-src" : "fork"
    SLAPTAIN_PATCHES      = flavour == "slaptain" ? "1" : "0"
    OPENLDAP_OL26_VERSION = OPENLDAP_OL26_VERSION
    RUN_UPSTREAM_TESTS    = RUN_UPSTREAM_TESTS
  }
  labels = flavour == "ol26src" ? {
    "net.chuck-chuck-chuck.slaptain.local-patches" = OL26SRC_PATCHES
    "org.opencontainers.image.description"         = "OpenLDAP 2.6 from Debian source, with slaptain local patches"
  } : {}
  tags = [image(img, "${TAG}${{
    pure     = ""
    slaptain = "-ol${OPENLDAP_27_VERSION}-slaptain"
    ol26src  = "-ol26src"
  }[flavour]}")]
}

# The -ol26 pair installs Debian's binary packages from the archive.
target "ol26" {
  name       = "${img}-ol26"
  inherits   = ["_common"]
  context    = "images/openldap"
  dockerfile = "Containerfile.ol26-${img == "slapd" ? "slapd" : "init"}"
  matrix = {
    img = ["slapd", "slapd-init"]
  }
  args = {
    OPENLDAP_OL26_VERSION = OPENLDAP_OL26_VERSION
  }
  tags = [image(img, "${TAG}-ol26")]
}

target "toolkit" {
  inherits   = ["_common"]
  context    = "images/slapd-toolkit"
  dockerfile = "Containerfile"
  tags       = [image("slapd-toolkit", TAG)]
}

target "operator" {
  inherits   = ["_common"]
  context    = "operator"
  dockerfile = "../images/operator/Containerfile"
  tags       = [image("operator", TAG)]
}

# Its own version line (ADR-033 decision 4): git tags exporter/vX.Y.Z.
target "exporter" {
  inherits   = ["_common"]
  context    = "exporter"
  dockerfile = "../images/slapd-exporter/Containerfile"
  tags       = [image("slapd-exporter", EXPORTER_TAG)]
}

# ── Local-only builds ───────────────────────────────────────────────────────
# Nothing below is in the default group, so CI never builds or publishes it.
# `make build-slapd-debug` and friends reach it.

# Diagnostic slapd variants (docs/DEBUG-IMAGES.md). BUILD_VARIANT reaches the
# package stage of the one Containerfile, so each is a full package build.
# slapd-init stays on the plain tag: the OpenLDAP version, and so the on-disk
# format, is the same. The patch default of the Containerfile applies.
target "slapd-debug" {
  inherits   = ["_common"]
  context    = "images/openldap"
  dockerfile = "Containerfile"
  target     = "slapd"
  args = {
    BUILD_VARIANT      = "debug"
    RUN_UPSTREAM_TESTS = RUN_UPSTREAM_TESTS
  }
  tags = [image("slapd", "${TAG}-debug")]
}

target "slapd-asan" {
  inherits   = ["_common"]
  context    = "images/openldap"
  dockerfile = "Containerfile"
  target     = "slapd"
  args = {
    BUILD_VARIANT      = "asan"
    RUN_UPSTREAM_TESTS = RUN_UPSTREAM_TESTS
  }
  tags = [image("slapd", "${TAG}-asan")]
}

# EXPERIMENTAL 2.6.16 forward-port (internal use only, not a select.flavour
# cell; docs/OPENLDAP-VERSIONS.md). Its packages come from Containerfile.ol2616,
# not from a stage of the main file, so they enter as the `debs` context, which
# shadows the stage of that name. The carrier has no tag and no output, so it
# can never reach a registry.
variable "OL2616_PATCHES" {
  default = "accesslog-purge-phase.patch accesslog-purge-logging.patch"
}

target "debs-ol2616" {
  inherits   = ["_common"]
  context    = "images/openldap"
  dockerfile = "Containerfile.ol2616"
  args       = { SLAPTAIN_PATCH_LIST = OL2616_PATCHES }
  output     = ["type=cacheonly"]
}

target "_ol2616" {
  inherits   = ["_common"]
  context    = "images/openldap"
  dockerfile = "Containerfile"
  contexts   = { debs = "target:debs-ol2616" }
  labels = {
    "net.chuck-chuck-chuck.slaptain.local-patches" = OL2616_PATCHES
    "net.chuck-chuck-chuck.slaptain.experimental"  = "forward-port, internal use only, not a select.flavour cell"
    "org.opencontainers.image.description"         = "EXPERIMENTAL: upstream OpenLDAP 2.6.16 with Debian packaging and slaptain local patches"
  }
}

target "slapd-ol2616" {
  inherits = ["_ol2616"]
  target   = "slapd"
  tags     = [image("slapd", "${TAG}-ol2.6.16-slaptain")]
}

target "slapd-init-ol2616" {
  inherits = ["_ol2616"]
  target   = "slapd-init"
  tags     = [image("slapd-init", "${TAG}-ol2.6.16-slaptain")]
}
