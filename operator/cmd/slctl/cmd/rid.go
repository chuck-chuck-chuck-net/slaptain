package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// `slctl rid decode <rid>` — reading a syncrepl replica ID back out of a slapd
// log line (ADR-031, RID-PLAN M6).
//
// The whole value of ADR-031 decision 1 is that a RID stopped being an opaque
// address and became a coordinate: `rid = 1 + segment·M + offset`, with
// `segment` naming the database and `offset` naming either a pod at the
// reader's own site (`offset < P`) or a link to a peer site (`offset >= P`,
// index `offset − P`). Every term is declared or structural, so the number is
// self-describing. This command is where a human cashes that in.
//
// Three properties shape the surface, in this order:
//
//  1. **It works with no cluster.** The main user is someone reading a log on a
//     laptop. Decoding is arithmetic against a layout, and the default layout
//     is a compiled-in constant, so the offline path needs no I/O and is not a
//     degraded mode — it is the base case.
//  2. **It does better with a cluster.** The segment maps to the SlapdDatabase
//     that declares it and the site index to the site that declares it, both
//     read from CRs, and the layout comes from the mesh actually in force
//     rather than from the defaults.
//  3. **It never guesses.** Anything it could not resolve is said in words —
//     "no SlapdDatabase ... declares ridSegment 15" — and never left as
//     silence, which would read identically to "resolved to nothing".
//
// The one rule that separates this from the operator: a RID inside slapd's
// 0..999 band but outside the matrix the layout can produce is NOT an error
// here. ridcoord.Decode refuses it, which is right for an operator that must
// not issue such a number, and wrong for a diagnostic whose user took the
// number off a real stanza — very likely one written before the ADR-031
// renumber. The answer owed is "inside slapd's band, outside this mesh's
// matrix", with both ceilings named (ADR-031 Consequences, final slctl
// bullet; RID-PLAN M6).

// ridDecodeStatus is what the decode could make of the number. It is a status
// and not an error because a diagnostic always answers; the four non-OK values
// are four different sentences a human needs, not four failures.
type ridDecodeStatus string

const (
	// ridDecodeOK — the RID is a coordinate in this layout's matrix.
	ridDecodeOK ridDecodeStatus = "ok"
	// ridDecodeReserved — rid 0, which ADR-031 decision 1 reserves and never
	// allocates. A zero is a bug somewhere, never a provider.
	ridDecodeReserved ridDecodeStatus = "reserved"
	// ridDecodeOutsideMatrix — inside slapd's 0..999 band, above the largest
	// RID this layout produces (C·M). A legal syncrepl id that this mesh never
	// issued: a pre-ADR-031 stanza, another tool, or a hand edit.
	ridDecodeOutsideMatrix ridDecodeStatus = "outside-matrix"
	// ridDecodeOutsideBand — below 0 or above MaxRID. Not a syncrepl replica
	// ID at all; slapd itself would refuse it.
	ridDecodeOutsideBand ridDecodeStatus = "outside-band"
	// ridDecodeLayoutUnusable — the layout cannot produce any segment, so the
	// number cannot be decoded against anything. The fault is the layout, and
	// saying so beats reporting the RID as out of range.
	ridDecodeLayoutUnusable ridDecodeStatus = "layout-unusable"
)

