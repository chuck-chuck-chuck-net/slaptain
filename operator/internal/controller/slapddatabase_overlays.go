package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/go-ldap/ldap/v3"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// observedOverlay is one olcOverlay entry under a data database, as read from a
// pod. Attribute names are lower-cased; values keep slapd's {N} prefixes.
type observedOverlay struct {
	dn    string
	name  string
	attrs map[string][]string
}

// overlayAttrChange replaces one attribute of an existing overlay entry.
type overlayAttrChange struct {
	dn     string
	attr   string
	values []string
}

// planUserOverlays decides what spec.overlays needs on one pod: the declared
// overlays that are absent, in declared order, and a replace for every declared
// attribute whose values differ. Overlays and attributes nobody declared are
// left alone (ADR-026 R2), so dropping an entry from the spec removes nothing.
func planUserOverlays(current []observedOverlay, desired []ldapv1alpha1.DatabaseOverlay) (
	[]ldapv1alpha1.DatabaseOverlay, []overlayAttrChange,
) {
	byName := make(map[string]observedOverlay, len(current))
	for _, o := range current {
		byName[o.name] = o
	}

	var adds []ldapv1alpha1.DatabaseOverlay
	var mods []overlayAttrChange
	for _, d := range desired {
		cur, ok := byName[d.Name]
		if !ok {
			adds = append(adds, d)
			continue
		}
		for _, attr := range slices.Sorted(maps.Keys(d.Attributes)) {
			want := d.Attributes[attr]
			if !sameOverlayValues(cur.attrs[strings.ToLower(attr)], want) {
				mods = append(mods, overlayAttrChange{dn: cur.dn, attr: attr, values: want})
			}
		}
	}
	return adds, mods
}

// sameOverlayValues compares in order, ignoring the {N} prefix slapd puts on
// X-ORDERED values such as olcUniqueURI.
func sameOverlayValues(current, desired []string) bool {
	if len(current) != len(desired) {
		return false
	}
	for i := range current {
		if stripOrderingPrefix(current[i]) != desired[i] {
			return false
		}
	}
	return true
}

// ensureUserOverlays converges spec.overlays on one pod's data database. It
// runs after the operator's own syncprov and accesslog, so on a new database
// the declared overlays come after those in the overlay stack.
func (r *SlapdDatabaseReconciler) ensureUserOverlays(
	ctx context.Context,
	conn *ldap.Conn,
	host, dataDN string,
	desired []ldapv1alpha1.DatabaseOverlay,
) error {
	if len(desired) == 0 {
		return nil
	}
	log := logf.FromContext(ctx)

	current, err := readOverlays(conn, dataDN)
	if err != nil {
		return err
	}
	adds, mods := planUserOverlays(current, desired)

	for _, o := range adds {
		log.Info("adding overlay to data database", "host", host, "dataDN", dataDN, "overlay", o.Name)
		addReq := ldap.NewAddRequest("olcOverlay="+o.Name+","+dataDN, nil)
		objectClasses := []string{ocOverlayConfig}
		if o.ObjectClass != "" && !strings.EqualFold(o.ObjectClass, ocOverlayConfig) {
			objectClasses = append(objectClasses, o.ObjectClass)
		}
		addReq.Attribute("objectClass", objectClasses)
		addReq.Attribute(attrOlcOverlay, []string{o.Name})
		for _, attr := range slices.Sorted(maps.Keys(o.Attributes)) {
			addReq.Attribute(attr, o.Attributes[attr])
		}
		if err := guardedAdd(conn, addReq); err != nil &&
			!ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			return fmt.Errorf("add overlay %s: %w", o.Name, err)
		}
	}
	for _, m := range mods {
		log.Info("updating overlay attribute", "host", host, "dn", m.dn, "attribute", m.attr)
		modReq := ldap.NewModifyRequest(m.dn, nil)
		modReq.Replace(m.attr, m.values)
		if err := guardedModify(conn, modReq); err != nil {
			return fmt.Errorf("modify %s on %s: %w", m.attr, m.dn, err)
		}
	}
	return nil
}

// readOverlays lists the overlay entries directly under dataDN with all their
// attributes.
func readOverlays(conn *ldap.Conn, dataDN string) ([]observedOverlay, error) {
	sr, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcOverlayConfig)", nil, nil,
	))
	if err != nil {
		return nil, fmt.Errorf("search overlays under %s: %w", dataDN, err)
	}
	out := make([]observedOverlay, 0, len(sr.Entries))
	for _, e := range sr.Entries {
		names := e.GetEqualFoldAttributeValues(attrOlcOverlay)
		if len(names) == 0 {
			continue
		}
		o := observedOverlay{dn: e.DN, name: stripOrderingPrefix(names[0]), attrs: map[string][]string{}}
		for _, a := range e.Attributes {
			o.attrs[strings.ToLower(a.Name)] = slices.Clone(a.Values)
		}
		out = append(out, o)
	}
	return out, nil
}
