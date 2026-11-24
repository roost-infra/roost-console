package billing

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type RelayPreferences struct {
	CompressionEnabled *bool `json:"compression_enabled"`
	CompressionPercent *int  `json:"compression_percent"`
	MemoryEnabled      *bool `json:"memory_enabled"`
}

type RelayPreferenceSet struct {
	Personal     RelayPreferences `json:"personal"`
	Organization RelayPreferences `json:"organization"`
	Effective    RelayPreferences `json:"effective"`
	UserRevision int64            `json:"user_revision"`
	OrgRevision  int64            `json:"org_revision"`
}

type relayPreferenceQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) RelayPreferences(ctx context.Context, org, user string) (RelayPreferenceSet, error) {
	return readRelayPreferences(ctx, s.Pool, org, user)
}

func readRelayPreferences(ctx context.Context, db relayPreferenceQueryer, org, user string) (RelayPreferenceSet, error) {
	var set RelayPreferenceSet
	p, o, e := &set.Personal, &set.Organization, &set.Effective
	err := db.QueryRow(ctx, `
		select p.compression_enabled,p.compression_percent,p.memory_enabled,
		       o.compression_enabled,o.compression_percent,o.memory_enabled,
		       coalesce(p.compression_enabled,o.compression_enabled),
		       coalesce(p.compression_percent,o.compression_percent),
		       coalesce(p.memory_enabled,o.memory_enabled),
		       coalesce(p.revision,0),coalesce(o.revision,0)
		from public.organizations g
		left join billing.relay_preferences o on o.org_id=g.id and o.principal=''
		left join billing.relay_preferences p on p.org_id=g.id and p.principal=nullif($2,'')
		where g.id=$1 and ($2='' or exists (select 1 from public.organization_members m
			where m.org_id=g.id and m.user_id::text=$2))`, org, user).Scan(
		&p.CompressionEnabled, &p.CompressionPercent, &p.MemoryEnabled,
		&o.CompressionEnabled, &o.CompressionPercent, &o.MemoryEnabled,
		&e.CompressionEnabled, &e.CompressionPercent, &e.MemoryEnabled,
		&set.UserRevision, &set.OrgRevision)
	return set, err
}

func (s *Store) SaveRelayPreferences(ctx context.Context, org, principal string, value RelayPreferences) error {
	_, err := s.Pool.Exec(ctx, `insert into billing.relay_preferences
		(org_id,principal,compression_enabled,compression_percent,memory_enabled,revision)
		values ($1,$2,$3,$4,$5,1)
		on conflict (org_id,principal) do update set compression_enabled=excluded.compression_enabled,
		compression_percent=excluded.compression_percent,memory_enabled=excluded.memory_enabled,
		revision=relay_preferences.revision+1`, org, principal, value.CompressionEnabled, value.CompressionPercent, value.MemoryEnabled)
	return err
}

func lockRelayPreference(ctx context.Context, tx pgx.Tx, org, principal string) error {
	if _, err := tx.Exec(ctx, `insert into billing.relay_preferences (org_id,principal)
		values ($1,$2) on conflict do nothing`, org, principal); err != nil {
		return err
	}
	var revision int64
	return tx.QueryRow(ctx, `select revision from billing.relay_preferences
		where org_id=$1 and principal=$2 for update`, org, principal).Scan(&revision)
}
