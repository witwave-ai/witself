package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

type managedPrincipalRow func(...any) error

func (r managedPrincipalRow) Scan(dest ...any) error { return r(dest...) }

type managedPrincipalCatalog struct {
	*principalCatalogReader
	current, session string
	membership       bool
}

func newManagedPrincipalCatalog(role SyncPrincipalRole) *managedPrincipalCatalog {
	r := &managedPrincipalCatalog{principalCatalogReader: newPrincipalCatalogReader(), current: string(role), session: string(role), membership: true}
	r.roles[string(SyncPrincipalMigrator)] = syncPrincipalRoleCatalog{name: string(SyncPrincipalMigrator), canLogin: true, membership: true}
	return r
}

func (r *managedPrincipalCatalog) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	switch query {
	case managedPrincipalIdentitySQL:
		return managedPrincipalRow(func(dest ...any) error {
			if len(dest) != 2 {
				return errors.New("managed fixture identity destination mismatch")
			}
			*dest[0].(*string), *dest[1].(*string) = r.current, r.session
			return nil
		})
	case managedPrincipalOwnerLoginSQL:
		return principalCatalogRow{valid: r.roles[string(SyncPrincipalOwner)].canLogin}
	case managedPrincipalMigratorMembershipSQL:
		return principalCatalogRow{valid: r.membership}
	default:
		return r.principalCatalogReader.QueryRow(ctx, query, args...)
	}
}

func TestManagedPrincipalsRuntimePreflight(t *testing.T) {
	ctx := context.Background()
	for _, runtime := range []SyncPrincipalRole{SyncPrincipalServer, SyncPrincipalWorker, SyncPrincipalMaintenance} {
		t.Run(string(runtime), func(t *testing.T) {
			if verifyManagedRuntimePrincipal(ctx, newManagedPrincipalCatalog(runtime), runtime) != nil {
				t.Fatal("managed runtime rejected valid restricted identity")
			}
			for _, field := range []string{"current", "session"} {
				for _, wrong := range []string{string(SyncPrincipalOwner), string(SyncPrincipalMigrator), "localcell", "unexpected"} {
					r := newManagedPrincipalCatalog(runtime)
					if field == "current" {
						r.current = wrong
					} else {
						r.session = wrong
					}
					if verifyManagedRuntimePrincipal(ctx, r, runtime) == nil {
						t.Fatal("managed runtime accepted incorrect physical identity")
					}
				}
			}
			for _, role := range syncPrincipalRoleNames() {
				for _, predicate := range []string{"superuser", "create_role", "create_database", "bypass_rls", "inherit", "replication", "login", "membership", "replication_parameter"} {
					if role == string(SyncPrincipalOwner) && (predicate == "membership" || predicate == "replication_parameter") {
						continue
					}
					t.Run(role+"/"+predicate, func(t *testing.T) {
						r := newManagedPrincipalCatalog(runtime)
						row := r.roles[role]
						switch predicate {
						case "superuser":
							row.superuser = true
						case "create_role":
							row.createRole = true
						case "create_database":
							row.createDB = true
						case "bypass_rls":
							row.bypassRLS = true
						case "inherit":
							row.inherit = true
						case "replication":
							row.replication = true
						case "login":
							row.canLogin = !row.canLogin
						case "membership":
							row.membership = true
						case "replication_parameter":
							row.replicationParameter = true
						}
						r.roles[role] = row
						if verifyManagedRuntimePrincipal(ctx, r, runtime) == nil {
							t.Fatal("managed runtime accepted unsafe catalog predicate")
						}
					})
				}
			}
			for _, predicate := range []syncPrincipalTopologyPredicate{syncPrincipalTopologyOwnerIdentity, syncPrincipalTopologyFunctionACL} {
				r := newManagedPrincipalCatalog(runtime)
				r.topology[predicate] = false
				if verifyManagedRuntimePrincipal(ctx, r, runtime) == nil {
					t.Fatal("managed runtime accepted unsafe ownership or ACL")
				}
			}
		})
	}
	if verifyManagedRuntimePrincipal(ctx, newManagedPrincipalCatalog(SyncPrincipalOwner), SyncPrincipalOwner) == nil {
		t.Fatal("managed runtime accepted unsupported purpose")
	}
}

func TestManagedPrincipalsRuntimeIdentityBeforeSchema(t *testing.T) {
	ctx := context.Background()
	for _, role := range []SyncPrincipalRole{SyncPrincipalServer, SyncPrincipalWorker, SyncPrincipalMaintenance} {
		r := newManagedPrincipalCatalog(role)
		r.topology[syncPrincipalTopologyOwnerIdentity] = false
		r.topology[syncPrincipalTopologyFunctionACL] = false
		if verifyManagedRuntimeIdentity(ctx, r, role) != nil {
			t.Fatal("managed identity depended on installed schema ownership")
		}
		r.session = string(SyncPrincipalSnapshot)
		if verifyManagedRuntimeIdentity(ctx, r, role) != ErrSyncPrincipalTopologyUnverified {
			t.Fatal("managed identity accepted wrong session before schema")
		}
	}
}

func TestManagedPrincipalsMigrationPreflight(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{"", "current", "session", "owner_login", "membership", "superuser", "inherit", "replication_parameter"} {
		t.Run("predicate_"+bad, func(t *testing.T) {
			r := newManagedPrincipalCatalog(SyncPrincipalMigrator)
			r.current = string(SyncPrincipalOwner)
			row := r.roles[string(SyncPrincipalMigrator)]
			switch bad {
			case "current":
				r.current = string(SyncPrincipalMigrator)
			case "session":
				r.session = string(SyncPrincipalOwner)
			case "owner_login":
				owner := r.roles[string(SyncPrincipalOwner)]
				owner.canLogin = true
				r.roles[string(SyncPrincipalOwner)] = owner
			case "membership":
				r.membership = false
			case "superuser":
				row.superuser = true
			case "inherit":
				row.inherit = true
			case "replication_parameter":
				row.replicationParameter = true
			}
			r.roles[string(SyncPrincipalMigrator)] = row
			if (verifyManagedMigrationPrincipal(ctx, r) == nil) != (bad == "") {
				t.Fatal("managed migration custody predicate was not isolated")
			}
		})
	}
}
