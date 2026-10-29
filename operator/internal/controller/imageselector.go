package controller

import (
	"fmt"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The data-plane build matrix (ADR-040). Two axes — what can open your volumes,
// and whether you run our patches — resolving to a SUFFIX on the operator's own
// tag, so an unpinned image tracks the operator release the way it always has.
//
// The default cell carries NO suffix, deliberately. That is ADR-040 decision 2
// (the unsuffixed build is the pure one), and it is what keeps the charts'
// `image.tag | default .Chart.AppVersion` fallback an identity rather than a
// coincidence. It also means an unset selector renders a pod template that is
// byte-identical to the one the operator rendered before this field existed.
//
// The suffixes are the only place the tag vocabulary appears. That is the point
// of the selector: a user says what they need, not what an artifact is called,
// so renaming the artifacts is a change to this table and to nothing a user
// has written down.
const (
	storageFormatCurrent = "lmdb1.0"
	storageFormatLegacy  = "lmdb0.9"

	flavourPure     = "pure"
	flavourSlaptain = "slaptain"

	// The tag suffix naming each build. The unsuffixed cell is the default
	// (ADR-040 decision 2), so an unqualified tag means pure 2.7.
	tagSuffixSlaptain27 = "-ol2.7.2-slaptain"
	tagSuffixOL26       = "-ol26"
	tagSuffixOL26Src    = "-ol26src"
)

// imageTagSuffixes is the matrix, keyed [storageFormat][flavour].
var imageTagSuffixes = map[string]map[string]string{
	storageFormatCurrent: {
		flavourPure:     "",
		flavourSlaptain: tagSuffixSlaptain27,
	},
	storageFormatLegacy: {
		// Debian's own 2.6 binary, from its own source package.
		flavourPure: tagSuffixOL26,
		// Debian's 2.6 source plus images/openldap/local-patches/.
		flavourSlaptain: tagSuffixOL26Src,
	},
}

// imageTagSuffixForSelector resolves a selector to the tag suffix for the
// matching data-plane build. A nil or empty selector takes the defaults, which
// are the unsuffixed cell.
//
// Unknown values are REFUSED rather than guessed. The CRD enum rejects them at
// admission, so reaching this error means a cluster was stored under an older
// schema or the enum and this table have drifted — both cases where resolving
// to "some image" would be worse than saying so. ADR-024's rule: a spec field
// that cannot be honoured is rejected or converged, never ignored.
func imageTagSuffixForSelector(sel *ldapv1alpha1.SlapdImageSelector) (string, error) {
	format, flavour := storageFormatCurrent, flavourPure
	if sel != nil {
		if sel.StorageFormat != "" {
			format = sel.StorageFormat
		}
		if sel.Flavour != "" {
			flavour = sel.Flavour
		}
	}
	byFlavour, ok := imageTagSuffixes[format]
	if !ok {
		return "", fmt.Errorf("unknown spec.images.select.storageFormat %q (known: %s, %s)",
			format, storageFormatCurrent, storageFormatLegacy)
	}
	suffix, ok := byFlavour[flavour]
	if !ok {
		return "", fmt.Errorf("unknown spec.images.select.flavour %q (known: %s, %s)",
			flavour, flavourPure, flavourSlaptain)
	}
	return suffix, nil
}

// resolveImageRefSelected builds "repository:tag" for a data-plane image, with
// the selector's suffix applied to the DEFAULTED tag only.
//
// An explicit tag is used verbatim and never has a suffix appended: pinning an
// artifact means that artifact, and silently rewriting someone's pin would make
// the pin a suggestion. A user who pins and also selects has said two things,
// and the more specific one wins — the same precedence the repository field
// already has.
func resolveImageRefSelected(img ldapv1alpha1.SlapdImageConfig, defaultRepo, defaultTag, suffix string) string {
	if img.Tag != "" {
		return resolveImageRef(img, defaultRepo, defaultTag)
	}
	tag := defaultTag
	if tag == "" {
		tag = "latest"
	}
	return resolveImageRef(ldapv1alpha1.SlapdImageConfig{
		Repository: img.Repository,
		Tag:        tag + suffix,
	}, defaultRepo, defaultTag)
}

// Data-plane image components, as they appear in the published repository path.
const (
	componentSlapd     = "slapd"
	componentSlapdInit = "slapd-init"
)

// dataPlaneImage resolves the full "repository:tag" for one of the cluster's
// two data-plane images, applying the cluster's selector.
//
// Every call site that needs a slapd or slapd-init image goes through here —
// the StatefulSet's two containers, the mkdir Jobs, the backup Job and the
// restore Job. That is deliberate and is the mechanism behind ADR-021's "pin
// both or neither": there is no way to resolve one of the pair without the
// selector that governs both, so the two cannot drift by someone adding a
// sixth call site that forgets.
func dataPlaneImage(sc *ldapv1alpha1.SlapdCluster, component, operatorImage, defaultTag string) (string, error) {
	suffix, err := imageTagSuffixForSelector(sc.Spec.Images.Select)
	if err != nil {
		return "", err
	}
	var img ldapv1alpha1.SlapdImageConfig
	switch component {
	case componentSlapd:
		img = sc.Spec.Images.Slapd
	case componentSlapdInit:
		img = sc.Spec.Images.Init
	default:
		return "", fmt.Errorf("not a data-plane image component: %q", component)
	}
	return resolveImageRefSelected(img, defaultDataPlaneRepo(operatorImage, component), defaultTag, suffix), nil
}