// ridResolution is the naming context a decode resolves against: the layout in
// force, where it came from and whether that provenance can be trusted, plus
// whatever the cluster declares about segments, sites and pods.
//
// Every map is allowed to be nil. A nil map means "not consulted", which the
// decode reports differently from "consulted and found nothing" — the
// distinction property 3 above exists for.
type ridResolution struct {
	// Layout is the matrix shape the number is decoded against.
	Layout ridcoord.Layout
	// LayoutSource is human-readable provenance, printed verbatim: the default,
	// a named SlapdMesh, or an explicit --layout.
	LayoutSource string
	// LayoutTrusted is false when the layout shown is a fallback standing in
	// for one that could not be read — a cluster naming a SlapdMesh that is not
	// readable. The arithmetic still runs, because a best-effort decode beats
	// no decode, but the result says so.
	LayoutTrusted bool

	// ClusterName is the SlapdCluster consulted, empty when none was.
	ClusterName string
	// Replicas is that cluster's spec.replicas, used only to notice that a
	// decoded local ordinal names no pod it currently runs.
	Replicas int32
	// MeshName is the SlapdMesh consulted, empty when none was.
	MeshName string

	// Databases maps a declared ridSegment to the SlapdDatabase names declaring
	// it. A slice, not a string, because two databases on one segment is a real
	// state (ADR-031 decision 3 refuses it in the operator) and a decode that
	// could only show one would hide exactly the collision a human is hunting.
	// Nil means no cluster was consulted.
	Databases map[int32][]string
	// Sites maps a declared serverIDIndex to the site name declaring it
	// (ADR-028). Nil means no mesh was consulted.
	Sites map[int32]string
}

// ridDecodeResult is one decoded number, in full. Pointer fields are nil when
// the term does not apply: Segment for anything but a decoded coordinate,
// Ordinal for a remote one, SiteIndex for a local one.
type ridDecodeResult struct {
	RID    int32
	Status ridDecodeStatus
	// Layout is the layout the decode actually used, echoed so the output can
	// state it without the caller carrying the resolution alongside.
	Layout       ridcoord.Layout
	LayoutSource string

	Segment   *int32
	Kind      ridcoord.Kind // the zero Kind when Status is not ridDecodeOK
	Ordinal   *int32
	SiteIndex *int32

	// Databases names every SlapdDatabase declaring Segment — empty when none
	// does, or when no cluster was consulted. Notes distinguishes the two.
	Databases []string
	// Pod is the pod a local coordinate names (`<cluster>-<ordinal>`), empty
	// when no cluster was consulted.
	Pod string
	// Site is the site a remote coordinate names, empty when no mesh was
	// consulted or no site declares the index.
	Site string

	// Summary is the one sentence that answers the question the user asked.
	// Never empty, for any status.
	Summary string
	// Notes carries everything that could not be resolved, each as a complete
	// sentence naming what was looked for and where.
	Notes []string
}

// ptrI32 is the int32 counterpart of ptrI64, for the optional terms of a
// decoded coordinate.
//
//go:fix inline
func ptrI32(v int32) *int32 { return new(v) }

