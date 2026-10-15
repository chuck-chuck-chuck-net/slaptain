/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-ldap/ldap/v3"
)

// recordingWriter stands in for *ldap.Conn. It records what reached the wire,
// which is the only thing the guard's test is about: a refused request must
// leave this empty.
type recordingWriter struct {
	modifies []*ldap.ModifyRequest
	adds     []*ldap.AddRequest
	err      error
}

func (w *recordingWriter) Modify(req *ldap.ModifyRequest) error {
	w.modifies = append(w.modifies, req)
	return w.err
}

func (w *recordingWriter) Add(req *ldap.AddRequest) error {
	w.adds = append(w.adds, req)
	return w.err
}

// TestStartupOnlyAttrViolation pins the deny-list's matching rules. The
// attribute is matched the way slapd matches an attribute description:
// case-insensitively, and ignoring any ;option suffix — an operator that got
// past the guard by sending olcspsessionlogsource would be a double free
// (ADR-034 context, sp_cf_gen).
func TestStartupOnlyAttrViolation(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  string
	}{
		{"nothing", nil, ""},
		{"ordinary attributes", []string{"olcSpSessionlog", "olcSpCheckpoint", "olcSyncRepl"}, ""},
		{"the denied attribute", []string{"olcSpSessionlogSource"}, "olcSpSessionlogSource"},
		{"lowercased", []string{"olcspsessionlogsource"}, "olcspsessionlogsource"},
		{"uppercased", []string{"OLCSPSESSIONLOGSOURCE"}, "OLCSPSESSIONLOGSOURCE"},
		{"with an option suffix", []string{"olcSpSessionlogSource;x-ordered"}, "olcSpSessionlogSource;x-ordered"},
		{"with surrounding whitespace", []string{" olcSpSessionlogSource "}, " olcSpSessionlogSource "},
		{"a denied one among many", []string{
			"objectClass", "olcOverlay", "olcSpSessionlogSource", "olcSpCheckpoint",
		}, "olcSpSessionlogSource"},
		{"a near miss is allowed", []string{"olcSpSessionlogSourceExtra"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := startupOnlyAttrViolation(tt.names); got != tt.want {
				t.Errorf("startupOnlyAttrViolation(%v) = %q, want %q", tt.names, got, tt.want)
			}
		})
	}
}

// TestGuardedModifyRefusesStartupOnlyAttribute: the request never reaches the
// connection. ADR-034 R2 — a Modify carrying olcSpSessionlogSource kills the
// server it is sent to, so "returns an error" is not the assertion; "did not
// send it" is.
func TestGuardedModifyRefusesStartupOnlyAttribute(t *testing.T) {
	for _, op := range []string{"replace", "add", "delete"} {
		t.Run(op, func(t *testing.T) {
			w := &recordingWriter{}
			req := ldap.NewModifyRequest("olcOverlay={0}syncprov,olcDatabase={1}mdb,cn=config", nil)
			switch op {
			case "replace":
				req.Replace(startupAttrSessionlogSource, []string{"cn=accesslog-default,dc=x"})
			case "add":
				req.Add(startupAttrSessionlogSource, []string{"cn=accesslog-default,dc=x"})
			case "delete":
				req.Delete(startupAttrSessionlogSource, nil)
			}

			err := guardedModify(w, req)
			if err == nil {
				t.Fatal("guardedModify accepted a startup-only attribute")
			}
			if !strings.Contains(err.Error(), startupAttrSessionlogSource) {
				t.Errorf("error %q does not name the attribute", err)
			}
			if !strings.Contains(err.Error(), "ADR-034") {
				t.Errorf("error %q does not point at ADR-034", err)
			}
			if len(w.modifies) != 0 {
				t.Fatalf("the request reached the connection: %d modify(s) sent", len(w.modifies))
			}
		})
	}
}

// TestGuardedAddRefusesStartupOnlyAttribute: an LDAP Add of the overlay entry
// carrying the attribute takes the SAME crashing path as a Modify —
// config_add_internal sets lineno = 0 for any LDAP operation, so
// CONFIG_ONLINE_ADD is true there too (ADR-034 context, fact 1). Guarding only
// Modify would be a false comfort.
func TestGuardedAddRefusesStartupOnlyAttribute(t *testing.T) {
	w := &recordingWriter{}
	req := ldap.NewAddRequest("olcOverlay=syncprov,olcDatabase={1}mdb,cn=config", nil)
	req.Attribute("objectClass", []string{"olcOverlayConfig", "olcSyncProvConfig"})
	req.Attribute("olcOverlay", []string{"syncprov"})
	req.Attribute(startupAttrSessionlogSource, []string{"cn=accesslog-default,dc=x"})

	err := guardedAdd(w, req)
	if err == nil {
		t.Fatal("guardedAdd accepted a startup-only attribute")
	}
	if !strings.Contains(err.Error(), startupAttrSessionlogSource) {
		t.Errorf("error %q does not name the attribute", err)
	}
	if len(w.adds) != 0 {
		t.Fatalf("the request reached the connection: %d add(s) sent", len(w.adds))
	}
}

