package billing

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRelayPersonalMemoryDB(t *testing.T) {
	dsn := os.Getenv("ROOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ROOST_TEST_DATABASE_URL to run the memory database test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := New(pool)
	org, user, peer := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`insert into auth.users (id,email) values ($1,$3),($2,$4)`, user, peer, user+"@example.test", peer+"@example.test")
	defer pool.Exec(ctx, `delete from auth.users where id in ($1,$2)`, user, peer)
	exec(`insert into public.organizations(id,name,slug,owner_user_id) values ($1,'memory test',$2,$3)`, org, org, user)
	exec(`insert into public.organization_members(org_id,user_id,role) values ($1,$2,'owner'),($1,$3,'member')`, org, user, peer)

	on, off, target, zero := true, false, 30, 0
	read := func(principal string) RelayPreferenceSet {
		t.Helper()
		prefs, err := store.RelayPreferences(ctx, org, principal)
		if err != nil {
			t.Fatal(err)
		}
		return prefs
	}
	save := func(principal string, prefs RelayPreferences) {
		t.Helper()
		if err = store.SaveRelayPreferences(ctx, org, principal, prefs); err != nil {
			t.Fatal(err)
		}
	}
	save("", RelayPreferences{CompressionEnabled: &on, CompressionPercent: &target, MemoryEnabled: &on})
	save(user, RelayPreferences{CompressionEnabled: &off, CompressionPercent: &zero})
	own := read(user)
	if *own.Effective.CompressionEnabled || *own.Effective.CompressionPercent != 0 || !*own.Effective.MemoryEnabled {
		t.Fatalf("explicit false/0 or inheritance lost: %+v", own)
	}
	if other := read(peer); !*other.Effective.CompressionEnabled || *other.Effective.CompressionPercent != 30 {
		t.Fatalf("user override leaked to peer: %+v", other)
	}

	item, err := store.SaveRelayMemory(ctx, org, user, RelayMemory{Body: "Preferred brokerage: 彩虹证券"})
	if err != nil {
		t.Fatal(err)
	}
	call := RelayMemoryCall{OrgID: org, UserID: user, RequestID: "request-1", UserRevision: own.UserRevision, OrgRevision: own.OrgRevision}
	run := func(action, body string) []RelayMemory {
		t.Helper()
		call.Action, call.Text = action, body
		items, err := store.RunRelayMemory(ctx, call)
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	run("start", "")
	if got := run("search", "我用哪家证券公司？"); len(got) != 1 || got[0].ID != item.ID {
		t.Fatalf("search = %+v", got)
	}
	if got, err := store.RelayMemories(ctx, org, peer, "", 0); err != nil || len(got) != 0 {
		t.Fatalf("peer list = %+v; err = %v", got, err)
	}
	if _, err := store.SaveRelayMemory(ctx, org, peer, item); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("peer edit = %v", err)
	}
	item.Body = "Updated brokerage: 蓝天证券"
	edited, err := store.SaveRelayMemory(ctx, org, user, item)
	if err != nil || edited.Version != item.Version+1 {
		t.Fatalf("update = %+v; err = %v", edited, err)
	}
	if _, err = store.SaveRelayMemory(ctx, org, user, item); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale edit = %v", err)
	}
	writes := run("write", "new model memory")
	if len(writes) != 1 || len(run("write", "duplicate")) != 0 {
		t.Fatalf("write was not idempotent: %+v", writes)
	}
	events, err := store.RelayMemoryEvents(ctx, org, user, 0)
	if err != nil || len(events) != 3 || events[0].Phase != "write" ||
		events[1].Phase != "match" || events[1].MemoryIDs[0] != item.ID || events[2].Phase != "search" {
		t.Fatalf("events = %+v; err = %v", events, err)
	}
	if got, err := store.RelayMemoryEvents(ctx, org, peer, 0); err != nil || len(got) != 0 {
		t.Fatalf("peer events = %+v; err = %v", got, err)
	}
	if deleted, err := store.DeleteRelayMemories(ctx, org, user, []string{item.ID}, false); err != nil || deleted != 1 {
		t.Fatalf("single delete = %d; err = %v", deleted, err)
	}
	if deleted, err := store.DeleteRelayMemories(ctx, org, user, nil, true); err != nil || deleted != 1 {
		t.Fatalf("clear = %d; err = %v", deleted, err)
	}
	call.RequestID = "stale-async-write"
	if _, err = store.RunRelayMemory(ctx, call); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale write after clear = %v", err)
	}
	call.UserRevision = read(user).UserRevision
	if result := run("search", "new model memory"); len(result) != 0 {
		t.Fatalf("deleted memory still recalled: %+v", result)
	}
	save(user, RelayPreferences{MemoryEnabled: &off})
	call.UserRevision = read(user).UserRevision
	if _, err = store.RunRelayMemory(ctx, call); !errors.Is(err, ErrNotFound) {
		t.Fatalf("memory call when switched off = %v", err)
	}
}