// decodeRID is the whole decision, pure.
//
// It never returns an error: the four non-OK statuses are the answers for the
// four ways a number can fail to be a coordinate, and each carries a Summary
// saying which. In particular a RID in (C·M, MaxRID] yields
// ridDecodeOutsideMatrix with both ceilings named — ridcoord.Decode's refusal
// is consumed here, not surfaced.
func decodeRID(rid int32, res ridResolution) ridDecodeResult {
	r := ridDecodeResult{RID: rid, Layout: res.Layout, LayoutSource: res.LayoutSource}

	// The layout is read first, because when it can produce no segment at all
	// nothing downstream of it means anything — and reporting the RID as out
	// of range would send the reader after the wrong thing.
	if err := res.Layout.Validate(); err != nil {
		r.Status = ridDecodeLayoutUnusable
		r.Summary = fmt.Sprintf(
			"rid %d could not be decoded: the RID layout %s can produce no segment (%v). "+
				"The fault is the layout, not the number.",
			rid, describeRIDLayoutAxes(res.Layout), err)
		return r
	}

	if note := untrustedLayoutNote(res); note != "" {
		r.Notes = append(r.Notes, note)
	}

	m := res.Layout.SegmentWidth()
	last := res.Layout.MaxSegments() * m

	switch {
	case rid == 0:
		r.Status = ridDecodeReserved
		r.Summary = "rid 0 is reserved and is never allocated (ADR-031 decision 1), so it names no " +
			"provider at all — look for a stanza whose replica ID was never assigned."
		return r
	case rid < 0 || rid > ridcoord.MaxRID:
		r.Status = ridDecodeOutsideBand
		r.Summary = fmt.Sprintf(
			"rid %d is not a syncrepl replica ID at all: slapd's replica IDs are 0..%d, "+
				"and it would refuse this one itself.",
			rid, ridcoord.MaxRID)
		return r
	case rid > last:
		r.Status = ridDecodeOutsideMatrix
		r.Summary = fmt.Sprintf(
			"rid %d is inside slapd's replica-ID band (0..%d) but above the largest RID this layout "+
				"produces (%d = %d segments × %d) — a legal replica ID this mesh never issued. "+
				"Most likely a stanza written before the ADR-031 renumber, a hand edit, or another tool.",
			rid, ridcoord.MaxRID, last, res.Layout.MaxSegments(), m)
		return r
	}

	coord, err := res.Layout.Decode(rid)
	if err != nil {
		// Unreachable: every refusal Decode can raise is answered above. Kept
		// so a future change to ridcoord cannot turn into a silent zero.
		r.Status = ridDecodeOutsideMatrix
		r.Summary = fmt.Sprintf("rid %d could not be decoded against this layout: %v", rid, err)
		return r
	}

	r.Status = ridDecodeOK
	r.Segment = new(coord.Segment)
	r.Kind = coord.Kind

	segmentPart, segNotes := resolveRIDSegment(coord.Segment, res)
	r.Databases = segNotes.databases
	r.Notes = append(r.Notes, segNotes.notes...)

	if coord.Kind == ridcoord.KindLocal {
		r.Ordinal = new(coord.Ordinal)
		providerPart, pod, notes := resolveRIDLocal(coord.Ordinal, res)
		r.Pod = pod
		r.Notes = append(r.Notes, notes...)
		r.Summary = fmt.Sprintf("rid %d → %s, %s", rid, segmentPart, providerPart)
		return r
	}

	r.SiteIndex = new(coord.SiteIndex)
	providerPart, site, notes := resolveRIDRemote(coord.SiteIndex, res)
	r.Site = site
	r.Notes = append(r.Notes, notes...)
	r.Summary = fmt.Sprintf("rid %d → %s, %s", rid, segmentPart, providerPart)
	return r
}

// untrustedLayoutNote says, in words, that every term below is conditional on a
// layout that was never actually read. The operator withholds in this state
// (RID-PLAN hazard 3); a diagnostic was asked a question, so it answers against
// the defaults and labels the answer.
func untrustedLayoutNote(res ridResolution) string {
	if res.LayoutTrusted {
		return ""
	}
	where := "the mesh it is declared in"
	if res.MeshName != "" {
		where = fmt.Sprintf("SlapdMesh %q", res.MeshName)
	}
	return fmt.Sprintf(
		"the RID layout could not be read from %s — decoded against the defaults (%s), "+
			"so every term below is conditional on that assumption",
		where, describeRIDLayoutAxes(res.Layout))
}

// ridSegmentResolution is what the segment term could be made of.
type ridSegmentResolution struct {
	databases []string
	notes     []string
}

