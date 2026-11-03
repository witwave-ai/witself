package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type principalCatalogRow struct {
	role  *syncPrincipalRoleCatalog
	valid bool
	err   error
}

func (r principalCatalogRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.role == nil {
		if len(dest) != 1 {
			return errors.New("principal catalog fixture destination mismatch")
		}
		value, ok := dest[0].(*bool)
		if !ok {
			return errors.New("principal catalog fixture destination mismatch")
		}
		*value = r.valid
		return nil
	}
	values := []any{r.role.name, r.role.superuser, r.role.createRole, r.role.createDB,
		r.role.bypassRLS, r.role.inherit, r.role.replication, r.role.canLogin,
		r.role.membership, r.role.replicationParameter}
	if len(dest) != len(values) {
		return errors.New("principal catalog fixture destination mismatch")
	}
	for i, value := range values {
		switch value := value.(type) {
		case string:
			target, ok := dest[i].(*string)
			if !ok {
				return errors.New("principal catalog fixture destination mismatch")
			}
			*target = value
		case bool:
			target, ok := dest[i].(*bool)
			if !ok {
				return errors.New("principal catalog fixture destination mismatch")
			}
			*target = value
		}
	}
	return nil
}

// This catalog contains individual role rows, never an aggregate preflight
// verdict. Every query must select the reviewed fields and exactly one name.
type principalCatalogReader struct {
	roles     map[string]syncPrincipalRoleCatalog
	topology  map[syncPrincipalTopologyPredicate]bool
	failQuery string
	calls     int
	queries   []string
	args      [][]any
}

func newPrincipalCatalogReader() *principalCatalogReader {
	r := &principalCatalogReader{
		roles:    make(map[string]syncPrincipalRoleCatalog),
		topology: make(map[syncPrincipalTopologyPredicate]bool),
	}
	for i, name := range syncPrincipalRoleNames() {
		r.roles[name] = syncPrincipalRoleCatalog{name: name, canLogin: i != 0}
	}
	for _, predicate := range syncPrincipalTopologyPredicates {
		r.topology[predicate] = true
	}
	return r
}

func (r *principalCatalogReader) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	r.calls++
	r.queries = append(r.queries, query)
	r.args = append(r.args, args)
	if query == r.failQuery {
		return principalCatalogRow{err: errors.New("private fixture marker")}
	}
	if query == syncPrincipalRoleCatalogSQL {
		const fields = "SELECT r.rolname,r.rolsuper,r.rolcreaterole,r.rolcreatedb,\n r.rolbypassrls,r.rolinherit,r.rolreplication,r.rolcanlogin,"
		if len(args) != 1 || !strings.HasPrefix(query, fields) ||
			!strings.Contains(query, "EXISTS(SELECT 1 FROM pg_auth_members m WHERE m.member=r.oid)") ||
			!strings.Contains(query, "has_parameter_privilege(r.oid,'session_replication_role','SET')") ||
			!strings.HasSuffix(query, "FROM pg_roles r WHERE r.rolname=$1") {
			return principalCatalogRow{err: errors.New("principal catalog fixture role query mismatch")}
		}
		name, ok := args[0].(string)
		if !ok {
			return principalCatalogRow{err: errors.New("principal catalog fixture name mismatch")}
		}
		role, found := r.roles[name]
		if !found {
			return principalCatalogRow{err: pgx.ErrNoRows}
		}
		return principalCatalogRow{role: &role}
	}
	predicates := map[string]syncPrincipalTopologyPredicate{
		syncPrincipalOwnerIdentitySQL: syncPrincipalTopologyOwnerIdentity,
		syncPrincipalFunctionACLSQL:   syncPrincipalTopologyFunctionACL,
		syncPrincipalOwnerDrainSQL:    syncPrincipalTopologyOwnerDrain,
	}
	predicate, ok := predicates[query]
	if !ok {
		return principalCatalogRow{err: errors.New("principal catalog fixture unexpected query")}
	}
	return principalCatalogRow{valid: r.topology[predicate]}
}

