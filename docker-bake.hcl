# The build graph. One description, driven by `scripts/bake.sh`, consumed
# identically by a laptop and by every pipeline.
#
# It used to live in the Makefile, which was reasonable while make was the
# only thing that built images and unreasonable once the pipelines stopped
# wanting make. make's defining contribution -- rebuilding only what changed --
# never applied in CI anyway: a fresh checkout has no .stamps/, so every stamp
# was stale and everything rebuilt. The real incrementality was always
# BuildKit's content-addressed cache, one layer below. So the stamps are gone
# and make keeps the job it is actually good at: being a human frontend.
#
# WHY A CARRIER IS NOT AN IMAGE ANY MORE
#
# OpenLDAP is compiled once and feeds both images of a pair. That used to need
# openldap-deb to be a real, named, pushed image, because two separate `buildx
# build` invocations can hand each other nothing except through a registry.
#
# `contexts = { debs = "target:openldap-deb" }` removes the registry from that
# handoff: bake stitches both targets into ONE BuildKit graph and passes the
# carrier's layers in-process. It is never serialized, named, pushed or
# pulled. So "compile once, feed both" is a property of the graph -- not of a
# cache hit, and not of a registry round-trip.
#
# A named context SHADOWS a build stage of the same name, which is why the
# Containerfiles keep working unchanged: each declares
#   ARG OPENLDAP_DEB_IMAGE=...
#   FROM ${OPENLDAP_DEB_IMAGE} AS debs
# and the `debs` context supplied here wins over that stage. Supply no
# context and the stage does its old job, so a bare
# `buildx build --build-arg OPENLDAP_DEB_IMAGE=<ref>` still builds every one
# of them. Adopting bake did not make these files bake-only.

variable "REGISTRY"            { default = "ghcr.io/chuck-chuck-chuck-net" }
variable "PROJECT"             { default = "slaptain" }
variable "IMAGE_TAG"           { default = "dev" }
variable "EXPORTER_IMAGE_TAG"  { default = "dev" }

# Derived by scripts/bake.sh, because HCL cannot read a file: the OpenLDAP
# version comes from the packaging fork's changelog and the -ol26src patch
# list from the directory, so neither can disagree with what was applied.
variable "OPENLDAP_27_VERSION" { default = "" }
variable "OL26SRC_PATCHES"     { default = "" }

variable "OPENLDAP_OL26_VERSION" { default = "2.6.10+dfsg-1" }
variable "RUN_UPSTREAM_TESTS"    { default = "0" }

# EXPERIMENTAL 2.6 forward-port (internal use only, never in `images`).
variable "OL2616_VERSION"     { default = "2.6.16" }
variable "OL2616_SHA256"      { default = "8d8452bc3f56aa7a392c62dd377733f83d1314eed6c52fe44028439a3bb4df18" }
variable "OL2616_DEBIAN_PKG"  { default = "2.6.14+dfsg-2" }
variable "OL2616_PKG_VERSION" { default = "2.6.16-0+slaptain1" }
variable "OL2616_PATCHES"     { default = "accesslog-purge-phase.patch accesslog-purge-logging.patch" }

# Where finished images go. scripts/bake.sh sets it; the default publishes
# nothing, so a bare `buildx bake` can never push by accident.
variable "OUTPUT" { default = "type=cacheonly" }

# Registry-backed layer cache, so a builder with a cold or wiped cache does
# not pay for the OpenLDAP compiles again. Empty = off, which is right for a
# laptop (its builder keeps its own cache) and is why nothing is cached by
# accident.
#
# Applied to the CARRIERS only, and one ref per carrier. Only applied there
# because they are the expensive targets -- minutes of compiling against
# seconds of Go -- and one ref EACH because a shared ref would have each
# target overwrite the previous one's manifest, leaving a cache that always
# misses and looks like it is working.
variable "CACHE_REF" { default = "" }

function "cache_from" {
  params = [name]
  result = CACHE_REF == "" ? [] : ["type=registry,ref=${CACHE_REF}:${name}"]
}
function "cache_to" {
  params = [name]
  result = CACHE_REF == "" ? [] : ["type=registry,ref=${CACHE_REF}:${name},mode=max"]
}

function "img" {
  params = [name, tag]
  result = "${REGISTRY}/${PROJECT}/${name}:${tag}"
}

# ── Groups ──────────────────────────────────────────────────────────────────

# Everything published by a release. Carriers are absent on purpose: they are
# reachable only as contexts, so no output setting can make one escape.
group "images" {
  targets = [
    "slapd-init", "slapd",
    "slapd-init-slaptain", "slapd-slaptain",
    "slapd-init-ol26", "slapd-ol26",
    "slapd-init-ol26src", "slapd-ol26src",
    "toolkit", "operator", "exporter",
  ]
}