// resolveRIDSegment turns a segment number into the database(s) declaring it,
// returning the phrase for the summary alongside whatever could not be
// resolved.
func resolveRIDSegment(segment int32, res ridResolution) (string, ridSegmentResolution) {
	out := ridSegmentResolution{}

	if res.Databases == nil {
		out.notes = append(out.notes, fmt.Sprintf(
			"no cluster was consulted, so segment %d could not be resolved to a database "+
				"(point slctl at a cluster, or pass --cluster, to name it)", segment))
		return fmt.Sprintf("segment %d", segment), out
	}

	out.databases = res.Databases[segment]
	switch len(out.databases) {
	case 0:
		out.notes = append(out.notes, fmt.Sprintf(
			"no SlapdDatabase of cluster %q declares ridSegment %d",
			res.ClusterName, segment))
		return fmt.Sprintf("segment %d", segment), out
	case 1:
		return fmt.Sprintf("segment %d (database %q)", segment, out.databases[0]), out
	default:
		out.notes = append(out.notes, fmt.Sprintf(
			"ridSegment %d is declared by %d databases (%s) — ADR-031 decision 3 refuses that, "+
				"so one of them is misconfigured and their stanzas collide",
			segment, len(out.databases), strings.Join(out.databases, ", ")))
		return fmt.Sprintf("segment %d (databases %s)", segment, strings.Join(out.databases, ", ")), out
	}
}

// resolveRIDLocal words the local block.
//
// A local offset names "the pod at ordinal N of the site whose stanza this is",
// NOT "pod N of the cluster slctl happens to be pointed at". Decision 1 makes
// the local block uniform across sites precisely so that decoding needs no
// knowledge of where the reader stands; the pod name is offered as what that
// ordinal means where the stanza came from, and nothing here asserts the number
// was read at this site.
func resolveRIDLocal(ordinal int32, res ridResolution) (string, string, []string) {
	var notes []string

	if res.ClusterName == "" {
		notes = append(notes, fmt.Sprintf(
			"no cluster was consulted, so local ordinal %d could not be given a pod name", ordinal))
		return fmt.Sprintf("local slot: the pod at ordinal %d of the site this stanza came from", ordinal),
			"", notes
	}

	pod := fmt.Sprintf("%s-%d", res.ClusterName, ordinal)
	if res.Replicas > 0 && ordinal >= res.Replicas {
		notes = append(notes, fmt.Sprintf(
			"cluster %q declares %d replicas, so ordinal %d names no pod it currently runs — "+
				"a stanza left over from a larger fleet, or a stanza from a site that is larger than this one",
			res.ClusterName, res.Replicas, ordinal))
	}
	return fmt.Sprintf(
			"local slot: the pod at ordinal %d of the site this stanza came from (%s, by the naming of cluster %q here)",
			ordinal, pod, res.ClusterName),
		pod, notes
}

// resolveRIDRemote words the remote block: a link to a peer SITE, never to a
// particular pod at it (ADR-031 decision 1).
func resolveRIDRemote(siteIndex int32, res ridResolution) (string, string, []string) {
	var notes []string

	if res.Sites == nil {
		switch {
		case res.MeshName != "":
			notes = append(notes, fmt.Sprintf(
				"SlapdMesh %q could not be read, so peer site index %d could not be resolved to a site name",
				res.MeshName, siteIndex))
		case res.ClusterName != "":
			notes = append(notes, fmt.Sprintf(
				"cluster %q names no SlapdMesh, so peer site index %d could not be resolved to a site name",
				res.ClusterName, siteIndex))
		default:
			notes = append(notes, fmt.Sprintf(
				"no cluster was consulted, so peer site index %d could not be resolved to a site name", siteIndex))
		}
		return fmt.Sprintf("remote slot: the link to peer site index %d", siteIndex), "", notes
	}

	site, ok := res.Sites[siteIndex]
	if !ok {
		mesh := res.MeshName
		if mesh == "" {
			mesh = "the mesh"
		}
		notes = append(notes, fmt.Sprintf(
			"no site of SlapdMesh %q declares serverIDIndex %d — a retired site, or a stanza "+
				"written before the site list changed", mesh, siteIndex))
		return fmt.Sprintf("remote slot: the link to peer site index %d", siteIndex), "", notes
	}
	return fmt.Sprintf("remote slot: the link to peer site index %d (%q)", siteIndex, site), site, notes
}