// TestGuardedWritesPassOrdinaryRequestsThrough: the guard is a mechanical
// interposition. An ordinary request is forwarded unchanged and the
// connection's own error is returned verbatim — same errors, same wrapping, so
// every caller's ldap.IsErrorWithCode check keeps working.
func TestGuardedWritesPassOrdinaryRequestsThrough(t *testing.T) {
	t.Run("modify is forwarded", func(t *testing.T) {
		w := &recordingWriter{}
		req := ldap.NewModifyRequest("olcDatabase={1}mdb,cn=config", nil)
		req.Replace("olcSpSessionlog", []string{"5000"})
		if err := guardedModify(w, req); err != nil {
			t.Fatalf("guardedModify returned %v", err)
		}
		if len(w.modifies) != 1 || w.modifies[0] != req {
			t.Fatalf("request was not forwarded unchanged: %#v", w.modifies)
		}
	})

	t.Run("add is forwarded", func(t *testing.T) {
		w := &recordingWriter{}
		req := ldap.NewAddRequest("olcOverlay=syncprov,olcDatabase={1}mdb,cn=config", nil)
		req.Attribute("olcOverlay", []string{"syncprov"})
		if err := guardedAdd(w, req); err != nil {
			t.Fatalf("guardedAdd returned %v", err)
		}
		if len(w.adds) != 1 || w.adds[0] != req {
			t.Fatalf("request was not forwarded unchanged: %#v", w.adds)
		}
	})

	t.Run("the connection's error is returned verbatim", func(t *testing.T) {
		sentinel := ldap.NewError(ldap.LDAPResultEntryAlreadyExists, errors.New("already there"))
		w := &recordingWriter{err: sentinel}
		req := ldap.NewAddRequest("olcDatabase=mdb,cn=config", nil)
		req.Attribute("olcSuffix", []string{"dc=x"})
		err := guardedAdd(w, req)
		if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			t.Fatalf("error was not passed through verbatim: %v", err)
		}

		w2 := &recordingWriter{err: sentinel}
		mreq := ldap.NewModifyRequest("cn=config", nil)
		mreq.Replace("olcLogLevel", []string{"256"})
		if err := guardedModify(w2, mreq); !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			t.Fatalf("error was not passed through verbatim: %v", err)
		}
	})
}

// ── The chokepoint has to stay the only one ──────────────────────────────────

// rawLDAPWriteAllowList names every place in this package that may still call
// an LDAP connection's Modify or Add directly, with the reason.
//
// All of them write into a DATA TREE, never into cn=config. ADR-034's guard is
// about cn=config: an attribute slapd's configuration parser mishandles on the
// online path. A data-tree entry goes through the backend, not through
// config_add_internal, so there is nothing for the guard to protect and routing
// those through it would only blur what the list means.
//
// A new entry here is the cost of a new raw write, and it is meant to be paid
// consciously: the next startup-only attribute will be written by a function
// that does not exist yet, somewhere nowhere near this file.
var rawLDAPWriteAllowList = map[string]string{
	"slapdcluster_authdb.go:ensureAuthSuffixEntry":      "adds cn=slaptain-auth, the node-local auth database's own suffix entry (ADR-027) — a data-tree add",
	"slapddatabase_authdb.go:ensureAuthIdentity":        "adds and converges cn=repl-<db>,cn=slaptain-auth in the auth database (ADR-027) — data-tree writes",
	"slapddatabase_controller.go:ensureReplicationUser": "adds the legacy cn=replication,<suffix> entry in the replicated data tree (ADR-027 migration window)",
	"slapddatabase_controller.go:applySeedEntry":        "applies the user's seed LDIF into the data tree (ADR-012)",
}

// TestNoRawLDAPWritesOutsideTheGuard fails when a new conn.Modify / conn.Add
// appears in this package outside the guard and outside the allow-list above.
//
// This is the test that gives ADR-034 R2 a future. The guard only helps where
// it is called, and the attribute that needs it next will be written by code
// nobody has written yet — olcDbMaxSize already lives in
// slapddatabase_tunables.go, nowhere near the syncprov code. So the invariant
// worth pinning is not "the guard is correct" but "there is one chokepoint".
//
// The receiver heuristic: a selector call .Modify(…) / .Add(…) whose receiver
// is an identifier whose name contains "conn", case-insensitively. That is how
// every LDAP connection in this package is named, and it keeps time.Add,
// client.Add and the builders' req.Add out of the result.
func TestNoRawLDAPWritesOutsideTheGuard(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	var found []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "startup_only_guard.go" {
			continue // the chokepoint itself
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Modify" && sel.Sel.Name != "Add") {
					return true
				}
				recv, ok := sel.X.(*ast.Ident)
				if !ok || !strings.Contains(strings.ToLower(recv.Name), "conn") {
					return true
				}
				key := name + ":" + fn.Name.Name
				if _, allowed := rawLDAPWriteAllowList[key]; allowed {
					return true
				}
				found = append(found, key+" → "+recv.Name+"."+sel.Sel.Name+"() at "+
					fset.Position(call.Pos()).String())
				return true
			})
		}
	}

	if len(found) > 0 {
		sort.Strings(found)
		t.Fatalf("raw LDAP write(s) outside the ADR-034 guard:\n  %s\n\n"+
			"Route cn=config writes through guardedModify/guardedAdd. If the write targets a "+
			"DATA TREE, add it to rawLDAPWriteAllowList with the reason.",
			strings.Join(found, "\n  "))
	}
}

// TestRawLDAPWriteAllowListIsNotStale fails when an allow-list entry names a
// site that no longer exists. Without it the list rots into a set of excuses
// for code that is gone, and a future raw write landing in a renamed-but-listed
// function would pass unseen.
func TestRawLDAPWriteAllowListIsNotStale(t *testing.T) {
	fset := token.NewFileSet()
	live := map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Modify" && sel.Sel.Name != "Add") {
					return true
				}
				if recv, ok := sel.X.(*ast.Ident); ok &&
					strings.Contains(strings.ToLower(recv.Name), "conn") {
					live[name+":"+fn.Name.Name] = true
				}
				return true
			})
		}
	}
	for key := range rawLDAPWriteAllowList {
		if !live[key] {
			t.Errorf("allow-list entry %q no longer names a raw LDAP write — remove it", key)
		}
	}
}