group "default" { targets = ["images"] }

# The legacy 2.6 pairs on their own, for an -ol26 e2e lane (ADR-021).
group "ol26"    { targets = ["slapd-init-ol26", "slapd-ol26"] }
group "ol26src" { targets = ["slapd-init-ol26src", "slapd-ol26src"] }

# EXPERIMENTAL forward-port; not in `images`, not a select.flavour cell.
group "ol2616"  { targets = ["slapd-init-ol2616", "slapd-ol2616"] }

# Diagnostic variants (docs/DEBUG-IMAGES.md). Never in `images`, never in a chart.
group "debug"   { targets = ["slapd-debug"] }
group "asan"    { targets = ["slapd-asan"] }

# ── Carriers ────────────────────────────────────────────────────────────────
# Reached only through `contexts`. cacheonly is not a default to be overridden
# but a statement: these are build inputs and have no registry identity.

target "_carrier" {
  context    = "images/openldap-deb"
  dockerfile = "Containerfile"
  output     = ["type=cacheonly"]
  args       = { RUN_UPSTREAM_TESTS = RUN_UPSTREAM_TESTS }
}

target "openldap-deb" {
  inherits = ["_carrier"]
  args     = { SLAPTAIN_PATCHES = "0" }
  cache-from = cache_from("openldap-deb")
  cache-to   = cache_to("openldap-deb")
}

target "openldap-deb-slaptain" {
  inherits = ["_carrier"]
  args     = { SLAPTAIN_PATCHES = "1" }
  cache-from = cache_from("openldap-deb-slaptain")
  cache-to   = cache_to("openldap-deb-slaptain")
}

target "openldap-deb-ol26src" {
  inherits   = ["_carrier"]
  dockerfile = "Containerfile.ol26src"
  args       = { OPENLDAP_OL26_VERSION = OPENLDAP_OL26_VERSION }
  cache-from = cache_from("openldap-deb-ol26src")
  cache-to   = cache_to("openldap-deb-ol26src")
}

target "openldap-deb-ol2616" {
  inherits   = ["_carrier"]
  dockerfile = "Containerfile.ol2616"
  args = {
    OPENLDAP_VERSION     = OL2616_VERSION
    OPENLDAP_SHA256      = OL2616_SHA256
    DEBIAN_PKG_VERSION   = OL2616_DEBIAN_PKG
    OPENLDAP_PKG_VERSION = OL2616_PKG_VERSION
    SLAPTAIN_PATCH_LIST  = OL2616_PATCHES
  }
  cache-from = cache_from("openldap-deb-ol2616")
  cache-to   = cache_to("openldap-deb-ol2616")
}

target "openldap-deb-debug" {
  inherits = ["_carrier"]
  args     = { BUILD_VARIANT = "debug" }
  cache-from = cache_from("openldap-deb-debug")
  cache-to   = cache_to("openldap-deb-debug")
}

target "openldap-deb-asan" {
  inherits = ["_carrier"]
  args     = { BUILD_VARIANT = "asan" }
  cache-from = cache_from("openldap-deb-asan")
  cache-to   = cache_to("openldap-deb-asan")
}

# ── The 2.7 pairs ───────────────────────────────────────────────────────────
# PURE is the unsuffixed default and PATCHED takes the token (ADR-040
# decision 2): turning the rotation on has to be two acts, enabling it AND
# asking for a build that can express the phase.

target "_init" {
  context    = "images/slapd-init"
  dockerfile = "Containerfile"
  output     = [OUTPUT]
}

target "_slapd" {
  context    = "images/slapd"
  dockerfile = "Containerfile"
  output     = [OUTPUT]
}

target "slapd-init" {
  inherits = ["_init"]
  contexts = { debs = "target:openldap-deb" }
  tags     = [img("slapd-init", IMAGE_TAG)]
}

target "slapd" {
  inherits = ["_slapd"]
  contexts = { debs = "target:openldap-deb" }
  tags     = [img("slapd", IMAGE_TAG)]
}

target "slapd-init-slaptain" {
  inherits = ["_init"]
  contexts = { debs = "target:openldap-deb-slaptain" }
  tags     = [img("slapd-init", "${IMAGE_TAG}-ol${OPENLDAP_27_VERSION}-slaptain")]
}

target "slapd-slaptain" {
  inherits = ["_slapd"]
  contexts = { debs = "target:openldap-deb-slaptain" }
  tags     = [img("slapd", "${IMAGE_TAG}-ol${OPENLDAP_27_VERSION}-slaptain")]
}

# ── The 2.6 pairs ───────────────────────────────────────────────────────────
# -ol26 installs Debian's prebuilt binary and so needs no carrier and can
# carry no patch; -ol26src builds the same 2.6 from source and does. Pin both
# images of a pair or neither -- the tools must match the server.