// describeRIDLayoutAxes spells a layout's two declared axes. Deliberately only
// the axes: the derived quantities are meaningless for a layout that cannot
// produce a segment, which is one of the two places this is used.
func describeRIDLayoutAxes(l ridcoord.Layout) string {
	return fmt.Sprintf("%d pods/site × %d sites", l.PodsPerSite, l.MaxSites)
}

// parseRIDLayoutFlag parses the --layout value, `<podsPerSite>x<maxSites>` —
// `8x12` being the default layout. The order is pods first, sites second,
// matching ADR-031 decision 5's own table and the `M = P + S` formula.
//
// A syntactically well-formed layout that cannot produce a segment is returned
// as-is with a nil error: it is the decode's job to report an unusable layout
// as ridDecodeLayoutUnusable, so that the message names the arithmetic rather
// than the flag. Only a value that is not two numbers is a parse error.
func parseRIDLayoutFlag(s string) (ridcoord.Layout, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), "x")
	if len(parts) != 2 {
		return ridcoord.Layout{}, fmt.Errorf(
			"layout %q is not <podsPerSite>x<maxSites> (for example 8x12)", s)
	}
	// ParseInt with a bitSize of 32 and not a conversion afterwards: 2^32 + 8
	// converts to a perfectly plausible 8, and the value is wrong only once the
	// width of the destination is applied to it.
	p, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 32)
	if err != nil {
		return ridcoord.Layout{}, fmt.Errorf("layout %q: podsPerSite %q is not a 32-bit number", s, parts[0])
	}
	sites, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 32)
	if err != nil {
		return ridcoord.Layout{}, fmt.Errorf("layout %q: maxSites %q is not a 32-bit number", s, parts[1])
	}
	return ridcoord.Layout{PodsPerSite: int32(p), MaxSites: int32(sites)}, nil
}

// buildRIDResolution assembles the naming context from what could be read.
//
// All three inputs are optional, and the shape of the answer follows what was
// supplied:
//
//   - sc nil: the offline base case. Default layout, trusted, no maps.
//   - sc without meshRef: default layout, trusted, databases resolved.
//   - sc with meshRef, mesh non-nil: the mesh's spec.ridLayout, defaulted PER
//     AXIS exactly as the operator's own ridLayoutFor does — a zero axis is
//     unset, not unusable, because API-server defaulting does not run on a
//     manifest read off disk.
//   - sc with meshRef, mesh nil: the mesh could not be read. The default layout
//     stands in, LayoutTrusted is false, and the decode says so. The operator
//     withholds here; a diagnostic answers and labels the answer.
//
// dbs are filtered to those naming sc, and only those declaring a ridSegment.
// They are ignored entirely when sc is nil, since clusterRef cannot be matched
// against anything.
func buildRIDResolution(sc *ldapv1alpha1.SlapdCluster, mesh *ldapv1alpha1.SlapdMesh, dbs []ldapv1alpha1.SlapdDatabase) ridResolution {
	res := ridResolution{
		Layout:        ridcoord.DefaultLayout(),
		LayoutSource:  "the compiled-in default",
		LayoutTrusted: true,
	}
	if sc == nil {
		return res
	}

	res.ClusterName = sc.Name
	res.Replicas = sc.Spec.Replicas

	// A non-nil map even when empty: "consulted, declares nothing" is a finding
	// about the cluster and must not read like "never looked".
	res.Databases = map[int32][]string{}
	for _, db := range dbs {
		if db.Spec.ClusterRef != sc.Name || db.Spec.Replication.RIDSegment == nil {
			continue
		}
		seg := *db.Spec.Replication.RIDSegment
		res.Databases[seg] = append(res.Databases[seg], db.Name)
	}
	for seg := range res.Databases {
		sort.Strings(res.Databases[seg])
	}

	if sc.Spec.MeshRef == "" {
		return res
	}
	res.MeshName = sc.Spec.MeshRef

	if mesh == nil {
		res.LayoutSource = fmt.Sprintf("SlapdMesh %q (unreadable; showing the defaults)", sc.Spec.MeshRef)
		res.LayoutTrusted = false
		return res
	}

	res.LayoutSource = fmt.Sprintf("SlapdMesh %q", sc.Spec.MeshRef)
	// Defaulted per axis, exactly as the operator's ridLayoutFor does: a zero
	// axis is unset, not unusable, because API-server defaulting runs on the
	// API server's path and nowhere else.
	if rl := mesh.Spec.RIDLayout; rl != nil {
		if rl.PodsPerSite != 0 {
			res.Layout.PodsPerSite = rl.PodsPerSite
		}
		if rl.MaxSites != 0 {
			res.Layout.MaxSites = rl.MaxSites
		}
	}

	// Indexed by the DECLARED serverIDIndex (ADR-028), never by list position:
	// indexing by position renames every site the moment someone sorts the YAML.
	res.Sites = map[int32]string{}
	for _, site := range mesh.Spec.Sites {
		if site.ServerIDIndex == nil {
			continue
		}
		res.Sites[*site.ServerIDIndex] = site.Name
	}
	return res
}

