package controller

import (
	"reflect"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

func TestPlanUserOverlays(t *testing.T) {
	const (
		dataDN       = "olcDatabase={1}mdb,cn=config"
		refintName   = "refint"
		uniqueMember = "uniqueMember"
	)
	refint := ldapv1alpha1.DatabaseOverlay{
		Name:        refintName,
		ObjectClass: "olcRefintConfig",
		Attributes:  map[string][]string{"olcRefintAttribute": {uniqueMember}},
	}
	shadowbind := ldapv1alpha1.DatabaseOverlay{Name: "shadowbind"}

	tests := []struct {
		name     string
		current  []observedOverlay
		desired  []ldapv1alpha1.DatabaseOverlay
		wantAdds []string
		wantMods []overlayAttrChange
	}{
		{
			name:     "absent overlays are added in declared order",
			current:  []observedOverlay{{dn: "olcOverlay={0}syncprov," + dataDN, name: overlaySyncprov}},
			desired:  []ldapv1alpha1.DatabaseOverlay{shadowbind, refint},
			wantAdds: []string{"shadowbind", refintName},
		},
		{
			name: "present and matching overlay is left alone",
			current: []observedOverlay{{
				dn: "olcOverlay={1}refint," + dataDN, name: refintName,
				attrs: map[string][]string{"olcrefintattribute": {uniqueMember}},
			}},
			desired: []ldapv1alpha1.DatabaseOverlay{refint},
		},
		{
			name: "a differing attribute is replaced on the overlay's real DN",
			current: []observedOverlay{{
				dn: "olcOverlay={1}refint," + dataDN, name: refintName,
				attrs: map[string][]string{"olcrefintattribute": {"member"}},
			}},
			desired: []ldapv1alpha1.DatabaseOverlay{refint},
			wantMods: []overlayAttrChange{{
				dn: "olcOverlay={1}refint," + dataDN, attr: "olcRefintAttribute", values: []string{uniqueMember},
			}},
		},
		{
			name: "ordering prefixes slapd adds to X-ORDERED values do not count as drift",
			current: []observedOverlay{{
				dn: "olcOverlay={2}unique," + dataDN, name: "unique",
				attrs: map[string][]string{"olcuniqueuri": {"{0}ldap:///dc=x?uid?sub"}},
			}},
			desired: []ldapv1alpha1.DatabaseOverlay{{
				Name: "unique", ObjectClass: "olcUniqueConfig",
				Attributes: map[string][]string{"olcUniqueURI": {"ldap:///dc=x?uid?sub"}},
			}},
		},
		{
			name: "an overlay no longer declared is not removed",
			current: []observedOverlay{
				{dn: "olcOverlay={1}refint," + dataDN, name: refintName},
			},
			desired: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adds, mods := planUserOverlays(tc.current, tc.desired)
			var addNames []string
			for _, a := range adds {
				addNames = append(addNames, a.Name)
			}
			if !reflect.DeepEqual(addNames, tc.wantAdds) {
				t.Errorf("adds = %v, want %v", addNames, tc.wantAdds)
			}
			if !reflect.DeepEqual(mods, tc.wantMods) {
				t.Errorf("mods = %+v, want %+v", mods, tc.wantMods)
			}
		})
	}
}