target "slapd-init-ol26" {
  inherits   = ["_init"]
  dockerfile = "Containerfile.ol26"
  args       = { OPENLDAP_OL26_VERSION = OPENLDAP_OL26_VERSION }
  tags       = [img("slapd-init", "${IMAGE_TAG}-ol26")]
}

target "slapd-ol26" {
  inherits   = ["_slapd"]
  dockerfile = "Containerfile.ol26"
  args       = { OPENLDAP_OL26_VERSION = OPENLDAP_OL26_VERSION }
  tags       = [img("slapd", "${IMAGE_TAG}-ol26")]
}

# The label is derived from the directory, so it cannot drift from what was
# applied: -ol26 and -ol26src differ by three characters and only one of them
# carries patches.
target "_ol26src_labels" {
  labels = {
    "net.chuck-chuck-chuck.slaptain.local-patches"   = OL26SRC_PATCHES
    "org.opencontainers.image.description"           = "OpenLDAP 2.6 from Debian source, with slaptain local patches"
  }
}

target "slapd-init-ol26src" {
  inherits = ["_init", "_ol26src_labels"]
  contexts = { debs = "target:openldap-deb-ol26src" }
  tags     = [img("slapd-init", "${IMAGE_TAG}-ol26src")]
}

target "slapd-ol26src" {
  inherits = ["_slapd", "_ol26src_labels"]
  contexts = { debs = "target:openldap-deb-ol26src" }
  tags     = [img("slapd", "${IMAGE_TAG}-ol26src")]
}

# ── EXPERIMENTAL 2.6.16 forward-port ────────────────────────────────────────
# Upstream 2.6.16, vendored Debian packaging, both local patches. Exists as a
# MEASUREMENT arm: ITS#10465's purge pacing never landed on 2.6, so this purge
# loop is mechanically the one every ADR-037 figure was measured against.

target "_ol2616_labels" {
  labels = {
    "net.chuck-chuck-chuck.slaptain.local-patches" = OL2616_PATCHES
    "net.chuck-chuck-chuck.slaptain.experimental"  = "forward-port, internal use only, not a select.flavour cell"
    "org.opencontainers.image.description"         = "EXPERIMENTAL: upstream OpenLDAP ${OL2616_VERSION} with Debian packaging and slaptain local patches"
  }
}

target "slapd-init-ol2616" {
  inherits = ["_init", "_ol2616_labels"]
  contexts = { debs = "target:openldap-deb-ol2616" }
  tags     = [img("slapd-init", "${IMAGE_TAG}-ol2.6.16-slaptain")]
}

target "slapd-ol2616" {
  inherits = ["_slapd", "_ol2616_labels"]
  contexts = { debs = "target:openldap-deb-ol2616" }
  tags     = [img("slapd", "${IMAGE_TAG}-ol2.6.16-slaptain")]
}

# ── Diagnostic variants ─────────────────────────────────────────────────────
# BUILD_VARIANT must match the carrier's: the compiler flags live in the
# .debs, this picks the debug symbols and environment that go with them.
# Deploy by pinning spec.images.slapd.tag ALONE -- unlike -ol26, slapd-init
# stays on the plain tag, because the OpenLDAP version and therefore the
# on-disk format are identical and only slapd's own flags differ.

target "slapd-debug" {
  inherits = ["_slapd"]
  contexts = { debs = "target:openldap-deb-debug" }
  args     = { BUILD_VARIANT = "debug" }
  tags     = [img("slapd", "${IMAGE_TAG}-debug")]
}

target "slapd-asan" {
  inherits = ["_slapd"]
  contexts = { debs = "target:openldap-deb-asan" }
  args     = { BUILD_VARIANT = "asan" }
  tags     = [img("slapd", "${IMAGE_TAG}-asan")]
}

# ── Everything else ─────────────────────────────────────────────────────────
# operator and exporter build from the repository ROOT (each COPYs its own Go
# module), which is what .dockerignore is for.

target "toolkit" {
  context    = "images/slapd-toolkit"
  dockerfile = "Containerfile"
  output     = [OUTPUT]
  tags       = [img("slapd-toolkit", IMAGE_TAG)]
}

target "operator" {
  context    = "."
  dockerfile = "images/operator/Containerfile"
  output     = [OUTPUT]
  tags       = [img("operator", IMAGE_TAG)]
}

# Its own version line (ADR-033 decision 4): git tags exporter/vX.Y.Z -> image
# X.Y.Z, so an operator upgrade rolls nothing through the sidecar.
target "exporter" {
  context    = "."
  dockerfile = "images/slapd-exporter/Containerfile"
  output     = [OUTPUT]
  tags       = [img("slapd-exporter", EXPORTER_IMAGE_TAG)]
}