// renderRIDDecodeText renders the human form: the number, the layout it was
// read against, the terms it decomposes into, and the notes. It is the text a
// person reads at 3am, so every resolved name appears next to the number it
// resolves.
func renderRIDDecodeText(r ridDecodeResult) string {
	var b strings.Builder

	fmt.Fprintf(&b, "RID %d\n", r.RID)
	b.WriteString("────────────────────────────────────────\n")
	fmt.Fprintf(&b, "  Status:     %s\n", r.Status)
	fmt.Fprintf(&b, "  Layout:     %s%s (%s)\n",
		describeRIDLayoutAxes(r.Layout), renderRIDLayoutDerived(r.Layout), r.LayoutSource)

	if r.Segment != nil {
		seg := fmt.Sprintf("%d", *r.Segment)
		if len(r.Databases) > 0 {
			seg += " — database " + strings.Join(r.Databases, ", ")
		}
		fmt.Fprintf(&b, "  Segment:    %s\n", seg)
	}
	switch {
	case r.Ordinal != nil:
		prov := fmt.Sprintf("pod ordinal %d of the site this stanza came from", *r.Ordinal)
		if r.Pod != "" {
			prov += " — " + r.Pod
		}
		fmt.Fprintf(&b, "  Provider:   %s\n", prov)
	case r.SiteIndex != nil:
		prov := fmt.Sprintf("peer site index %d", *r.SiteIndex)
		if r.Site != "" {
			prov += " — site " + r.Site
		}
		fmt.Fprintf(&b, "  Provider:   %s\n", prov)
	}

	fmt.Fprintf(&b, "\n  %s\n", r.Summary)

	if len(r.Notes) > 0 {
		b.WriteString("\nNotes:\n")
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "  - %s\n", n)
		}
	}
	return b.String()
}

// renderRIDLayoutDerived appends the derived quantities, and only when the
// layout actually has any: a layout that can produce no segment would otherwise
// print "M = 0, 0 segments" next to the sentence explaining why it cannot.
func renderRIDLayoutDerived(l ridcoord.Layout) string {
	m := l.SegmentWidth()
	if m == 0 {
		return ""
	}
	return fmt.Sprintf(" → M = %d, %d segments, highest RID %d", m, l.MaxSegments(), l.MaxSegments()*m)
}

// ridLayoutJSON is the layout as emitted, with the two derived quantities
// included so a consumer need not re-implement the formula to know where the
// matrix ends.
type ridLayoutJSON struct {
	PodsPerSite  int32  `json:"podsPerSite"`
	MaxSites     int32  `json:"maxSites"`
	SegmentWidth int32  `json:"segmentWidth"`
	MaxSegments  int32  `json:"maxSegments"`
	Source       string `json:"source"`
	Trusted      bool   `json:"trusted"`
}

