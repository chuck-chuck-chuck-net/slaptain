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