func TestSyncPrincipalRoleCatalogMissingEachRole(t *testing.T) {
	for index, name := range syncPrincipalRoleNames() {
		t.Run(name, func(t *testing.T) {
			reader := newPrincipalCatalogReader()
			delete(reader.roles, name)
			if len(reader.roles) != 5 {
				t.Fatal("principal catalog missing-role fixture is not isolated")
			}
			for _, other := range syncPrincipalRoleNames() {
				if other != name {
					if _, ok := reader.roles[other]; !ok {
						t.Fatal("principal catalog missing-role fixture removed another identity")
					}
				}
			}
			if verifySyncPrincipals(context.Background(), reader, syncPrincipalRoleNames()) != ErrSyncPrincipalsMissing {
				t.Fatal("principal preflight accepted a missing reviewed identity")
			}
			if reader.calls != index+1 || reader.args[index][0] != name {
				t.Fatal("principal preflight did not inspect the isolated missing identity")
			}
		})
	}
}

func TestSyncPrincipalRoleCatalogForbiddenAttributes(t *testing.T) {
	attributes := []struct {
		name         string
		nonownerOnly bool
		alter        func(*syncPrincipalRoleCatalog)
	}{
		{"rolsuper", false, func(r *syncPrincipalRoleCatalog) { r.superuser = true }},
		{"rolcreaterole", false, func(r *syncPrincipalRoleCatalog) { r.createRole = true }},
		{"rolcreatedb", false, func(r *syncPrincipalRoleCatalog) { r.createDB = true }},
		{"rolbypassrls", false, func(r *syncPrincipalRoleCatalog) { r.bypassRLS = true }},
		{"rolinherit", false, func(r *syncPrincipalRoleCatalog) { r.inherit = true }},
		{"rolreplication", false, func(r *syncPrincipalRoleCatalog) { r.replication = true }},
		{"owner_membership", true, func(r *syncPrincipalRoleCatalog) { r.membership = true }},
		{"session_replication_role", true, func(r *syncPrincipalRoleCatalog) { r.replicationParameter = true }},
		{"nologin", true, func(r *syncPrincipalRoleCatalog) { r.canLogin = false }},
	}
	for index, name := range syncPrincipalRoleNames() {
		for _, attribute := range attributes {
			if index == 0 && attribute.nonownerOnly {
				continue
			}
			t.Run(name+"/"+attribute.name, func(t *testing.T) {
				reader := newPrincipalCatalogReader()
				baseline := newPrincipalCatalogReader()
				role := reader.roles[name]
				before := role
				attribute.alter(&role)
				reader.roles[name] = role
				// Exactly one catalog boolean on exactly one role differs. In particular,
				// SUPERUSER cases do not also gain parameter privilege and mask its check.
				changed := 0
				prior, after := reflect.ValueOf(before), reflect.ValueOf(role)
				for field := range prior.NumField() {
					if prior.Field(field).Kind() == reflect.Bool && prior.Field(field).Bool() != after.Field(field).Bool() {
						changed++
					}
				}
				if changed != 1 || role.name != before.name {
					t.Fatal("principal attribute fixture is not isolated")
				}
				for other, expected := range baseline.roles {
					if other != name && reader.roles[other] != expected {
						t.Fatal("principal attribute fixture changed another role")
					}
				}
				if verifySyncPrincipals(context.Background(), reader, syncPrincipalRoleNames()) != ErrSyncPrincipalsMissing {
					t.Fatal("principal preflight accepted isolated forbidden role attribute")
				}
				if reader.calls != index+1 {
					t.Fatal("principal preflight did not stop at isolated unsafe identity")
				}
			})
		}
	}
}