// ridDecodeJSON is the --json form of a decode.
type ridDecodeJSON struct {
	RID       int32         `json:"rid"`
	Status    string        `json:"status"`
	Layout    ridLayoutJSON `json:"layout"`
	Segment   *int32        `json:"segment,omitempty"`
	Kind      string        `json:"kind,omitempty"`
	Ordinal   *int32        `json:"ordinal,omitempty"`
	SiteIndex *int32        `json:"siteIndex,omitempty"`
	Databases []string      `json:"databases,omitempty"`
	Pod       string        `json:"pod,omitempty"`
	Site      string        `json:"site,omitempty"`
	Summary   string        `json:"summary"`
	Notes     []string      `json:"notes,omitempty"`
}

// buildRIDDecodeJSON projects a result onto the emitted form.
func buildRIDDecodeJSON(r ridDecodeResult, trusted bool) ridDecodeJSON {
	j := ridDecodeJSON{
		RID:    r.RID,
		Status: string(r.Status),
		Layout: ridLayoutJSON{
			PodsPerSite:  r.Layout.PodsPerSite,
			MaxSites:     r.Layout.MaxSites,
			SegmentWidth: r.Layout.SegmentWidth(),
			MaxSegments:  r.Layout.MaxSegments(),
			Source:       r.LayoutSource,
			Trusted:      trusted,
		},
		Segment:   r.Segment,
		Ordinal:   r.Ordinal,
		SiteIndex: r.SiteIndex,
		Databases: r.Databases,
		Pod:       r.Pod,
		Site:      r.Site,
		Summary:   r.Summary,
		Notes:     r.Notes,
	}
	// The zero Kind is emitted as nothing at all: a non-coordinate has no kind,
	// and Kind.String() would print "Kind(0)" into a consumer's field.
	if r.Kind == ridcoord.KindLocal || r.Kind == ridcoord.KindRemote {
		j.Kind = r.Kind.String()
	}
	return j
}

var (
	ridLayoutFlag  string
	ridOfflineFlag bool
	ridClusterFlag string
)

var ridCmd = &cobra.Command{
	Use:   "rid",
	Short: "Work with syncrepl replica IDs",
	Long:  "Commands for reading and checking syncrepl replica IDs (RIDs), which ADR-031 makes coordinates in a (database, provider) matrix.",
}

var ridDecodeCmd = &cobra.Command{
	Use:   "decode <rid>",
	Short: "Decode a syncrepl RID into (database, provider)",
	Long: "Decode a syncrepl replica ID into the database segment and the provider it names.\n\n" +
		"Works offline against the default RID layout (8x12), which is all a RID needs to be\n" +
		"decoded. When a cluster is reachable, the segment is resolved to the SlapdDatabase\n" +
		"declaring it, the peer site index to the site declaring it, and the layout is read\n" +
		"from the SlapdMesh actually in force.",
	Args: cobra.ExactArgs(1),
	RunE: runRIDDecode,
}

func init() {
	ridCmd.AddCommand(ridDecodeCmd)
	rootCmd.AddCommand(ridCmd)

	ridDecodeCmd.Flags().StringVar(&ridLayoutFlag, "layout", "",
		"RID layout to decode against, as <podsPerSite>x<maxSites> (default: the mesh's, else 8x12)")
	ridDecodeCmd.Flags().BoolVar(&ridOfflineFlag, "offline", false,
		"do not contact a cluster; decode arithmetically only")
	ridDecodeCmd.Flags().StringVar(&ridClusterFlag, "cluster", "",
		"SlapdCluster to resolve segments and sites against (only needed when the namespace holds more than one)")
}

func runRIDDecode(cmd *cobra.Command, args []string) error {
	// A number that is not a number is the one thing this command refuses: the
	// five statuses are answers about RIDs, and "banana" is not a RID a human
	// read off a stanza, it is a typo.
	n, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 32)
	if err != nil {
		return fmt.Errorf("%q is not a replica ID: expected a number in 0..%d", args[0], ridcoord.MaxRID)
	}
	rid := int32(n)

	res := resolveRIDContext(cmd)

	if ridLayoutFlag != "" {
		layout, err := parseRIDLayoutFlag(ridLayoutFlag)
		if err != nil {
			return err
		}
		res.Layout = layout
		res.LayoutSource = fmt.Sprintf("--layout %s", ridLayoutFlag)
		res.LayoutTrusted = true
	}

	result := decodeRID(rid, res)

	if jsonOutput {
		out, err := json.MarshalIndent(buildRIDDecodeJSON(result, res.LayoutTrusted), "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(out))
		return nil
	}
	fmt.Fprint(cmd.OutOrStdout(), renderRIDDecodeText(result))
	return nil
}

// resolveRIDContext reads as much naming context as the environment will give
// up. Nothing in here is allowed to fail the command: a decode against the
// compiled-in default is the base case, so every unreadable input degrades to
// the offline answer with a line on stderr saying which input was unreadable.
func resolveRIDContext(cmd *cobra.Command) ridResolution {
	if ridOfflineFlag {
		return buildRIDResolution(nil, nil, nil)
	}

	ctx := context.Background()
	k8sClient, _, _, ns, err := initClient()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Note: no cluster could be contacted (%v); decoding offline.\n", err)
		return buildRIDResolution(nil, nil, nil)
	}

	sc, err := resolveRIDCluster(ctx, k8sClient, ns)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Note: %v; decoding offline.\n", err)
		return buildRIDResolution(nil, nil, nil)
	}

	dbList := &ldapv1alpha1.SlapdDatabaseList{}
	if err := k8sClient.List(ctx, dbList, client.InNamespace(ns)); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Note: SlapdDatabases could not be listed (%v).\n", err)
	}

	var mesh *ldapv1alpha1.SlapdMesh
	if sc.Spec.MeshRef != "" {
		m := &ldapv1alpha1.SlapdMesh{}
		key := client.ObjectKey{Name: sc.Spec.MeshRef, Namespace: ns}
		if err := k8sClient.Get(ctx, key, m); err != nil {
			// Deliberately not fatal, and deliberately not silent: the decode
			// labels its own answer untrusted.
			fmt.Fprintf(cmd.ErrOrStderr(), "Note: SlapdMesh %q could not be read (%v).\n", sc.Spec.MeshRef, err)
		} else {
			mesh = m
		}
	}
	return buildRIDResolution(sc, mesh, dbList.Items)
}

// resolveRIDCluster picks the SlapdCluster whose declarations name the segments
// and sites. One in the namespace needs no flag; more than one does, because
// guessing would silently resolve a segment against the wrong RID space.
func resolveRIDCluster(ctx context.Context, k8sClient client.Client, ns string) (*ldapv1alpha1.SlapdCluster, error) {
	name := ridClusterFlag
	if name == "" {
		name = namespaceSingleClusterName(ctx, k8sClient, ns)
	}
	if name == "" {
		return nil, fmt.Errorf("no single SlapdCluster in namespace %q to resolve against (pass --cluster)", ns)
	}
	sc := &ldapv1alpha1.SlapdCluster{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, sc); err != nil {
		return nil, fmt.Errorf("SlapdCluster %q could not be read (%w)", name, err)
	}
	return sc, nil
}

// namespaceSingleClusterName returns the name of the namespace's one
// SlapdCluster, or "" when there is none or more than one.
func namespaceSingleClusterName(ctx context.Context, k8sClient client.Client, ns string) string {
	list := &ldapv1alpha1.SlapdClusterList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(ns)); err != nil {
		return ""
	}
	if len(list.Items) != 1 {
		return ""
	}
	return list.Items[0].Name
}