func TestSyncPrincipalTopologyCatalogFailuresRefuse(t *testing.T) {
	ctx := context.Background()
	st := &Store{}
	reader := newPrincipalCatalogReader()
	if st.verifySyncPrincipalTopology(ctx, reader) != nil || reader.calls != 9 {
		t.Fatal("principal topology valid catalog did not reach every predicate")
	}
	for i, name := range syncPrincipalRoleNames() {
		if reader.args[i][0] != name {
			t.Fatal("principal topology lost reviewed role inventory")
		}
	}
	if !slices.Equal(reader.args[7][2].([]string), syncPrincipalRoleNames()) ||
		!slices.Equal(reader.args[7][0].([]string), SyncWriterInventory()) {
		t.Fatal("principal topology catalog lost complete inventory fence")
	}
	for _, tc := range []struct {
		name      string
		predicate syncPrincipalTopologyPredicate
		query     string
		calls     int
	}{
		{"roles", syncPrincipalTopologyRoles, syncPrincipalRoleCatalogSQL, 1},
		{"owner_identity", syncPrincipalTopologyOwnerIdentity, syncPrincipalOwnerIdentitySQL, 7},
		{"function_acl", syncPrincipalTopologyFunctionACL, syncPrincipalFunctionACLSQL, 8},
		{"owner_drain", syncPrincipalTopologyOwnerDrain, syncPrincipalOwnerDrainSQL, 9},
	} {
		for _, unavailable := range []bool{false, true} {
			name := tc.name + "/refusal"
			if unavailable {
				name = tc.name + "/error"
			}
			t.Run(name, func(t *testing.T) {
				reader := newPrincipalCatalogReader()
				if unavailable {
					reader.failQuery = tc.query
				} else if tc.predicate == syncPrincipalTopologyRoles {
					delete(reader.roles, string(SyncPrincipalOwner))
				} else {
					reader.topology[tc.predicate] = false
				}
				if st.verifySyncPrincipalTopology(ctx, reader) != ErrSyncPrincipalTopologyUnverified || reader.calls != tc.calls {
					t.Fatal("principal topology catalog crossed refusal boundary")
				}
			})
		}
	}
}

func TestSyncPrincipalTopologySinglePredicateBypass(t *testing.T) {
	for _, bypass := range syncPrincipalTopologyPredicates {
		for _, failing := range syncPrincipalTopologyPredicates {
			reader := newPrincipalCatalogReader()
			if failing == syncPrincipalTopologyRoles {
				delete(reader.roles, string(SyncPrincipalOwner))
			} else {
				reader.topology[failing] = false
			}
			st := &Store{syncPrincipalTest: &syncPrincipalTestOptions{topologyBypass: bypass}}
			err := st.verifySyncPrincipalTopology(context.Background(), reader)
			if bypass == failing && err != nil || bypass != failing && err != ErrSyncPrincipalTopologyUnverified {
				t.Fatal("principal topology selector bypassed another predicate")
			}
		}
	}
	reader := newPrincipalCatalogReader()
	if (&Store{}).verifySyncPrincipalTopologyPredicate(context.Background(), reader, syncPrincipalTopologyNone) != ErrSyncPrincipalTopologyUnverified || reader.calls != 0 {
		t.Fatal("principal topology accepted an unknown predicate")
	}
}

func TestSyncPrincipalTopologyOwnerBypassIsExplicitTestSeam(t *testing.T) {
	reader := &principalCatalogReader{}
	st := &Store{syncPrincipalTest: &syncPrincipalTestOptions{singleLogin: true}}
	if st.verifySyncPrincipalTopology(context.Background(), reader) != nil || reader.calls != 0 {
		t.Fatal("principal topology explicit owner fixture bypass failed")
	}
	st.syncPrincipalTest.singleLogin = false
	if st.verifySyncPrincipalTopology(context.Background(), reader) != ErrSyncPrincipalTopologyUnverified || reader.calls != 1 {
		t.Fatal("principal topology disabled owner fixture bypass survived")
	}
}

func TestSyncPrincipalMalformedRoleListRefusesBeforeCatalog(t *testing.T) {
	duplicate := syncPrincipalRoleNames()
	duplicate[5] = duplicate[0]
	empty := syncPrincipalRoleNames()
	empty[5] = ""
	for _, roles := range [][]string{nil, {}, {"fixture"}, append(syncPrincipalRoleNames(), "extra"), duplicate, empty} {
		reader := &principalCatalogReader{}
		if verifySyncPrincipals(context.Background(), reader, roles) != ErrSyncPrincipalsMissing || reader.calls != 0 {
			t.Fatal("principal preflight accepted malformed role inventory")
		}
	}
}

func TestSyncPrincipalRoleNamesAreFresh(t *testing.T) {
	roles := SyncPrincipalRoleNames()
	roles[0] = "unreviewed"
	if !slices.Equal(SyncPrincipalRoleNames(), syncPrincipalRoleNames()) {
		t.Fatal("principal reviewed inventory accessor exposed mutable storage")
	}
}
